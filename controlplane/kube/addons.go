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

	// A logs store needs a collector shipping pod logs into it, or it stays empty. Deploy a
	// Fluent Bit (Apache-2.0) DaemonSet that tails the node's container logs and forwards them
	// to the store; it derives pod/namespace from the log filename, so it needs no API access
	// (no RBAC). Other add-on types have no collector.
	if spec.Type == controlplane.AddonLogs {
		if err := a.deployLogsCollector(ctx, name, labels, spec.Port); err != nil {
			return controlplane.AddonInfo{}, err
		}
	}

	return controlplane.AddonInfo{
		Name:         name,
		Type:         spec.Type,
		Mode:         "installed",
		Image:        spec.Image,
		Endpoint:     fmt.Sprintf("%s.%s.svc:%d", name, a.namespace, spec.Port),
		Capabilities: spec.Capabilities,
	}, nil
}

// fluentBitImage is the pinned log collector (Apache-2.0). It ships pod logs to the store.
const fluentBitImage = "fluent/fluent-bit:3.2.10"

// fluentBitConfig tails the node's container logs (CRI format) and forwards each record to the
// VictoriaLogs store at host:9428 over its JSON-lines ingestion API, keeping the source filename
// (which encodes pod/namespace/container) as the stream field. %s is the store service name.
const fluentBitConfig = `[SERVICE]
    Flush        5
    Log_Level    info
    Daemon       Off
[INPUT]
    Name             tail
    Path             /var/log/containers/*.log
    Path_Key         filename
    Tag              kube.*
    multiline.parser cri
    Skip_Long_Lines  On
    Mem_Buf_Limit    16MB
[OUTPUT]
    Name    http
    Match   *
    Host    %s
    Port    9428
    URI     /insert/jsonline?_stream_fields=filename&_msg_field=message&_time_field=time
    Format  json_lines
    Json_date_key    time
    Json_date_format iso8601
`

func (a *Adapter) deployLogsCollector(ctx context.Context, store string, labels map[string]string, _ int32) error {
	name := store + "-collector"
	cmLabels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: labels[addonLabel]}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace, Labels: cmLabels},
		Data:       map[string]string{"fluent-bit.conf": fmt.Sprintf(fluentBitConfig, store)},
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating collector config %q: %w", name, err)
	}

	hostPathDir := corev1.HostPathDirectory
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace, Labels: cmLabels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nameLabel: name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: cmLabels},
				Spec: corev1.PodSpec{
					// Run on every node, including control-plane nodes (k3d's single node is one).
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:  "fluent-bit",
						Image: fluentBitImage,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "varlog", MountPath: "/var/log", ReadOnly: true},
							{Name: "config", MountPath: "/fluent-bit/etc/fluent-bit.conf", SubPath: "fluent-bit.conf"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "varlog", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log", Type: &hostPathDir}}},
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}},
					},
				},
			},
		},
	}
	if _, err := a.client.AppsV1().DaemonSets(a.namespace).Create(ctx, ds, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating collector %q: %w", name, err)
	}
	return nil
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
		typ := controlplane.AddonType(dep.Labels[addonLabel])
		var caps []string
		if spec, ok := controlplane.LookupAddon(typ); ok {
			caps = spec.Capabilities
		}
		out = append(out, controlplane.AddonInfo{
			Name:         dep.Name,
			Type:         typ,
			Mode:         "installed",
			Image:        image,
			Endpoint:     fmt.Sprintf("%s.%s.svc:%d", dep.Name, a.namespace, port),
			Capabilities: caps,
			Ready:        deploymentAvailable(dep, 1),
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
	// And the logs collector, if this add-on had one (harmless no-op otherwise).
	collector := name + "-collector"
	_ = a.client.AppsV1().DaemonSets(a.namespace).Delete(ctx, collector, metav1.DeleteOptions{})
	_ = a.client.CoreV1().ConfigMaps(a.namespace).Delete(ctx, collector, metav1.DeleteOptions{})
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
