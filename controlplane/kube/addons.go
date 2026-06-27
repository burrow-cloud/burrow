// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/burrow-cloud/burrow/controlplane"
)

// addonLabel marks a Deployment/Service/PVC as a Burrow add-on instance and records its type,
// so add-ons are listed and removed by reading the cluster — the cluster is the registry, the
// same way ListWorkloads reads apps (ADR-0025).
const addonLabel = "burrow.cloud/addon"

// addonName is the deterministic resource name for an add-on of type t (one instance per type).
func addonName(t controlplane.AddonType) string { return "burrow-" + string(t) }

func (a *Adapter) DeployAddon(ctx context.Context, spec controlplane.AddonSpec) (controlplane.AddonInfo, error) {
	name := addonName(spec.Type)
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: string(spec.Type)}

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if spec.StorageGi > 0 {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace, Labels: labels},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", spec.StorageGi))},
				},
			},
		}
		if _, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return controlplane.AddonInfo{}, fmt.Errorf("kube: creating addon volume %q: %w", name, err)
		}
		volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}
		mounts = []corev1.VolumeMount{{Name: "data", MountPath: addonDataPath(spec.Type)}}
	}

	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nameLabel: name}},
			// A ReadWriteOnce volume can't be held by two pods at once, so a rolling update
			// would deadlock — recreate instead.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:         string(spec.Type),
						Image:        spec.Image,
						Args:         addonArgs(spec),
						Ports:        []corev1.ContainerPort{{ContainerPort: spec.Port}},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	if _, err := a.client.AppsV1().Deployments(a.namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return controlplane.AddonInfo{}, fmt.Errorf("kube: creating addon %q: %w", name, err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{nameLabel: name},
			Ports:    []corev1.ServicePort{{Port: spec.Port, TargetPort: intstr.FromInt32(spec.Port), Protocol: corev1.ProtocolTCP}},
		},
	}
	if _, err := a.client.CoreV1().Services(a.namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return controlplane.AddonInfo{}, fmt.Errorf("kube: creating addon service %q: %w", name, err)
	}

	return controlplane.AddonInfo{
		Name:     name,
		Type:     spec.Type,
		Image:    spec.Image,
		Endpoint: fmt.Sprintf("%s.%s.svc:%d", name, a.namespace, spec.Port),
	}, nil
}

func (a *Adapter) ListAddons(ctx context.Context) ([]controlplane.AddonInfo, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: addonLabel})
	if err != nil {
		return nil, fmt.Errorf("kube: listing addons: %w", err)
	}
	out := make([]controlplane.AddonInfo, 0, len(deps.Items))
	for i := range deps.Items {
		dep := &deps.Items[i]
		image, port := "", int32(0)
		if c := dep.Spec.Template.Spec.Containers; len(c) > 0 {
			image = c[0].Image
			if len(c[0].Ports) > 0 {
				port = c[0].Ports[0].ContainerPort
			}
		}
		out = append(out, controlplane.AddonInfo{
			Name:     dep.Name,
			Type:     controlplane.AddonType(dep.Labels[addonLabel]),
			Image:    image,
			Endpoint: fmt.Sprintf("%s.%s.svc:%d", dep.Name, a.namespace, port),
			Ready:    deploymentAvailable(dep, 1),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (a *Adapter) DeleteAddon(ctx context.Context, name string) error {
	deps := a.client.AppsV1().Deployments(a.namespace)
	if _, err := deps.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: addon %q: %w", name, controlplane.ErrNotFound)
	} else if err != nil {
		return fmt.Errorf("kube: reading addon %q: %w", name, err)
	}
	// The Deployment is the source of truth for existence; remove all three resources.
	if err := deps.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting addon %q: %w", name, err)
	}
	_ = a.client.CoreV1().Services(a.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	_ = a.client.CoreV1().PersistentVolumeClaims(a.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	return nil
}

// addonDataPath is the in-container data directory for a stateful add-on.
func addonDataPath(t controlplane.AddonType) string {
	switch t {
	case controlplane.AddonLogs:
		return "/vlogs"
	default:
		return "/data"
	}
}

// addonArgs are the container args for an add-on: listen on its port and persist under the
// mounted data path.
func addonArgs(spec controlplane.AddonSpec) []string {
	switch spec.Type {
	case controlplane.AddonLogs:
		return []string{fmt.Sprintf("-httpListenAddr=:%d", spec.Port), "-storageDataPath=" + addonDataPath(spec.Type)}
	default:
		return nil
	}
}
