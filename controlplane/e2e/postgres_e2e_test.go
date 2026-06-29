// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

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

	// This test process runs OUT of the cluster, so it cannot resolve the instance's in-cluster
	// Service DNS name (burrow-postgres.<ns>.svc) that burrowd uses in production. For each admin
	// operation, port-forward the Postgres pod to a local port and point the provisioner's ADMIN
	// connection at it; the app's DATABASE_URL still gets the in-cluster Service name, which the
	// round-trip Job (a pod) resolves. A fresh forward per operation keeps the test robust against
	// a single forward dropping mid-run.
	pgSelector := "burrow.cloud/addon=postgres"

	// Attach the app: provisions the database/role and writes DATABASE_URL into the app's Secret.
	var res cp.AttachResult
	withPortForward(t, cfg, client, addonNS, pgSelector, 5432, "attach addon", func(localPort int) error {
		prov.WithAdminEndpoint(fmt.Sprintf("127.0.0.1:%d", localPort))
		var aerr error
		res, aerr = engine.AttachAddon(ctx, cp.AddonPostgres, app)
		return aerr
	})
	if res.SecretKey != "DATABASE_URL" {
		t.Fatalf("attach SecretKey = %q, want DATABASE_URL", res.SecretKey)
	}

	// Round-trip a row from inside the cluster using the app's DATABASE_URL (sourced from the
	// per-app Secret), proving the credential and the database both work.
	runRoundTripJob(t, ctx, client, appNS, app)

	// Detach: drops the database and role and removes the DATABASE_URL key (also an admin
	// operation, so it runs through a fresh port-forward).
	withPortForward(t, cfg, client, addonNS, pgSelector, 5432, "detach addon", func(localPort int) error {
		prov.WithAdminEndpoint(fmt.Sprintf("127.0.0.1:%d", localPort))
		return engine.DetachAddon(ctx, cp.AddonPostgres, app, true)
	})
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

// TestPostgresBackupRestoreE2E drives on-demand backup and restore through the real Kubernetes
// adapter against a live cluster (ADR-0032): install the instance, attach an app, seed a row,
// BackupAddon (an in-cluster pg_dump Job), drop the row with an in-cluster Job, then RestoreAddon
// (an in-cluster pg_restore Job) and assert the row is back. The backup/restore Jobs run in-cluster,
// so — unlike attach — the engine calls need no port-forward: they create Jobs that reach the
// instance Service directly. It runs only when BURROW_TEST_KUBECONFIG points at a disposable
// cluster; it creates its own namespaces and cleans them up.
func TestPostgresBackupRestoreE2E(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the Postgres backup/restore end-to-end test")
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
	appNS := fmt.Sprintf("burrow-pgbak-app-%d", stamp)
	addonNS := fmt.Sprintf("burrow-pgbak-addons-%d", stamp)
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

	if _, err := engine.InstallAddon(ctx, cp.AddonPostgres, true); err != nil {
		t.Fatalf("InstallAddon postgres: %v", err)
	}
	waitForCond(t, 180*time.Second, "postgres ready", func() (bool, error) {
		return k8s.AddonReady(ctx, "burrow-postgres")
	})

	pgSelector := "burrow.cloud/addon=postgres"

	// Attach the app (an admin-SQL op, so it goes through a port-forward like the other e2e).
	withPortForward(t, cfg, client, addonNS, pgSelector, 5432, "attach addon", func(localPort int) error {
		prov.WithAdminEndpoint(fmt.Sprintf("127.0.0.1:%d", localPort))
		_, aerr := engine.AttachAddon(ctx, cp.AddonPostgres, app)
		return aerr
	})

	// Seed a known row from inside the cluster using the app's DATABASE_URL.
	runSQLJob(t, ctx, client, appNS, app, "seed",
		`psql "$DATABASE_URL" -c "CREATE TABLE IF NOT EXISTS t (id int);"
psql "$DATABASE_URL" -c "INSERT INTO t VALUES (7);"`)

	// Back up: burrowd creates an in-cluster pg_dump Job — NO port-forward needed.
	res, err := engine.BackupAddon(ctx, cp.AddonPostgres, app)
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	if res.Backup.Status != cp.BackupCompleted {
		t.Fatalf("backup status = %q, want completed", res.Backup.Status)
	}

	// Drop the row in-cluster, proving restore actually puts it back.
	runSQLJob(t, ctx, client, appNS, app, "drop",
		`psql "$DATABASE_URL" -c "DELETE FROM t WHERE id = 7;"
test "$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM t WHERE id = 7;")" = "0"`)

	// Restore: burrowd creates an in-cluster pg_restore Job — again NO port-forward.
	if err := engine.RestoreAddon(ctx, cp.AddonPostgres, app, res.Backup.ID, true); err != nil {
		t.Fatalf("RestoreAddon: %v", err)
	}

	// Assert the row is back, in-cluster.
	runSQLJob(t, ctx, client, appNS, app, "assert",
		`psql "$DATABASE_URL" -tAc "SELECT id FROM t WHERE id = 7;" | grep -q 7`)
}

// runSQLJob runs a one-shot psql Job in the app namespace that reads DATABASE_URL from the app's
// per-app Secret (via envFrom) and executes script. name disambiguates the Job. The Job uses the
// official postgres image's psql client and must complete successfully.
func runSQLJob(t *testing.T, ctx context.Context, client kubernetes.Interface, appNS, app, name, script string) {
	t.Helper()
	var backoff int32
	jobName := "pg-" + name
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: appNS},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "psql",
						Image:   "postgres:17.4",
						Command: []string{"sh", "-c", "set -e\n" + script},
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
		t.Fatalf("create %s job: %v", jobName, err)
	}
	waitForCond(t, 180*time.Second, jobName+" job succeeded", func() (bool, error) {
		j, err := client.BatchV1().Jobs(appNS).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if j.Status.Failed > 0 {
			return false, fmt.Errorf("%s job failed", jobName)
		}
		return j.Status.Succeeded > 0, nil
	})
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

// withPortForward retries fn under a freshly-established port-forward each attempt, until fn
// returns nil or the timeout elapses. Re-establishing the forward per attempt keeps the test
// robust if a single forward drops; the wrapped admin operation is idempotent, so re-running it is
// safe. desc names the operation in the failure message.
func withPortForward(t *testing.T, cfg *rest.Config, client kubernetes.Interface, ns, labelSelector string, containerPort int, desc string, fn func(localPort int) error) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for {
		last = func() error {
			localPort, stop, err := openPortForward(cfg, client, ns, labelSelector, containerPort)
			if err != nil {
				return err
			}
			defer stop()
			return fn(localPort)
		}()
		if last == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 90s on %s: %v", desc, last)
		}
		time.Sleep(3 * time.Second)
	}
}

// openPortForward forwards a local ephemeral port to containerPort on the first pod matching
// labelSelector in ns, returning the chosen local port and a stop function. This lets the
// out-of-cluster test reach an in-cluster Service that only resolves inside the cluster — the same
// trick `kubectl port-forward` uses. It returns an error (rather than failing the test) so the
// caller can retry.
func openPortForward(cfg *rest.Config, client kubernetes.Interface, ns, labelSelector string, containerPort int) (int, func(), error) {
	pods, err := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return 0, nil, fmt.Errorf("list pods %q in %s: %w", labelSelector, ns, err)
	}
	if len(pods.Items) == 0 {
		return 0, nil, fmt.Errorf("no pod matching %q in %s to port-forward", labelSelector, ns)
	}
	pod := pods.Items[0].Name

	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return 0, nil, fmt.Errorf("spdy round tripper: %w", err)
	}
	reqURL := client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", containerPort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, fmt.Errorf("new port forward: %w", err)
	}
	go func() { _ = fw.ForwardPorts() }()
	select {
	case <-readyCh:
	case <-time.After(15 * time.Second):
		close(stopCh)
		return 0, nil, fmt.Errorf("port-forward to %s/%s not ready within 15s", ns, pod)
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopCh)
		return 0, nil, fmt.Errorf("get forwarded ports: %w", err)
	}
	return int(ports[0].Local), func() { close(stopCh) }, nil
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
