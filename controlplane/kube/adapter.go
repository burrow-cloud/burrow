// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package kube is the production controlplane.Kubernetes adapter, built on the official
// client-go SDK (ADR-0011). It translates the workload seam into Kubernetes Deployments
// and reads their status, scales, streams logs, and deletes them. It is a thin
// translation layer — no orchestration logic, which lives in the engine. v0.1 supports
// only WorkloadDeployment.
//
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the
// managed module can wire it; it is source-available under FSL-1.1-ALv2.
package kube

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Kubernetes = (*Adapter)(nil)

const (
	nameLabel      = "app.kubernetes.io/name"
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "burrow"
)

// Adapter operates Burrow workloads in a single Kubernetes namespace.
type Adapter struct {
	client    kubernetes.Interface
	namespace string
}

// New returns an Adapter over the given clientset and namespace (defaulting to
// "default"). Tests inject a fake clientset; production injects a real one
// (see NewFromConfig).
func New(client kubernetes.Interface, namespace string) *Adapter {
	if namespace == "" {
		namespace = "default"
	}
	return &Adapter{client: client, namespace: namespace}
}

func (a *Adapter) ApplyWorkload(ctx context.Context, spec controlplane.WorkloadSpec) error {
	if spec.Kind != "" && spec.Kind != controlplane.WorkloadDeployment {
		return fmt.Errorf("kube: workload kind %q is not supported in v0.1 (Deployment only): %w", spec.Kind, controlplane.ErrNotImplemented)
	}
	deployments := a.client.AppsV1().Deployments(a.namespace)

	// Create-or-update under conflict retry: the Deployment controller continuously
	// updates the live object (its status), so a get-then-update can lose the
	// resourceVersion race and 409. We re-read and retry on conflict. The closure
	// returns raw API errors so retry.RetryOnConflict can recognize a conflict.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := deployments.Get(ctx, spec.App, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err := deployments.Create(ctx, a.buildDeployment(spec), metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		desired := a.buildDeployment(spec)
		desired.ResourceVersion = existing.ResourceVersion
		_, err = deployments.Update(ctx, desired, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("kube: applying deployment %q: %w", spec.App, err)
	}
	return nil
}

func (a *Adapter) WorkloadStatus(ctx context.Context, app string) (controlplane.WorkloadStatus, error) {
	dep, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, app, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return controlplane.WorkloadStatus{}, fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	}
	if err != nil {
		return controlplane.WorkloadStatus{}, fmt.Errorf("kube: reading deployment %q: %w", app, err)
	}
	var desired int32
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	image := ""
	if c := dep.Spec.Template.Spec.Containers; len(c) > 0 {
		image = c[0].Image
	}
	return controlplane.WorkloadStatus{
		App:             app,
		Kind:            controlplane.WorkloadDeployment,
		Image:           image,
		DesiredReplicas: desired,
		ReadyReplicas:   dep.Status.ReadyReplicas,
		UpdatedReplicas: dep.Status.UpdatedReplicas,
		Available:       deploymentAvailable(dep, desired),
	}, nil
}

func (a *Adapter) ScaleWorkload(ctx context.Context, app string, replicas int32) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, err := a.client.AppsV1().Deployments(a.namespace).Patch(ctx, app, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("kube: scaling deployment %q: %w", app, err)
	}
	return nil
}

func (a *Adapter) Logs(ctx context.Context, app string, opts controlplane.LogOptions) ([]controlplane.LogLine, error) {
	// Confirm the workload exists so an unknown app is ErrNotFound, not empty logs.
	if _, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, app, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	} else if err != nil {
		return nil, fmt.Errorf("kube: reading deployment %q: %w", app, err)
	}

	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + app})
	if err != nil {
		return nil, fmt.Errorf("kube: listing pods for %q: %w", app, err)
	}

	var podOpts corev1.PodLogOptions
	if opts.TailLines > 0 {
		tl := int64(opts.TailLines)
		podOpts.TailLines = &tl
	}

	var lines []controlplane.LogLine
	for _, pod := range pods.Items {
		stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(pod.Name, &podOpts).Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("kube: logs for pod %q: %w", pod.Name, err)
		}
		data, readErr := io.ReadAll(stream)
		stream.Close()
		if readErr != nil {
			return nil, fmt.Errorf("kube: reading logs for pod %q: %w", pod.Name, readErr)
		}
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			lines = append(lines, controlplane.LogLine{Pod: pod.Name, Message: line})
		}
	}
	return lines, nil
}

func (a *Adapter) DeleteWorkload(ctx context.Context, app string) error {
	err := a.client.AppsV1().Deployments(a.namespace).Delete(ctx, app, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("kube: deleting deployment %q: %w", app, err)
	}
	return nil
}

func (a *Adapter) buildDeployment(spec controlplane.WorkloadSpec) *appsv1.Deployment {
	labels := map[string]string{nameLabel: spec.App, managedByLabel: managedByValue}
	selector := map[string]string{nameLabel: spec.App}

	var env []corev1.EnvVar
	for _, k := range sortedKeys(spec.Env) { // deterministic order
		env = append(env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
	}

	replicas := spec.Replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: spec.App, Namespace: a.namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    spec.App,
						Image:   spec.Image,
						Command: spec.Command,
						Env:     env,
					}},
				},
			},
		},
	}
}

func deploymentAvailable(dep *appsv1.Deployment, desired int32) bool {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return desired > 0 && dep.Status.ReadyReplicas >= desired
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
