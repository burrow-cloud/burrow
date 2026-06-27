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
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Kubernetes = (*Adapter)(nil)

const (
	nameLabel      = "app.kubernetes.io/name"
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "burrow"
	// defaultIngressClass is the IngressClass `burrow ingress install` creates (ingress-nginx).
	// The exposed app's Ingress is bound to it so the controller adopts and routes it.
	defaultIngressClass = "nginx"
)

// defaultAddonNamespace is where add-ons land when none is configured (local/test). In a
// real install it is always set explicitly via WithAddonNamespace from BURROW_ADDON_NAMESPACE;
// connect.DefaultAddonNamespace is the authoritative value the install manifest renders.
const defaultAddonNamespace = "burrow-addons"

// Adapter operates Burrow workloads in a single app namespace, and provisions add-ons in a
// separate add-on namespace (ADR-0025) so backing services don't mix with user workloads.
type Adapter struct {
	client         kubernetes.Interface
	namespace      string
	addonNamespace string
}

// New returns an Adapter over the given clientset and namespace (defaulting to
// "default"). Tests inject a fake clientset; production injects a real one
// (see NewFromConfig).
func New(client kubernetes.Interface, namespace string) *Adapter {
	if namespace == "" {
		namespace = "default"
	}
	return &Adapter{client: client, namespace: namespace, addonNamespace: defaultAddonNamespace}
}

// WithAddonNamespace sets the namespace Burrow deploys add-ons (and their collectors) into,
// kept separate from the app namespace and the credential-holding control-plane namespace
// (ADR-0025). An empty value leaves the default. Returns the Adapter for chaining.
func (a *Adapter) WithAddonNamespace(ns string) *Adapter {
	if ns != "" {
		a.addonNamespace = ns
	}
	return a
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

func (a *Adapter) ListWorkloads(ctx context.Context) ([]controlplane.WorkloadStatus, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("kube: listing deployments: %w", err)
	}
	out := make([]controlplane.WorkloadStatus, 0, len(deps.Items))
	for i := range deps.Items {
		dep := &deps.Items[i]
		var desired int32
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		image := ""
		if c := dep.Spec.Template.Spec.Containers; len(c) > 0 {
			image = c[0].Image
		}
		out = append(out, controlplane.WorkloadStatus{
			App:             dep.Name,
			Kind:            controlplane.WorkloadDeployment,
			Image:           image,
			DesiredReplicas: desired,
			ReadyReplicas:   dep.Status.ReadyReplicas,
			UpdatedReplicas: dep.Status.UpdatedReplicas,
			Available:       deploymentAvailable(dep, desired),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out, nil
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

func (a *Adapter) Expose(ctx context.Context, spec controlplane.ExposeSpec) error {
	services := a.client.CoreV1().Services(a.namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := services.Get(ctx, spec.App, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err := services.Create(ctx, a.buildService(spec), metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		desired := a.buildService(spec)
		desired.ResourceVersion = existing.ResourceVersion
		desired.Spec.ClusterIP = existing.Spec.ClusterIP // ClusterIP is immutable
		_, err = services.Update(ctx, desired, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("kube: applying service %q: %w", spec.App, err)
	}

	ingresses := a.client.NetworkingV1().Ingresses(a.namespace)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := ingresses.Get(ctx, spec.App, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err := ingresses.Create(ctx, a.buildIngress(spec), metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		desired := a.buildIngress(spec)
		desired.ResourceVersion = existing.ResourceVersion
		_, err = ingresses.Update(ctx, desired, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("kube: applying ingress %q: %w", spec.App, err)
	}
	return nil
}

func (a *Adapter) Unexpose(ctx context.Context, app string) error {
	ingErr := a.client.NetworkingV1().Ingresses(a.namespace).Delete(ctx, app, metav1.DeleteOptions{})
	svcErr := a.client.CoreV1().Services(a.namespace).Delete(ctx, app, metav1.DeleteOptions{})

	// Treat the operation as not-found only when neither resource existed; otherwise we
	// removed at least one and report any real failure.
	if apierrors.IsNotFound(ingErr) && apierrors.IsNotFound(svcErr) {
		return fmt.Errorf("kube: exposure for %q: %w", app, controlplane.ErrNotFound)
	}
	if ingErr != nil && !apierrors.IsNotFound(ingErr) {
		return fmt.Errorf("kube: deleting ingress %q: %w", app, ingErr)
	}
	if svcErr != nil && !apierrors.IsNotFound(svcErr) {
		return fmt.Errorf("kube: deleting service %q: %w", app, svcErr)
	}
	return nil
}

func (a *Adapter) ExposureStatus(ctx context.Context, app string) (controlplane.ExposureStatus, error) {
	ing, err := a.client.NetworkingV1().Ingresses(a.namespace).Get(ctx, app, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return controlplane.ExposureStatus{}, nil
	}
	if err != nil {
		return controlplane.ExposureStatus{}, fmt.Errorf("kube: reading ingress %q: %w", app, err)
	}
	out := controlplane.ExposureStatus{Exposed: true, TLS: len(ing.Spec.TLS) > 0}
	if len(ing.Spec.Rules) > 0 {
		out.Host = ing.Spec.Rules[0].Host
	}
	// The ingress controller writes the assigned external address into the Ingress status.
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.IP != "" {
			out.Address = lb.IP
			break
		}
		if lb.Hostname != "" {
			out.Address = lb.Hostname
			break
		}
	}
	return out, nil
}

// buildService is a ClusterIP Service fronting the app's Pods, forwarding port 80 to the
// app's container port.
func (a *Adapter) buildService(spec controlplane.ExposeSpec) *corev1.Service {
	labels := map[string]string{nameLabel: spec.App, managedByLabel: managedByValue}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: spec.App, Namespace: a.namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{nameLabel: spec.App},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(spec.Port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// buildIngress routes spec.Host to the app's Service on port 80, optionally requesting a
// cert-manager TLS certificate for the host.
func (a *Adapter) buildIngress(spec controlplane.ExposeSpec) *networkingv1.Ingress {
	labels := map[string]string{nameLabel: spec.App, managedByLabel: managedByValue}
	pathType := networkingv1.PathTypePrefix
	meta := metav1.ObjectMeta{Name: spec.App, Namespace: a.namespace, Labels: labels}
	var tls []networkingv1.IngressTLS
	if spec.TLS {
		// cert-manager watches this annotation and issues a cert into the named Secret.
		meta.Annotations = map[string]string{"cert-manager.io/cluster-issuer": spec.Issuer}
		tls = []networkingv1.IngressTLS{{Hosts: []string{spec.Host}, SecretName: spec.App + "-tls"}}
	}
	// Bind the Ingress to the ingress-nginx controller. ingress-nginx runs with
	// --ingress-class=nginx and (by default) ignores Ingresses that carry no class, so without
	// this the app Ingress is orphaned: it never gets an external address and the reachability
	// chain stalls. "nginx" is the class `burrow ingress install` sets up (ADR-0018).
	ingressClass := defaultIngressClass
	return &networkingv1.Ingress{
		ObjectMeta: meta,
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			TLS:              tls,
			Rules: []networkingv1.IngressRule{{
				Host: spec.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: spec.App,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
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
