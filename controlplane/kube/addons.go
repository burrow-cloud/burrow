// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

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

// addonEnvLabel records which ENVIRONMENT an add-on instance serves (ADR-0067 §1), so the cluster
// carries the same fact the registry row does and an operator reading `kubectl get deploy -n
// burrow-addons` can tell two instances of the same type apart by more than a name suffix.
//
// It is descriptive only: nothing selects on it, and the instance is always found by its NAME
// (addonName). That is what let ADR-0067 §2 rename the default environment from `default` to `prod`
// without touching a running instance — an add-on installed before the rename still carries
// `burrow.cloud/environment=default` until it is reinstalled, and reads nothing wrong from it,
// because the label is a note to a human rather than an index.
const addonEnvLabel = "burrow.cloud/environment"

// addonVolumeRole records what an add-on claim HOLDS: the add-on's own data (AddonVolumeData) or its
// dumps (AddonVolumeBackup). The two are different things to keep — a data claim comes back to life
// on reinstall, a backup claim is a pile of dumps that outlives the database it came from — and
// until backups were per-environment the role could be read off the one compiled-in claim name.
// It cannot any more: `burrow-postgres-staging.backups` is a backup claim whose name is not that
// constant, and attributing it by shape would be the name-guessing AddonVolumes exists to avoid.
const addonVolumeRole = "burrow.cloud/addon-volume"

// vmagentServiceAccount is the ServiceAccount the metrics add-on's vmagent scraper runs as. It and
// its pod-discovery Role/RoleBinding are NOT created by burrowd: burrowd holds only namespaced Roles
// and is deliberately forbidden from creating RBAC (least privilege). The grant is staged by the CLI
// at install time (`burrow addon install metrics` applies it kubeconfig-side; see
// cmd/burrow/manifests/addon-metrics-rbac.yaml.tmpl), and burrowd only verifies it exists with a
// read-only Get before deploying the scraper.
const vmagentServiceAccount = "burrow-vmagent"

func (a *Adapter) DeployAddon(ctx context.Context, spec controlplane.AddonSpec, env, instance string, archive *controlplane.ArchiveDestination) (controlplane.AddonInfo, error) {
	if instance == "" {
		return controlplane.AddonInfo{}, fmt.Errorf("kube: deploying the %s add-on in environment %q: no instance named: %w", spec.Type, env, controlplane.ErrInvalid)
	}
	name := instance
	// Every resource this creates is named after the INSTANCE, not the type, so a second
	// environment's add-on lands beside the first rather than on top of it (ADR-0067 §1). The
	// environment label records which environment the instance serves, so the cluster view agrees
	// with the registry row.
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: string(spec.Type), addonEnvLabel: env}
	// The data claim carries the role as well, so every claim Burrow creates says what it holds
	// rather than only the ones whose name gives it away.
	volumeLabels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: string(spec.Type), addonEnvLabel: env, addonVolumeRole: controlplane.AddonVolumeData}

	// A Postgres instance is one custom resource and no authored pod at all, so it branches before
	// anything below is created: the operator composes the workload, the volume and the services from
	// the `Cluster` (ADR-0066 §1). Everything downstream of the install — the endpoint, the superuser
	// Secret, attach, and the ADR-0032 backup Jobs — reaches it the same way it always did.
	if spec.Type == controlplane.AddonPostgres {
		return a.deployPostgresCluster(ctx, spec, env, name, labels, archive)
	}

	// The metrics add-on's vmagent scraper references a pre-provisioned ServiceAccount whose RBAC
	// only a kubeconfig-holder can apply (burrowd cannot create RBAC). The CLI self-heals this
	// before calling InstallAddon, but the agent path holds no credential that can write RBAC and
	// cannot. Verify the ServiceAccount exists with a read-only Get FIRST, so an agent-driven install
	// over absent RBAC fails cleanly here instead of half-deploying a vmagent pod that can never
	// schedule. No partial resources are created before this check.
	if spec.Type == controlplane.AddonMetrics {
		if err := a.requireAddonServiceAccount(ctx, vmagentServiceAccount); err != nil {
			return controlplane.AddonInfo{}, err
		}
	}

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if spec.StorageGi > 0 {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: volumeLabels},
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
						Name:  string(spec.Type),
						Image: spec.Image,
						// The metrics add-on's sample retention is cluster configuration read HERE,
						// at creation (ADR-0068 §6). An instance that already exists keeps the
						// retention it was created with — the args are a field on its pod template,
						// not a policy the add-on re-reads — so a change applies to the next install
						// of it rather than to the running one.
						Args:         addonArgs(spec, a.limits.ClusterDuration(ctx, controlplane.LimitAddonMetricRetention)),
						Ports:        []corev1.ContainerPort{{ContainerPort: spec.Port}},
						VolumeMounts: mounts,
						// The instance joins its Service when it ANSWERS, not when its container
						// starts (AddonSpec.Readiness). This is the whole of what the endpoint
						// returned below promises, and it is authored through the same resolver
						// and the same renderer an app's probe takes (ADR-0076), so there is one
						// mechanism rather than an add-on-shaped copy of it. LivenessProbe and
						// StartupProbe stay unset here for the reason they stay unset there:
						// ADR-0076 §1, a wrong liveness probe restarts a working container in a
						// loop and presents as the crash loop it was installed to detect.
						ReadinessProbe: readinessProbe(spec.Readiness()),
					}},
					Volumes: volumes,
				},
			},
		},
	}
	// An add-on instance runs an image BURROW chose (VictoriaLogs, VictoriaMetrics, Valkey), not the
	// app's, so it takes the PLATFORM hook (ADR-0073 §2) — an operator who sandboxed the tenant's
	// image on tenant-only nodes did not ask for their own log store there. Applied last, over the
	// fully-constructed pod spec, so it sees the containers and volumes composed above; a nil mutator
	// leaves the Deployment exactly as built (§4). The Postgres instance takes the placement seam
	// ADR-0077 built instead, because its pods are the OPERATOR's and there is no PodSpec here to
	// hand over.
	a.applyPlatformPodMutator(&dep.Spec.Template.Spec)
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
		Environment:  env,
		Mode:         "installed",
		Backend:      spec.Backend,
		Image:        spec.Image,
		Endpoint:     fmt.Sprintf("%s.%s.svc:%d", name, a.addonNamespace, spec.Port),
		Capabilities: spec.Capabilities,
		// Every add-on type says what it does about backups, including the types that do nothing
		// (issue #466). A logs or metrics store holding gigabytes on a volume nothing copies is a fact
		// worth stating at install time rather than one to discover after a node goes.
		Backups: controlplane.TypeBackups(spec.Type),
	}, nil
}

// requireAddonServiceAccount confirms an add-on's pre-provisioned ServiceAccount exists in the
// add-on namespace with a read-only Get (burrowd has serviceaccounts:get there, never create). A
// missing one means the CLI never staged the add-on's RBAC, so it returns a clear, typed error
// (wrapping ErrInvalid, which the API maps to a 4xx rather than a 500) telling the operator to run
// the install from a machine with kubeconfig access. Any other API error is surfaced as-is.
func (a *Adapter) requireAddonServiceAccount(ctx context.Context, name string) error {
	_, err := a.client.CoreV1().ServiceAccounts(a.addonNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("metrics requires a one-time RBAC grant that only your kubeconfig can apply; "+
			"run `burrow addon install metrics` from a machine with kubeconfig access (the CLI applies it "+
			"automatically), then retry: %w", controlplane.ErrInvalid)
	}
	if err != nil {
		return fmt.Errorf("kube: verifying the %q service account: %w", name, err)
	}
	return nil
}

// PostgresSuperuser is the fixed superuser role burrowd provisions the add-on Postgres instance
// with and connects as to run admin SQL (ADR-0031). It is deliberately not the built-in "postgres"
// role: a distinct, Burrow-owned admin role keeps the boundary clear.
const PostgresSuperuser = "burrow_admin"

// PostgresSecretName is the Secret in the add-on namespace that holds the DEFAULT environment's
// generated superuser password (ADR-0031). It lives in the add-on namespace — not the control-plane
// credentials Secret — because a pod can only mount a Secret in its own namespace.
//
// The Secret is named after the INSTANCE, so every environment's instance has its own superuser
// credential and this constant is the default environment's case of that rule (ADR-0067 §1) — which
// is why an install predating environments keeps the Secret, the volume, and the password it already
// has. Every other instance's Secret is named after that instance, and the name comes from the
// registry rather than from a derivation here (ADR-0091 §2): the engine resolves the instance and
// hands it down, so this adapter composes no instance name of its own.
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

// postgresExporterPort is the port postgres_exporter listens on (its documented default).
const postgresExporterPort int32 = 9187

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
					// This blanket toleration is hard-coded on purpose and stays (ADR-0073 §3): a
					// node-log collector that skips tainted nodes silently loses exactly those
					// nodes' logs, and it is the one placement field the engine legitimately knows
					// the answer to. The platform hook below still runs over it.
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
	// Fluent Bit is Burrow's image, not the app's, so the collector takes the PLATFORM hook
	// (ADR-0073 §2). Applied last, over the fully-constructed pod spec — which means the mutator
	// meets the blanket toleration above and must tolerate it: replacing the toleration slice
	// outright leaves the collector unable to run on tainted nodes, which is a collector that
	// quietly stops collecting rather than an error. A nil mutator leaves the DaemonSet exactly as
	// built (§4).
	a.applyPlatformPodMutator(&ds.Spec.Template.Spec)
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
// pipeline has a guaranteed series) and discovers pods in both the app namespace and the add-on
// namespace, keeping only those annotated prometheus.io/scrape: "true" and scraping them on their
// prometheus.io/port. Discovering the add-on namespace is what picks up the Postgres exporter (and
// any future exporting add-on) regardless of install order (ADR-0051). %s is the comma-separated
// namespace list, deduped when the two namespaces are the same.
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
		Data:       map[string]string{"scrape.yml": fmt.Sprintf(vmagentConfig, a.scrapeNamespaces())},
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
					ServiceAccountName: vmagentServiceAccount,
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
	// vmagent is Burrow's image, not the app's, so the collector takes the PLATFORM hook (ADR-0073
	// §2) — it scrapes the tenant's pods but is not one, and belongs wherever the operator puts
	// Burrow's own workloads. Applied last, over the fully-constructed pod spec, so the hook can see
	// the ServiceAccount and config mount above; a nil mutator leaves the Deployment exactly as
	// built (§4).
	a.applyPlatformPodMutator(&dep.Spec.Template.Spec)
	if _, err := a.client.AppsV1().Deployments(a.addonNamespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating collector %q: %w", name, err)
	}
	return nil
}

// scrapeNamespaces is the comma-separated namespace list vmagent discovers pods in: the app
// namespace (app-pod metrics) and the add-on namespace (the always-on Postgres exporter and any
// future exporting add-on). When the two are the same it lists the namespace once, so a single-
// namespace install does not double-scrape (ADR-0051).
func (a *Adapter) scrapeNamespaces() string {
	if a.addonNamespace == a.namespace {
		return a.namespace
	}
	return a.namespace + ", " + a.addonNamespace
}

// AddonReady reports whether the named add-on's backing workload is available (ADR-0025). Readiness
// is a live property of the cluster — the registry of what add-ons exist lives in the database — so
// this is a cheap single-object probe. A missing workload is reported as not ready (false, nil);
// only a real API error is returned.
//
// AVAILABLE MEANS ANSWERING, and it means that because of the readiness probe on the instance's
// container and not because of anything computed here. Availability is derived from ready replicas,
// and a container with no probe is ready the moment it is running — so this read, the endpoints
// behind the add-on's Service, and the install's own rollout wait ALL used to report a store that
// had not yet bound its socket. One probe fixes the three of them at once, because all three were
// reading the same input.
//
// WHICH OBJECT IS THE ADD-ON is resolved here rather than assumed, and that is what keeps a Postgres
// instance from reading as an ADR-0074 §6 discrepancy. §6's diagnosis is an ABSENCE — the registry
// says this exists and the cluster does not have it — and it is made by the failure observer from
// exactly this seam's answer. A Postgres instance has no Deployment by design: the operator
// reconciles a StatefulSet from the `Cluster` Burrow wrote. A probe that only looked for a
// Deployment would report a serving database as not running, once a minute, forever, and the ledger
// would carry an AddonNotRunning row about an add-on that is fine — a false absence, which is worse
// than no ledger at all because it is indistinguishable from a real one.
//
// The Deployment is looked for FIRST because it answers for every other add-on in one Get. The
// custom resource is consulted only when there is none, so a cluster running no Postgres add-on
// pays nothing for this.
//
// No new REASON is introduced by any of this. A `Cluster` that will not come up is an add-on whose
// workload is not available, which is what ReasonAddonNotRunning already says; the ledger's
// vocabulary is closed (ADR-0074 §5) and this does not widen it.
func (a *Adapter) AddonReady(ctx context.Context, name string) (bool, error) {
	dep, err := a.client.AppsV1().Deployments(a.addonNamespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return deploymentAvailable(dep, 1), nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("kube: reading addon %q: %w", name, err)
	}
	cluster, found, err := a.getCNPGCluster(ctx, name)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return cnpgClusterReady(cluster), nil
}

// DeleteAddon tears an add-on's workload down and, only when deleteData is set, destroys its data
// volume with it. The default is data-preserving: the PVC of a stateful add-on outlives the removal,
// so `addon remove` stops the instance without destroying what it holds (ADR-0025, ADR-0064 §1). The
// retained volume keeps its resource name, which is also the add-on name, so a re-install lands on
// exactly the same claim and the instance comes back with its data.
//
// t is the add-on's TYPE, and it is TOLD rather than discovered. The registry recorded it at install,
// and it decides which teardown runs: a Postgres instance is a CloudNativePG `Cluster` (ADR-0066 §1)
// with no Deployment at all, and every other add-on is the Deployment below. A removal is the one
// operation that must not infer that — inferring means reading the cluster, and every way of failing
// to read the cluster looks like "the object is not there", which on this path would mean walking
// past a running database and deleting the registry row that named it. The teardown itself still
// refuses anything it cannot read (cnpg_remove.go).
func (a *Adapter) DeleteAddon(ctx context.Context, name string, t controlplane.AddonType, deleteData bool) (controlplane.AddonRemoval, error) {
	if t == controlplane.AddonPostgres {
		return a.deletePostgresCluster(ctx, name, deleteData)
	}
	removal := controlplane.AddonRemoval{Namespace: a.addonNamespace}
	deps := a.client.AppsV1().Deployments(a.addonNamespace)
	if _, err := deps.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return removal, fmt.Errorf("kube: addon %q: %w", name, controlplane.ErrNotFound)
	} else if err != nil {
		return removal, fmt.Errorf("kube: reading addon %q: %w", name, err)
	}
	// The Deployment is the source of truth for existence; the workload and its Service always go.
	if err := deps.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return removal, fmt.Errorf("kube: deleting addon %q: %w", name, err)
	}
	_ = a.client.CoreV1().Services(a.addonNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	// And the collector, if this add-on had one (harmless no-op otherwise). The logs collector
	// is a DaemonSet and the metrics collector (vmagent) a Deployment, both named
	// <name>-collector; delete both, one of which will NotFound harmlessly.
	collector := name + "-collector"
	_ = a.client.AppsV1().DaemonSets(a.addonNamespace).Delete(ctx, collector, metav1.DeleteOptions{})
	_ = a.client.AppsV1().Deployments(a.addonNamespace).Delete(ctx, collector, metav1.DeleteOptions{})
	_ = a.client.CoreV1().ConfigMaps(a.addonNamespace).Delete(ctx, collector, metav1.DeleteOptions{})

	pvcs := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace)
	if deleteData {
		if err := pvcs.Delete(ctx, name, metav1.DeleteOptions{}); err == nil {
			removal.DataDeleted = true
		} else if !apierrors.IsNotFound(err) {
			return removal, fmt.Errorf("kube: deleting addon volume %q: %w", name, err)
		}
	} else if _, err := pvcs.Get(ctx, name, metav1.GetOptions{}); err == nil {
		removal.RetainedDataVolume = name
	}
	return removal, nil
}

// AddonVolumes lists the add-on PersistentVolumeClaims in the add-on namespace — every claim Burrow
// created for an add-on, including the ones whose add-on has since been removed. It is the read that
// makes ADR-0064 §6 possible: a removed add-on leaves no registry row, so the claim it left behind
// can only be found by looking at the cluster.
//
// Claims are identified by the LABELS Burrow writes at creation, never by a name prefix: a claim is
// Burrow's when it carries app.kubernetes.io/managed-by=burrow, and it is attributed to an add-on by
// the burrow.cloud/addon label recording the type. A user's own claim in this namespace carries
// neither and is not reported.
func (a *Adapter) AddonVolumes(ctx context.Context) ([]controlplane.AddonVolume, error) {
	list, err := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("kube: listing addon volumes in %q: %w", a.addonNamespace, err)
	}
	out := make([]controlplane.AddonVolume, 0, len(list.Items))
	for i := range list.Items {
		pvc := &list.Items[i]
		addon, role, ok := addonVolumeOwner(pvc)
		if !ok {
			continue
		}
		out = append(out, controlplane.AddonVolume{
			Name:        pvc.Name,
			Namespace:   a.addonNamespace,
			Addon:       addon,
			Environment: pvc.Labels[addonEnvLabel],
			Role:        role,
			Size:        addonVolumeSize(pvc),
			// A data claim keeps the add-on's resource name, so a reinstall creates over it and the
			// instance comes back with its data (ADR-0064 §1). The backup claim is mounted by the
			// backup/restore Jobs instead, so a reinstall does not adopt it.
			ReinstallAdopts: role == controlplane.AddonVolumeData,
			CreatedAt:       pvc.CreationTimestamp.Time,
		})
	}
	return out, nil
}

// addonVolumeOwner attributes a claim to the add-on it serves and says what it holds. The LABELS are
// authoritative: the add-on label names the type and the role label says whether the claim holds the
// add-on's data or its dumps.
//
// Two fallbacks cover claims written before those labels, and neither guesses from a name shape. A
// claim named exactly controlplane.PostgresBackupVolume is a backup claim — safe to match because it
// is a compiled-in constant, not a prefix guess about what a user might have named something. Any
// other labelled claim predates the role label and can only be a data claim, since the backup path
// never created one under another name until backups became per-environment and started labelling
// them.
func addonVolumeOwner(pvc *corev1.PersistentVolumeClaim) (controlplane.AddonType, string, bool) {
	if role := pvc.Labels[addonVolumeRole]; role != "" {
		if t := pvc.Labels[addonLabel]; t != "" {
			return controlplane.AddonType(t), role, true
		}
	}
	if pvc.Name == controlplane.PostgresBackupVolume {
		return controlplane.AddonPostgres, controlplane.AddonVolumeBackup, true
	}
	if t := pvc.Labels[addonLabel]; t != "" {
		return controlplane.AddonType(t), controlplane.AddonVolumeData, true
	}
	return "", "", false
}

// addonEnvironment reads the environment an add-on resource serves from its labels, resolving an
// absent label to the default environment. A resource created before add-ons were per-environment
// carries no label and can only be the default environment's, because that is the only one that
// existed when it was created (ADR-0067 §3).
func addonEnvironment(labels map[string]string) string {
	if env := labels[addonEnvLabel]; env != "" {
		return env
	}
	return controlplane.DefaultEnvironment
}

// addonVolumeSize reports the claim's capacity: what the cluster actually provisioned once the claim
// is bound, falling back to what was requested while it is not. Size is what a claim can honestly
// report — cost would need the provider's per-GiB price, which Burrow does not have (ADR-0064).
func addonVolumeSize(pvc *corev1.PersistentVolumeClaim) string {
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		return q.String()
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		return q.String()
	}
	return ""
}

// addonDataPath is the in-container data directory for a stateful add-on.
func addonDataPath(t controlplane.AddonType) string {
	switch t {
	case controlplane.AddonLogs:
		return "/vlogs"
	case controlplane.AddonMetrics:
		return "/victoria-metrics-data"
	default:
		return "/data"
	}
}

// addonArgs are the container args for an add-on: listen on its port and persist under the
// mounted data path.
//
// metricRetention is the configured sample retention for the metrics add-on (ADR-0068 §6), resolved
// by the caller. It is ignored by every other add-on.
func addonArgs(spec controlplane.AddonSpec, metricRetention time.Duration) []string {
	switch spec.Type {
	case controlplane.AddonLogs:
		return []string{fmt.Sprintf("-httpListenAddr=:%d", spec.Port), "-storageDataPath=" + addonDataPath(spec.Type)}
	case controlplane.AddonMetrics:
		// -retentionPeriod is how long VictoriaMetrics keeps samples. It was the literal `1` — one
		// month, VictoriaMetrics' unit for a bare number — and is now cluster configuration
		// (`addon.metric_retention`), whose built-in default is that same month expressed as the 744
		// hours VictoriaMetrics means by it.
		//
		// It is rendered in SECONDS rather than passed through as a Go duration string: the seconds
		// suffix is one VictoriaMetrics documents, and a compound form like "744h0m0s" would be
		// relying on its parser being lenient about a shape nothing else produces.
		return []string{
			fmt.Sprintf("-httpListenAddr=:%d", spec.Port),
			"-storageDataPath=" + addonDataPath(spec.Type),
			fmt.Sprintf("-retentionPeriod=%ds", int64(metricRetention.Seconds())),
		}
	default:
		return nil
	}
}

// LeanPostgresSettings are the server settings Burrow runs its Postgres instances with — both the
// control-plane state database (ADR-0012, rendered into cmd/burrow/manifests/install.yaml.tmpl) and
// the Postgres add-on, whose `Cluster` carries them as spec.postgresql.parameters
// (leanPostgresParameters). Burrow's databases are low-traffic control-plane/metadata stores, so
// the stock postgres defaults (128MB shared_buffers, 100 max_connections, default work_mem) are
// wildly generous; these lean values let the whole stack (k3s + burrowd + Postgres) fit a 1-2GB VPS
// with real headroom. The install manifest hard-codes the SAME values as postgres args — keep the
// two in step.
var LeanPostgresSettings = []string{
	"shared_buffers=64MB",
	"max_connections=30",
	"work_mem=4MB",
	"maintenance_work_mem=32MB",
	"effective_cache_size=256MB",
}

// postgresResources is the memory footprint declared on every Burrow Postgres pod (control-plane and
// add-on). With the lean settings above, steady-state RSS sits around 100-150MB; the 320Mi limit
// leaves genuine headroom so normal and moderate load never OOMKills the database (a control-plane DB
// OOMKill is bad), while the 96Mi request reflects its true idle footprint for the scheduler. A small
// CPU request keeps it schedulable and there is deliberately NO CPU limit — CPU throttling a database
// hurts latency, and the memory limit is the footprint that matters on a small box.
func postgresResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("96Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("320Mi"),
		},
	}
}
