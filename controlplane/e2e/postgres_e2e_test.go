// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
	"github.com/burrow-cloud/burrow/controlplane/kube"
	"github.com/burrow-cloud/burrow/controlplane/registry"
)

// TestPostgresAddonE2E drives the Postgres add-on through the real Kubernetes adapter and the real
// admin-SQL provisioner against a live cluster (ADR-0031): install the instance, attach an app
// (which provisions an isolated database + role and writes DATABASE_URL into the app's Secret),
// then run an in-cluster Job that connects with that DATABASE_URL and round-trips a row, and finally
// detach (dropping the database). Like the other e2es it runs only when BURROW_TEST_KUBECONFIG
// points at a disposable cluster; it creates its own namespaces and cleans them up. The round-trip
// runs inside the cluster because the add-on Service (burrow-postgres.<ns>.svc) is only reachable
// from in-cluster.
func TestPostgresAddonE2E(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the Postgres add-on end-to-end test")
	}
	ctx := context.Background()

	cfg, err := kube.ConfigFromKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	stamp := time.Now().UnixNano()
	appNS := fmt.Sprintf("burrow-pg-app-%d", stamp)
	addonNS := fmt.Sprintf("burrow-pg-addons-%d", stamp)
	for _, ns := range []string{appNS, addonNS} {
		if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
		ns := ns
		t.Cleanup(func() { _ = client.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })
	}

	k8s := kube.New(client, appNS).WithAddonNamespace(addonNS)
	prov := kube.NewPostgresProvisioner(client, addonNS)
	engine, err := cp.New(cp.Deps{
		Kubernetes:          k8s,
		Registry:            registry.New(),
		Database:            fake.NewDatabase(),
		Clock:               fake.NewClock(time.Now()),
		IDs:                 fake.NewIDs(),
		Resolver:            fake.NewResolver(),
		Credentials:         fake.NewCredentials(),
		DNS:                 fake.NewDNSFactory(),
		DatabaseProvisioner: prov,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	const app = "shop"

	// Install the Postgres instance and wait for it to become ready. confirm=true clears the
	// addon_install guardrail (the fake DB's default policy holds it for confirmation).
	if _, err := engine.InstallAddon(ctx, cp.AddonPostgres, true); err != nil {
		t.Fatalf("InstallAddon postgres: %v", err)
	}
	waitForCond(t, 180*time.Second, "postgres ready", func() (bool, error) {
		return k8s.AddonReady(ctx, "burrow-postgres")
	})

	// Attach the app: provisions the database/role and writes DATABASE_URL into the app's Secret.
	res, err := engine.AttachAddon(ctx, cp.AddonPostgres, app)
	if err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if res.SecretKey != "DATABASE_URL" {
		t.Fatalf("attach SecretKey = %q, want DATABASE_URL", res.SecretKey)
	}

	// Round-trip a row from inside the cluster using the app's DATABASE_URL (sourced from the
	// per-app Secret), proving the credential and the database both work.
	runRoundTripJob(t, ctx, client, appNS, app)

	// Detach: drops the database and role and removes the DATABASE_URL key.
	if err := engine.DetachAddon(ctx, cp.AddonPostgres, app, true); err != nil {
		t.Fatalf("DetachAddon: %v", err)
	}
	keys, err := k8s.SecretKeys(ctx, app)
	if err != nil {
		t.Fatalf("SecretKeys after detach: %v", err)
	}
	for _, k := range keys {
		if k == "DATABASE_URL" {
			t.Errorf("DATABASE_URL should be removed from the app's Secret after detach")
		}
	}
}

// runRoundTripJob runs a one-shot psql Job in the app namespace that reads DATABASE_URL from the
// app's per-app Secret (via envFrom), creates a table, inserts a row, and reads it back. The Job
// uses the official postgres image's psql client and must complete successfully.
func runRoundTripJob(t *testing.T, ctx context.Context, client kubernetes.Interface, appNS, app string) {
	t.Helper()
	script := `set -e
psql "$DATABASE_URL" -c "CREATE TABLE IF NOT EXISTS t (id int);"
psql "$DATABASE_URL" -c "INSERT INTO t VALUES (42);"
psql "$DATABASE_URL" -tAc "SELECT id FROM t WHERE id = 42;" | grep -q 42`
	var backoff int32
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-roundtrip", Namespace: appNS},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "psql",
						Image:   "postgres:17.4",
						Command: []string{"sh", "-c", script},
						// DATABASE_URL comes from the app's per-app Secret, exactly as the app reads it.
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cp.AppSecretName(app)},
							},
						}},
					}},
				},
			},
		},
	}
	if _, err := client.BatchV1().Jobs(appNS).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create round-trip job: %v", err)
	}
	waitForCond(t, 180*time.Second, "round-trip job succeeded", func() (bool, error) {
		j, err := client.BatchV1().Jobs(appNS).Get(ctx, "pg-roundtrip", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if j.Status.Failed > 0 {
			return false, fmt.Errorf("round-trip job failed")
		}
		return j.Status.Succeeded > 0, nil
	})
}

// waitForCond polls cond until it is true, erroring on a hard error or timeout.
func waitForCond(t *testing.T, timeout time.Duration, desc string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("waiting for %s: %v", desc, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, desc)
		}
		time.Sleep(2 * time.Second)
	}
}
