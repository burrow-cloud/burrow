// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestApplyAutoscalerBuildsHPA asserts the generated HorizontalPodAutoscaler targets the app's
// Deployment (apps/v1), carries the requested band and CPU (and memory) metrics, and the managed-by
// label.
func TestApplyAutoscalerBuildsHPA(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	spec := cp.AutoscaleSpec{MinReplicas: 2, MaxReplicas: 9, CPUPercent: 75, MemoryPercent: 60}
	if err := a.ApplyAutoscaler(ctx, "web", spec); err != nil {
		t.Fatalf("ApplyAutoscaler: %v", err)
	}

	hpa, err := client.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("HPA not found: %v", err)
	}
	ref := hpa.Spec.ScaleTargetRef
	if ref.APIVersion != "apps/v1" || ref.Kind != "Deployment" || ref.Name != "web" {
		t.Errorf("scaleTargetRef = %+v, want apps/v1 Deployment web", ref)
	}
	if hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 9 {
		t.Errorf("replica band = (min %v, max %d), want (2, 9)", hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas)
	}
	if hpa.Labels["app.kubernetes.io/managed-by"] != "burrow" {
		t.Errorf("managed-by label = %q, want burrow", hpa.Labels["app.kubernetes.io/managed-by"])
	}

	cpu := metricFor(hpa.Spec.Metrics, corev1.ResourceCPU)
	if cpu == nil || cpu.Target.AverageUtilization == nil || *cpu.Target.AverageUtilization != 75 {
		t.Errorf("cpu metric = %+v, want 75%% utilization", cpu)
	}
	if cpu != nil && cpu.Target.Type != autoscalingv2.UtilizationMetricType {
		t.Errorf("cpu target type = %q, want Utilization", cpu.Target.Type)
	}
	mem := metricFor(hpa.Spec.Metrics, corev1.ResourceMemory)
	if mem == nil || mem.Target.AverageUtilization == nil || *mem.Target.AverageUtilization != 60 {
		t.Errorf("memory metric = %+v, want 60%% utilization", mem)
	}
}

// TestApplyAutoscalerNoMemoryMetric confirms a zero memory target adds only the CPU metric.
func TestApplyAutoscalerNoMemoryMetric(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyAutoscaler(ctx, "web", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}); err != nil {
		t.Fatalf("ApplyAutoscaler: %v", err)
	}
	hpa, _ := client.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, "web", metav1.GetOptions{})
	if len(hpa.Spec.Metrics) != 1 {
		t.Fatalf("metrics = %d, want only CPU", len(hpa.Spec.Metrics))
	}
	if metricFor(hpa.Spec.Metrics, corev1.ResourceMemory) != nil {
		t.Errorf("unexpected memory metric when memory target is 0")
	}
}

// TestApplyAutoscalerUpdates confirms a second apply updates the existing HPA in place.
func TestApplyAutoscalerUpdates(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyAutoscaler(ctx, "web", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := a.ApplyAutoscaler(ctx, "web", cp.AutoscaleSpec{MinReplicas: 2, MaxReplicas: 10, CPUPercent: 60}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	hpa, _ := client.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, "web", metav1.GetOptions{})
	if *hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 10 {
		t.Errorf("band after update = (min %d, max %d), want (2, 10)", *hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas)
	}
}

// TestDeleteAutoscalerIdempotent removes the HPA and treats a missing HPA as a no-op.
func TestDeleteAutoscalerIdempotent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyAutoscaler(ctx, "web", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}); err != nil {
		t.Fatalf("ApplyAutoscaler: %v", err)
	}
	if err := a.DeleteAutoscaler(ctx, "web"); err != nil {
		t.Fatalf("DeleteAutoscaler: %v", err)
	}
	if _, err := client.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("HPA should be gone, get err = %v", err)
	}
	// Deleting an absent HPA is a no-op.
	if err := a.DeleteAutoscaler(ctx, "web"); err != nil {
		t.Errorf("DeleteAutoscaler (idempotent) = %v, want nil", err)
	}
}

// TestMetricsAPIAvailableAbsent confirms a cluster with no metrics.k8s.io group reports metrics
// absent (the fake clientset serves no such group).
func TestMetricsAPIAvailableAbsent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	available, err := a.MetricsAPIAvailable(ctx)
	if err != nil {
		t.Fatalf("MetricsAPIAvailable: %v", err)
	}
	if available {
		t.Errorf("metrics reported available on a cluster without metrics-server")
	}
}

// TestAutoscalerActive reports true only while an HPA named after the app exists: absent before an
// apply, present after, and absent again after a delete (NotFound → false, no error).
func TestAutoscalerActive(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if active, err := a.AutoscalerActive(ctx, "web"); err != nil || active {
		t.Fatalf("AutoscalerActive before apply = (%v, %v), want (false, nil)", active, err)
	}
	if err := a.ApplyAutoscaler(ctx, "web", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}); err != nil {
		t.Fatalf("ApplyAutoscaler: %v", err)
	}
	if active, err := a.AutoscalerActive(ctx, "web"); err != nil || !active {
		t.Fatalf("AutoscalerActive after apply = (%v, %v), want (true, nil)", active, err)
	}
	if err := a.DeleteAutoscaler(ctx, "web"); err != nil {
		t.Fatalf("DeleteAutoscaler: %v", err)
	}
	if active, err := a.AutoscalerActive(ctx, "web"); err != nil || active {
		t.Fatalf("AutoscalerActive after delete = (%v, %v), want (false, nil)", active, err)
	}
}

// metricFor returns the Resource metric spec for the named resource, or nil when absent.
func metricFor(metrics []autoscalingv2.MetricSpec, name corev1.ResourceName) *autoscalingv2.ResourceMetricSource {
	for i := range metrics {
		if metrics[i].Resource != nil && metrics[i].Resource.Name == name {
			return metrics[i].Resource
		}
	}
	return nil
}
