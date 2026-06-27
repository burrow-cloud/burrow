// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestIntegration exercises the real adapter against a real cluster (ADR-0010). It runs
// only when BURROW_TEST_KUBECONFIG points at a disposable cluster — a dedicated variable,
// not the ambient KUBECONFIG, so it never touches a developer's real cluster by accident.
// It creates its own namespace and cleans it up.
func TestIntegration(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the Kubernetes integration test")
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

	nsName := fmt.Sprintf("burrow-it-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	// A freshly created k3d cluster can EOF the first API call before its load-balancer is
	// forwarding; retry briefly so a cold start doesn't fail the suite (CI gates on this too,
	// but the local `task test:k3d` path has no such gate).
	var createErr error
	for attempt := 0; attempt < 10; attempt++ {
		if _, createErr = client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); createErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if createErr != nil {
		t.Fatalf("create namespace after retries: %v", createErr)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})
	})

	a := kube.New(client, nsName)
	const app = "web"

	// Deploy a workload that logs a line and stays running.
	spec := cp.WorkloadSpec{
		App: app, Kind: cp.WorkloadDeployment, Image: "busybox:1.36",
		Command:  []string{"sh", "-c", "echo hello-from-burrow; sleep 3600"},
		Replicas: 1,
	}
	if err := a.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	waitFor(t, 120*time.Second, "deployment available", func() (bool, error) {
		st, err := a.WorkloadStatus(ctx, app)
		if err != nil {
			return false, err
		}
		return st.Available, nil
	})

	waitFor(t, 60*time.Second, "log line", func() (bool, error) {
		lines, err := a.Logs(ctx, app, cp.LogOptions{})
		if err != nil {
			return false, nil // pod may still be starting; keep waiting
		}
		for _, l := range lines {
			if strings.Contains(l.Message, "hello-from-burrow") {
				return true, nil
			}
		}
		return false, nil
	})

	// Scale up and observe the new replicas become ready.
	if err := a.ScaleWorkload(ctx, app, 2); err != nil {
		t.Fatalf("ScaleWorkload: %v", err)
	}
	waitFor(t, 120*time.Second, "2 ready replicas", func() (bool, error) {
		st, err := a.WorkloadStatus(ctx, app)
		if err != nil {
			return false, err
		}
		return st.ReadyReplicas == 2, nil
	})

	// Delete and observe it disappear.
	if err := a.DeleteWorkload(ctx, app); err != nil {
		t.Fatalf("DeleteWorkload: %v", err)
	}
	waitFor(t, 60*time.Second, "deployment gone", func() (bool, error) {
		_, err := a.WorkloadStatus(ctx, app)
		return errors.Is(err, cp.ErrNotFound), nil
	})
}

func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() (bool, error)) {
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
