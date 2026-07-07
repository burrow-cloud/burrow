// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestEngineRunE2E drives the one-off command runner through the real Kubernetes adapter against a
// live cluster (ADR-0048): deploy an app, then run a command inside its OWN image as a short-lived
// Job. It asserts the app.run guardrail holds an unconfirmed run, a confirmed command that prints to
// stdout and exits 0 has its output and exit code captured, a command that exits non-zero surfaces as
// a structured result (not a transport error), and the Job carries the requested
// ttlSecondsAfterFinished. The deploy record uses the in-memory fake; this test is about the Job.
func TestEngineRunE2E(t *testing.T) {
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

	nsName := fmt.Sprintf("burrow-e2e-run-%d", time.Now().UnixNano())
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = client.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{}) })

	engine, err := cp.New(cp.Deps{
		Kubernetes:  kube.New(client, nsName),
		Database:    fake.NewDatabase(),
		Clock:       fake.NewClock(time.Now()),
		IDs:         fake.NewIDs(),
		Resolver:    fake.NewResolver(),
		Credentials: fake.NewCredentials(),
		DNS:         fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	const app = "web"
	// Deploy busybox as a long-running app so it has a current image to run a command in (ADR-0048 §2).
	if _, err := engine.Deploy(ctx, cp.DeployRequest{App: app, Image: "busybox:1.36", Command: []string{"sh", "-c", "sleep 3600"}, Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	waitForImage(t, ctx, engine, app, "busybox:1.36")

	// An unconfirmed run is held by the default app.run guardrail (confirm) — the command never runs.
	if _, err := engine.Run(ctx, cp.RunRequest{App: app, Command: []string{"echo", "hi"}}); err == nil {
		t.Fatal("run without confirm should be held by the app.run guardrail")
	} else if g, ok := cp.AsGuardrail(err); !ok || g.Code != cp.GuardrailAppRun || !g.NeedsConfirmation {
		t.Fatalf("run held err = %v, want an app.run confirmation hold", err)
	}

	// A confirmed run of a command that prints to stdout and exits 0: output and exit code captured.
	// (The Job's ttlSecondsAfterFinished — ADR-0048 §7 — is asserted precisely against the built Job
	// spec in the kube-adapter unit test; a post-hoc list here would race the TTL garbage collector.)
	res, err := engine.Run(ctx, cp.RunRequest{App: app, Command: []string{"sh", "-c", "echo hello-from-run; exit 0"}, Confirm: true})
	if err != nil {
		t.Fatalf("run exit 0: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello-from-run") {
		t.Errorf("stdout = %q, want to contain hello-from-run", res.Stdout)
	}

	// A confirmed run of a command that exits non-zero: captured as a structured result, not an error.
	res2, err := engine.Run(ctx, cp.RunRequest{App: app, Command: []string{"sh", "-c", "echo failing; exit 7"}, Confirm: true})
	if err != nil {
		t.Fatalf("run exit 7 returned a transport error, want a structured result: %v", err)
	}
	if res2.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", res2.ExitCode)
	}
}
