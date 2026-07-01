// SPDX-License-Identifier: Apache-2.0
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// TestAutoscaleIntegration exercises the real HPA CRUD path against a real cluster (ADR-0006): after
// an autoscale, a HorizontalPodAutoscaler exists targeting the app's Deployment with the expected
// band; after autoscale off, it is gone. Creating the HPA resource does not require metrics-server,
// so this runs on a bare k3d; it does not assert actual scaling behavior. Guarded by the same
// BURROW_TEST_KUBECONFIG as TestIntegration.
func TestAutoscaleIntegration(t *testing.T) {
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

	nsName := fmt.Sprintf("burrow-it-hpa-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
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

	// A HorizontalPodAutoscaler can be created before its target exists; deploy the workload anyway
	// so the target is real.
	spec := cp.WorkloadSpec{
		App: app, Kind: cp.WorkloadDeployment, Image: "busybox:1.36",
		Command:  []string{"sh", "-c", "sleep 3600"},
		Replicas: 1,
	}
	if err := a.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	if err := a.ApplyAutoscaler(ctx, app, cp.AutoscaleSpec{MinReplicas: 2, MaxReplicas: 6, CPUPercent: 75}); err != nil {
		t.Fatalf("ApplyAutoscaler: %v", err)
	}
	hpa, err := client.AutoscalingV2().HorizontalPodAutoscalers(nsName).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("HPA not found: %v", err)
	}
	if hpa.Spec.ScaleTargetRef.Kind != "Deployment" || hpa.Spec.ScaleTargetRef.Name != app {
		t.Errorf("scaleTargetRef = %+v, want Deployment %q", hpa.Spec.ScaleTargetRef, app)
	}
	if hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 6 {
		t.Errorf("band = (min %v, max %d), want (2, 6)", hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas)
	}

	// autoscale off removes the HPA.
	if err := a.DeleteAutoscaler(ctx, app); err != nil {
		t.Fatalf("DeleteAutoscaler: %v", err)
	}
	if _, err := client.AutoscalingV2().HorizontalPodAutoscalers(nsName).Get(ctx, app, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("HPA should be gone, get err = %v", err)
	}
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
