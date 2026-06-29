// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

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
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: labels},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", spec.StorageGi))},
				},
			},
		}
		if _, err := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return controlplane.AddonInfo{}, fmt.Errorf("kube: creating addon volume %q: %w", name, err)
		}
		volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}
		mounts = []corev1.VolumeMount{{Name: "data", MountPath: addonDataPath(spec.Type)}}
	}

	// Postgres needs a fixed superuser role and a generated superuser password before its pod
	// starts: burrowd creates the burrow-postgres Secret (the generated password) BEFORE the
	// Deployment and points the pod's POSTGRES_PASSWORD at it via secretKeyRef, so the password is
	// never inlined in the pod spec, never logged, and never returned (ADR-0031). Other add-ons
	// add no env.
	var env []corev1.EnvVar
	if spec.Type == controlplane.AddonPostgres {
		var err error
		if env, err = a.ensurePostgresSuperuserEnv(ctx, labels); err != nil {
			return controlplane.AddonInfo{}, err
		}
	}

	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: labels},
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
						Env:          env,
						Ports:        []corev1.ContainerPort{{ContainerPort: spec.Port}},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	if _, err := a.client.AppsV1().Deployments(a.addonNamespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return controlplane.AddonInfo{}, fmt.Errorf("kube: creating addon %q: %w", name, err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{nameLabel: name},
			Ports:    []corev1.ServicePort{{Port: spec.Port, TargetPort: intstr.FromInt32(spec.Port), Protocol: corev1.ProtocolTCP}},
		},
	}
	if _, err := a.client.CoreV1().Services(a.addonNamespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
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

	// A metrics store stays empty without something feeding it: there is no pre-existing
	// Prometheus to scrape the app. Deploy a vmagent (Apache-2.0) that discovers app pods via
	// the Kubernetes API and remote-writes the samples it scrapes into the store.
	if spec.Type == controlplane.AddonMetrics {
		if err := a.deployMetricsCollector(ctx, name, labels); err != nil {
			return controlplane.AddonInfo{}, err
		}
	}

	return controlplane.AddonInfo{
		Name:         name,
		Type:         spec.Type,
		Mode:         "installed",
		Backend:      spec.Backend,
		Image:        spec.Image,
		Endpoint:     fmt.Sprintf("%s.%s.svc:%d", name, a.addonNamespace, spec.Port),
		Capabilities: spec.Capabilities,
	}, nil
}

// PostgresSuperuser is the fixed superuser role burrowd provisions the add-on Postgres instance
// with and connects as to run admin SQL (ADR-0031). It is deliberately not the built-in "postgres"
// role: a distinct, Burrow-owned admin role keeps the boundary clear.
const PostgresSuperuser = "burrow_admin"

// PostgresSecretName is the Secret in the add-on namespace that holds the generated superuser
// password (ADR-0031). It lives in the add-on namespace — not the control-plane credentials Secret
// — because a pod can only mount a Secret in its own namespace.
const PostgresSecretName = "burrow-postgres"

// PostgresPasswordKey is the key under which the superuser password is stored in PostgresSecretName.
const PostgresPasswordKey = "password"

// generatePassword returns a strong random password: 32 bytes of crypto/rand, base64url-encoded
// (no padding) so it is shell- and URL-safe. It is used for both the superuser password and each
// app role's password; the value is never logged or returned (ADR-0031).
func generatePassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("kube: generating password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ensurePostgresSuperuserEnv creates the burrow-postgres Secret holding a freshly generated
// superuser password (idempotently — an already-present Secret is left untouched, so a re-install
// keeps the existing password and the running database) and returns the pod env that wires the
// Postgres container to it: POSTGRES_USER=burrow_admin (a literal, not a secret), POSTGRES_PASSWORD
// from a secretKeyRef into that Secret, and PGDATA under a subdirectory of the mounted volume. The
// generated password is written ONLY into the Secret — it is never inlined into the pod spec,
// returned, or logged (ADR-0031).
func (a *Adapter) ensurePostgresSuperuserEnv(ctx context.Context, labels map[string]string) ([]corev1.EnvVar, error) {
	secrets := a.client.CoreV1().Secrets(a.addonNamespace)
	if _, err := secrets.Get(ctx, PostgresSecretName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		pw, gerr := generatePassword()
		if gerr != nil {
			return nil, gerr
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: a.addonNamespace, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{PostgresPasswordKey: []byte(pw)},
		}
		if _, cerr := secrets.Create(ctx, sec, metav1.CreateOptions{}); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			// The error names the Secret only — never the generated value.
			return nil, fmt.Errorf("kube: creating postgres superuser secret %q: %w", PostgresSecretName, cerr)
		}
	} else if err != nil {
		return nil, fmt.Errorf("kube: reading postgres superuser secret %q: %w", PostgresSecretName, err)
	}

	return []corev1.EnvVar{
		{Name: "POSTGRES_USER", Value: PostgresSuperuser},
		{Name: "POSTGRES_PASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: PostgresSecretName},
				Key:                  PostgresPasswordKey,
			},
		}},
		// The official image refuses to initialize a non-empty data directory (a mounted PVC has a
		// lost+found), so put PGDATA in a subdirectory of the mount.
		{Name: "PGDATA", Value: addonDataPath(controlplane.AddonPostgres) + "/pgdata"},
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
    URI     /insert/jsonline?_stream_fields=filename&_msg_field=log,message,msg&_time_field=time
    Format  json_lines
    Json_date_key    time
    Json_date_format iso8601
`

func (a *Adapter) deployLogsCollector(ctx context.Context, store string, labels map[string]string, _ int32) error {
	name := store + "-collector"
	cmLabels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: labels[addonLabel]}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: cmLabels},
		Data:       map[string]string{"fluent-bit.conf": fmt.Sprintf(fluentBitConfig, store)},
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.addonNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating collector config %q: %w", name, err)
	}

	hostPathDir := corev1.HostPathDirectory
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: cmLabels},
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
	if _, err := a.client.AppsV1().DaemonSets(a.addonNamespace).Create(ctx, ds, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating collector %q: %w", name, err)
	}
	return nil
}

// vmagentImage is the pinned metrics collector (Apache-2.0). It scrapes app pods and
// remote-writes to the store.
const vmagentImage = "victoriametrics/vmagent:v1.115.0"

// vmagentPort is vmagent's own HTTP listen port. Exposing it lets vmagent self-scrape (the
// kubernetes-pods job below targets localhost:8429), so up{job="vmagent"} always exists.
const vmagentPort = 8429

// vmagentConfig is vmagent's Prometheus-style scrape config. It self-scrapes (so the metrics
// pipeline has a guaranteed series) and discovers pods in the app namespace, keeping only those
// annotated prometheus.io/scrape: "true" and scraping them on their prometheus.io/port. %s is the
// app namespace.
const vmagentConfig = `global:
  scrape_interval: 15s
scrape_configs:
  - job_name: vmagent
    static_configs:
      - targets: ['localhost:8429']
  - job_name: kubernetes-pods
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: [%s]
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        action: keep
        regex: "true"
      - source_labels: [__address__, __meta_kubernetes_pod_annotation_prometheus_io_port]
        action: replace
        regex: ([^:]+)(?::\d+)?;(\d+)
        replacement: $1:$2
        target_label: __address__
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_path]
        action: replace
        target_label: __metrics_path__
        regex: (.+)
      - source_labels: [__meta_kubernetes_namespace]
        target_label: namespace
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
`

// deployMetricsCollector deploys vmagent alongside the metrics store: a ConfigMap holding the
// scrape config and a single-replica Deployment (vmagent does API-based service discovery, so one
// replica suffices — no DaemonSet) that remote-writes into the store. vmagent runs as the
// burrow-vmagent ServiceAccount, whose read-only pod-discovery RBAC in the app namespace is
// pre-provisioned at install time so burrowd never needs RBAC-creation powers.
func (a *Adapter) deployMetricsCollector(ctx context.Context, store string, labels map[string]string) error {
	name := store + "-collector"
	cmLabels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: labels[addonLabel]}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: cmLabels},
		Data:       map[string]string{"scrape.yml": fmt.Sprintf(vmagentConfig, a.namespace)},
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.addonNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating collector config %q: %w", name, err)
	}

	replicas := int32(1)
	remoteWrite := fmt.Sprintf("http://%s.%s.svc:8428/api/v1/write", store, a.addonNamespace)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: cmLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nameLabel: name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: cmLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "burrow-vmagent",
					Containers: []corev1.Container{{
						Name:  "vmagent",
						Image: vmagentImage,
						Args: []string{
							"-promscrape.config=/config/scrape.yml",
							"-remoteWrite.url=" + remoteWrite,
						},
						Ports: []corev1.ContainerPort{{ContainerPort: vmagentPort}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/config"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}},
					},
				},
			},
		},
	}
	if _, err := a.client.AppsV1().Deployments(a.addonNamespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating collector %q: %w", name, err)
	}
	return nil
}

// AddonReady reports whether the named add-on's backing Deployment is available (ADR-0025).
// Readiness is a live property of the cluster — the registry of what add-ons exist lives in the
// database — so this is a cheap single-Deployment probe. A missing Deployment is reported as not
// ready (false, nil); only a real API error is returned.
func (a *Adapter) AddonReady(ctx context.Context, name string) (bool, error) {
	dep, err := a.client.AppsV1().Deployments(a.addonNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kube: reading addon %q: %w", name, err)
	}
	return deploymentAvailable(dep, 1), nil
}

func (a *Adapter) DeleteAddon(ctx context.Context, name string) error {
	deps := a.client.AppsV1().Deployments(a.addonNamespace)
	if _, err := deps.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: addon %q: %w", name, controlplane.ErrNotFound)
	} else if err != nil {
		return fmt.Errorf("kube: reading addon %q: %w", name, err)
	}
	// The Deployment is the source of truth for existence; remove all three resources.
	if err := deps.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting addon %q: %w", name, err)
	}
	_ = a.client.CoreV1().Services(a.addonNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	_ = a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	// And the collector, if this add-on had one (harmless no-op otherwise). The logs collector
	// is a DaemonSet and the metrics collector (vmagent) a Deployment, both named
	// <name>-collector; delete both, one of which will NotFound harmlessly.
	collector := name + "-collector"
	_ = a.client.AppsV1().DaemonSets(a.addonNamespace).Delete(ctx, collector, metav1.DeleteOptions{})
	_ = a.client.AppsV1().Deployments(a.addonNamespace).Delete(ctx, collector, metav1.DeleteOptions{})
	_ = a.client.CoreV1().ConfigMaps(a.addonNamespace).Delete(ctx, collector, metav1.DeleteOptions{})
	// The Postgres add-on owns the burrow-postgres superuser Secret (ADR-0031); remove it on
	// uninstall. The name is fixed, so this is a harmless no-op for any other add-on type.
	if name == addonName(controlplane.AddonPostgres) {
		_ = a.client.CoreV1().Secrets(a.addonNamespace).Delete(ctx, PostgresSecretName, metav1.DeleteOptions{})
	}
	return nil
}

// addonDataPath is the in-container data directory for a stateful add-on.
func addonDataPath(t controlplane.AddonType) string {
	switch t {
	case controlplane.AddonLogs:
		return "/vlogs"
	case controlplane.AddonMetrics:
		return "/victoria-metrics-data"
	case controlplane.AddonPostgres:
		// The official postgres image's conventional data mount. PGDATA is set to a subdirectory
		// of this (see ensurePostgresSuperuserEnv) so the image can initialize over a mounted PVC.
		return "/var/lib/postgresql/data"
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
	case controlplane.AddonMetrics:
		// -retentionPeriod=1 keeps one month of samples (VictoriaMetrics' default unit is months).
		return []string{fmt.Sprintf("-httpListenAddr=:%d", spec.Port), "-storageDataPath=" + addonDataPath(spec.Type), "-retentionPeriod=1"}
	default:
		return nil
	}
}
