// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
	"github.com/burrow-cloud/burrow/controlplane/kube"
	"github.com/burrow-cloud/burrow/controlplane/registry"
)

// TestEngineDeployRollbackE2E drives the engine through the real registry resolver and
// the real Kubernetes adapter against a live cluster: deploy an image (the engine
// resolves its digest from the registry and applies it), watch it become available,
// deploy a second image, then roll back and confirm the prior image is running again.
// The deploy record uses the in-memory fake — Postgres is covered separately; this test
// is about the registry+cluster composition.
func TestEngineDeployRollbackE2E(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the end-to-end test")
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

	nsName := fmt.Sprintf("burrow-e2e-%d", time.Now().UnixNano())
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = client.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{}) })

	engine, err := cp.New(cp.Deps{
		Kubernetes: kube.New(client, nsName),
		Registry:   registry.New(),
		Database:   fake.NewDatabase(),
		Clock:      fake.NewClock(time.Now()),
		IDs:        fake.NewIDs(),
		Resolver:   fake.NewResolver(),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	const app = "web"
	keepRunning := []string{"sh", "-c", "sleep 3600"}

	// Deploy v1 — the engine resolves the digest from the registry and applies it.
	v1, err := engine.Deploy(ctx, cp.DeployRequest{App: app, Image: "busybox:1.36", Command: keepRunning, Replicas: 1})
	if err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	if v1.Release.Digest == "" {
		t.Errorf("v1 digest is empty — the registry resolver did not resolve it")
	}
	waitForImage(t, ctx, engine, app, "busybox:1.36")

	// Deploy v2.
	if _, err := engine.Deploy(ctx, cp.DeployRequest{App: app, Image: "busybox:1.37", Command: keepRunning, Replicas: 1}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	waitForImage(t, ctx, engine, app, "busybox:1.37")

	// Roll back — redeploys v1's reference (ADR-0007).
	rb, err := engine.Rollback(ctx, app)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rb.Release.Image != "busybox:1.36" {
		t.Fatalf("rollback image = %q, want busybox:1.36", rb.Release.Image)
	}
	waitForImage(t, ctx, engine, app, "busybox:1.36")
}

// waitForImage polls Status until the app is available and running the wanted image.
func waitForImage(t *testing.T, ctx context.Context, engine *cp.Engine, app, image string) {
	t.Helper()
	deadline := time.Now().Add(150 * time.Second)
	for {
		st, err := engine.Status(ctx, app)
		if err == nil && st.Running && st.Workload.Available && st.Workload.Image == image {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to run %q (last err: %v)", app, image, err)
		}
		time.Sleep(2 * time.Second)
	}
}
