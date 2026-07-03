// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/burrow-cloud/burrow/controlplane"
)

// metricsAPIGroup is the API group metrics-server serves. Its presence in API-group discovery means
// metrics-server is installed, so an applied HorizontalPodAutoscaler can read the CPU/memory metrics
// it scales on. Discovery needs no RBAC (ADR-0034).
const metricsAPIGroup = "metrics.k8s.io"

// ApplyAutoscaler creates or updates an autoscaling/v2 HorizontalPodAutoscaler named after app,
// targeting app's Deployment, with the requested replica band and utilization targets (ADR-0006). It
// mirrors ApplyWorkload's create-or-update-under-conflict-retry: the HPA controller continuously
// writes the object's status, so a get-then-update can lose the resourceVersion race and 409; we
// re-read and retry on conflict.
func (a *Adapter) ApplyAutoscaler(ctx context.Context, app string, spec controlplane.AutoscaleSpec) error {
	hpas := a.client.AutoscalingV2().HorizontalPodAutoscalers(a.namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := hpas.Get(ctx, app, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err := hpas.Create(ctx, a.buildAutoscaler(app, spec), metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		desired := a.buildAutoscaler(app, spec)
		desired.ResourceVersion = existing.ResourceVersion
		_, err = hpas.Update(ctx, desired, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("kube: applying autoscaler %q: %w", app, err)
	}
	return nil
}

// DeleteAutoscaler removes app's HorizontalPodAutoscaler. A missing HPA is a no-op, not an error, so
// turning autoscaling off is idempotent.
func (a *Adapter) DeleteAutoscaler(ctx context.Context, app string) error {
	err := a.client.AutoscalingV2().HorizontalPodAutoscalers(a.namespace).Delete(ctx, app, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kube: deleting autoscaler %q: %w", app, err)
	}
	return nil
}

// AutoscalerActive reports whether app has an active HorizontalPodAutoscaler owning its replica
// count. It gets the autoscaling/v2 HPA named after app: present means active, NotFound means
// inactive (false, nil, not an error). A workload apply consults it so it leaves the HPA-managed
// count untouched.
func (a *Adapter) AutoscalerActive(ctx context.Context, app string) (bool, error) {
	_, err := a.client.AutoscalingV2().HorizontalPodAutoscalers(a.namespace).Get(ctx, app, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kube: reading autoscaler %q: %w", app, err)
	}
	return true, nil
}

// MetricsAPIAvailable reports whether the metrics.k8s.io API group is served, i.e. metrics-server is
// installed. It reads API-group discovery, which needs no RBAC (ADR-0034). A discovery error is
// returned so the caller can decide; the engine treats it as "absent" and warns rather than failing,
// so a probe hiccup never blocks applying an HPA.
func (a *Adapter) MetricsAPIAvailable(ctx context.Context) (bool, error) {
	groups, err := a.client.Discovery().ServerGroups()
	if err != nil {
		return false, fmt.Errorf("kube: discovering API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == metricsAPIGroup {
			return true, nil
		}
	}
	return false, nil
}

// buildAutoscaler renders the desired HorizontalPodAutoscaler for app: it targets the app's
// Deployment (apps/v1), moves within the requested replica band, and holds a target average CPU
// utilization, plus memory when a target is set. It carries the Burrow managed-by label like every
// other object the adapter applies.
func (a *Adapter) buildAutoscaler(app string, spec controlplane.AutoscaleSpec) *autoscalingv2.HorizontalPodAutoscaler {
	labels := map[string]string{nameLabel: app, managedByLabel: managedByValue}
	minReplicas := spec.MinReplicas
	metrics := []autoscalingv2.MetricSpec{resourceMetric(corev1.ResourceCPU, spec.CPUPercent)}
	if spec.MemoryPercent > 0 {
		metrics = append(metrics, resourceMetric(corev1.ResourceMemory, spec.MemoryPercent))
	}
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: a.namespace, Labels: labels},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       app,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: spec.MaxReplicas,
			Metrics:     metrics,
		},
	}
}

// resourceMetric is a Resource metric targeting an average utilization percentage of the named
// resource (CPU or memory), the shape an HPA uses to scale on pod resource usage.
func resourceMetric(name corev1.ResourceName, percent int32) autoscalingv2.MetricSpec {
	target := percent
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: name,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: &target,
			},
		},
	}
}
