// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
)

// TestControlPlaneDatabaseIntegration installs the control plane's database in its default shape —
// a CloudNativePG `Cluster` (ADR-0086 §1) — against a live cluster, and then opens it with the
// connection string the install wrote.
//
// It is the test for the claim nothing else can make: that the `Cluster` Burrow renders is one
// CloudNativePG accepts and bootstraps, and that what comes up is reachable at `postgres:5432` as
// the `burrow` role, which is what burrowd and its migrations do the moment they start. A rendered
// manifest that parses proves neither — the CRD prunes fields it does not describe, silently and
// with a 201.
//
// HEAVY: it needs BURROW_TEST_KUBECONFIG pointing at a disposable cluster with the CloudNativePG
// operator installed, which is what scripts/with-k3d.sh provides.
func TestControlPlaneDatabaseIntegration(t *testing.T) {
	kubeconfig := testKubeconfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	namespace := fmt.Sprintf("burrow-cnpg-%d", time.Now().UnixNano())
	const password = "pw-integration"

	manifests, err := renderManifests(installOptions{
		Namespace:      namespace,
		AppNamespace:   namespace + "-apps",
		AddonNamespace: namespace + "-addons",
		BuildNamespace: namespace + "-builds",
		Image:          "ghcr.io/burrow-cloud/burrowd:none",
		Token:          "tok-integration",
		DBPassword:     password,
		InstallID:      "id-integration",
		Port:           connect.DefaultPort,
		Database:       databaseCNPG,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}

	cfg, err := connect.RESTConfig(kubeconfig, "")
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	t.Cleanup(func() {
		for _, ns := range []string{namespace, namespace + "-apps", namespace + "-addons", namespace + "-builds"} {
			_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
		}
	})

	// The whole install manifest is applied, burrowd included. burrowd's image does not exist on this
	// cluster and its pod never starts, which is deliberate: this test is about the database coming up
	// underneath it, and applying the real bundle is what proves the `Cluster` lands beside everything
	// else rather than only on its own.
	if err := serverSideApply(ctx, kubeconfig, "", manifests, false, io.Discard, io.Discard); err != nil {
		t.Fatalf("applying the install manifests: %v", err)
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	ri := dyn.Resource(cnpgClusterGVR).Namespace(namespace)
	if err := waitForControlPlaneCluster(ctx, ri, namespace, io.Discard, clusterWait{
		grace: 3 * time.Minute, timeout: 12 * time.Minute, poll: 3 * time.Second,
	}); err != nil {
		t.Fatalf("waiting for the control plane database: %v", err)
	}

	// The managed Service the connection URL names. CloudNativePG's own services are
	// `postgres-rw`/`-ro`/`-r`, so this one existing is what makes the URL in the `burrowd-db` Secret
	// resolve at all.
	if _, err := cs.CoreV1().Services(namespace).Get(ctx, controlPlaneClusterName, metav1.GetOptions{}); err != nil {
		t.Fatalf("the managed Service %q the connection URL names is missing: %v", controlPlaneClusterName, err)
	}

	// Open the database with the exact URL the install wrote, from inside the cluster. This is the
	// assertion the rendered YAML cannot make: that the role, the database, the password and the
	// service name all agree.
	runPsqlCheck(ctx, t, cs, namespace)
}

// runPsqlCheck runs a one-shot Job that connects with the URL from the `burrowd-db` Secret and
// creates a table, which is what the embedded goose migrations do the first time burrowd starts. It
// is a create rather than a `SELECT 1` on purpose: the `burrow` role is the database's owner and not
// a superuser, and PostgreSQL 15 and later revoke CREATE on the public schema from everybody except
// the owner — so connecting proves the credential and writing proves the privilege.
func runPsqlCheck(ctx context.Context, t *testing.T, cs kubernetes.Interface, namespace string) {
	t.Helper()
	const name = "burrow-db-check"
	backoff := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "psql",
						Image:   "postgres:18",
						Command: []string{"sh", "-c", `psql "$URL" -v ON_ERROR_STOP=1 -c 'CREATE TABLE burrow_check (id int)'`},
						Env: []corev1.EnvVar{{
							Name: "URL",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "burrowd-db"},
								Key:                  "url",
							}},
						}},
					}},
				},
			},
		},
	}
	if _, err := cs.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the connection-check Job: %v", err)
	}

	deadline := time.Now().Add(5 * time.Minute)
	for {
		j, err := cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading the connection-check Job: %v", err)
		}
		if j.Status.Succeeded > 0 {
			return
		}
		if j.Status.Failed > 0 {
			t.Fatalf("the connection URL the install wrote did not open the database: the check Job failed")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the connection-check Job did not finish within 5m")
		}
		time.Sleep(3 * time.Second)
	}
}

// testKubeconfig returns the disposable cluster's kubeconfig, skipping when there is none. Every
// heavy test in this repository is opt-in the same way: it runs under scripts/with-k3d.sh and in CI,
// and is silently absent from a plain `go test ./...`.
func testKubeconfig(t *testing.T) string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv("BURROW_TEST_KUBECONFIG"))
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run this test")
	}
	return kubeconfig
}
