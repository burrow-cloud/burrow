// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
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

// addonName is the deterministic resource name for the instance of add-on type t serving
// environment env — one instance PER TYPE PER ENVIRONMENT (ADR-0067 §1). The derivation lives in the
// controlplane package (AddonInstanceName) so the engine, the registry, and this adapter cannot
// drift on which server an environment means; the default environment keeps the unqualified name an
// existing install already carries, so nothing migrates.
func addonName(t controlplane.AddonType, env string) (string, error) {
	return controlplane.AddonInstanceName(t, env)
}

// vmagentServiceAccount is the ServiceAccount the metrics add-on's vmagent scraper runs as. It and
// its pod-discovery Role/RoleBinding are NOT created by burrowd: burrowd holds only namespaced Roles
// and is deliberately forbidden from creating RBAC (least privilege). The grant is staged by the CLI
// at install time (`burrow addon install metrics` applies it kubeconfig-side; see
// cmd/burrow/manifests/addon-metrics-rbac.yaml.tmpl), and burrowd only verifies it exists with a
// read-only Get before deploying the scraper.
const vmagentServiceAccount = "burrow-vmagent"

func (a *Adapter) DeployAddon(ctx context.Context, spec controlplane.AddonSpec, env string) (controlplane.AddonInfo, error) {
	name, err := addonName(spec.Type, env)
	if err != nil {
		return controlplane.AddonInfo{}, err
	}
	// Every resource this creates is named after the INSTANCE, not the type, so a second
	// environment's add-on lands beside the first rather than on top of it (ADR-0067 §1). The
	// environment label records which environment the instance serves, so the cluster view agrees
	// with the registry row.
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue, addonLabel: string(spec.Type), addonEnvLabel: env}

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
	var podEnv []corev1.EnvVar
	// readiness gates the Service (and AddonReady) on the backing service actually accepting
	// connections, not merely on the container running. Postgres needs it: on first boot the
	// official image runs initdb and a temporary socket-only server before binding TCP 5432, so a
	// pod can be "running" for several seconds before it serves — a TCP probe on its port becomes
	// ready only when the real server is up.
	var readiness *corev1.Probe
	// Postgres declares a memory footprint (request + limit) so it fits a small VPS predictably;
	// other add-ons keep the cluster's defaults for now.
	var resources corev1.ResourceRequirements
	// The Postgres pod runs an always-on metrics-exporter sidecar and carries scrape annotations so
	// the metrics add-on (whenever it is installed, before or after Postgres) discovers and scrapes
	// it (ADR-0051); other add-ons add neither.
	var extraContainers []corev1.Container
	var podAnnotations map[string]string
	if spec.Type == controlplane.AddonPostgres {
		if podEnv, err = a.ensurePostgresSuperuserEnv(ctx, name, labels); err != nil {
			return controlplane.AddonInfo{}, err
		}
		resources = postgresResources()
		readiness = &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(spec.Port)}},
			InitialDelaySeconds: 3,
			PeriodSeconds:       3,
			TimeoutSeconds:      2,
			// ~90s of grace for initdb on a cold first boot before the pod is marked unready.
			FailureThreshold: 30,
		}

		// Always export metrics: a postgres_exporter sidecar connects to the local server as the
		// superuser and exposes Prometheus metrics on :9187, including pg_stat_statements slow-query
		// stats. The password is wired via secretKeyRef into burrow-postgres, never inlined (ADR-0031).
		extraContainers = append(extraContainers, postgresExporterContainer(name))
		// Annotate the pod so the metrics add-on's vmagent scrapes the exporter's /metrics on :9187 —
		// the same annotation style app pods use (adapter.go buildDeployment). Discovery is namespace
		// based, so install order (metrics before or after Postgres) does not matter (ADR-0051).
		podAnnotations = map[string]string{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   strconv.Itoa(int(postgresExporterPort)),
			"prometheus.io/path":   "/metrics",
		}
		// Create the pg_stat_statements extension on first boot via an init script the official image
		// runs during initdb; shared_preload_libraries (set in addonArgs) loads the module itself.
		initVol, initMount, ierr := a.ensurePostgresInitScript(ctx, name, labels)
		if ierr != nil {
			return controlplane.AddonInfo{}, ierr
		}
		volumes = append(volumes, initVol)
		mounts = append(mounts, initMount)
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
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					Containers: append([]corev1.Container{{
						Name:  string(spec.Type),
						Image: spec.Image,
						// The metrics add-on's sample retention is cluster configuration read HERE,
						// at creation (ADR-0068 §6). An instance that already exists keeps the
						// retention it was created with — the args are a field on its pod template,
						// not a policy the add-on re-reads — so a change applies to the next install
						// of it rather than to the running one.
						Args:           addonArgs(spec, a.limits.ClusterDuration(ctx, controlplane.LimitAddonMetricRetention)),
						Env:            podEnv,
						Ports:          []corev1.ContainerPort{{ContainerPort: spec.Port}},
						VolumeMounts:   mounts,
						ReadinessProbe: readiness,
						Resources:      resources,
					}}, extraContainers...),
					Volumes: volumes,
				},
			},
		},
	}
	// An add-on instance runs an image BURROW chose (Postgres, VictoriaLogs, VictoriaMetrics,
	// Valkey), not the app's, so it takes the PLATFORM hook (ADR-0073 §2) — an operator who
	// sandboxed the tenant's image on tenant-only nodes did not ask for their own Postgres there.
	// Applied last, over the fully-constructed pod spec, so it sees the containers, volumes, and
	// exporter sidecar composed above; a nil mutator leaves the Deployment exactly as built (§4).
	// This is also the hook's most dangerous reach: the Postgres instance is stateful, and moving it
	// to a pool where its volume cannot attach breaks the add-on rather than one deploy.
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
// has. Use postgresSecretName(env) on any path that can serve more than the default environment.
const PostgresSecretName = "burrow-postgres"

// postgresSecretName is the superuser Secret for environment env's Postgres instance: the instance's
// own name, since the credential that opens a server belongs to that server alone (ADR-0067 §1).
func postgresSecretName(env string) (string, error) {
	return addonName(controlplane.AddonPostgres, env)
}

// postgresInitConfigMap is the first-boot SQL ConfigMap for environment env's instance. It is
// per-instance for the same reason the Secret is: it is consumed during that server's initdb, and a
// removal that destroys one environment's data must not delete the script another environment's
// instance would need on its next fresh boot.
func postgresInitConfigMap(instance string) string { return instance + "-init" }

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
func (a *Adapter) ensurePostgresSuperuserEnv(ctx context.Context, instance string, labels map[string]string) ([]corev1.EnvVar, error) {
	secrets := a.client.CoreV1().Secrets(a.addonNamespace)
	if _, err := secrets.Get(ctx, instance, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		pw, gerr := generatePassword()
		if gerr != nil {
			return nil, gerr
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: instance, Namespace: a.addonNamespace, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{PostgresPasswordKey: []byte(pw)},
		}
		if _, cerr := secrets.Create(ctx, sec, metav1.CreateOptions{}); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			// The error names the Secret only — never the generated value.
			return nil, fmt.Errorf("kube: creating postgres superuser secret %q: %w", instance, cerr)
		}
	} else if err != nil {
		return nil, fmt.Errorf("kube: reading postgres superuser secret %q: %w", instance, err)
	}

	return []corev1.EnvVar{
		{Name: "POSTGRES_USER", Value: PostgresSuperuser},
		{Name: "POSTGRES_PASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: instance},
				Key:                  PostgresPasswordKey,
			},
		}},
		// The add-on standardizes on the "postgres" maintenance database: it is what the control
		// plane connects to (postgres.go connectAdmin), what the metrics exporter reads, and where the
		// init script must create pg_stat_statements — so setting POSTGRES_DB=postgres runs the init
		// script against "postgres" and keeps all three in agreement (ADR-0051). The image otherwise
		// defaults POSTGRES_DB to the POSTGRES_USER name (burrow_admin), which would leave the
		// extension in the wrong database. "postgres" always exists from initdb, so this adds no db.
		{Name: "POSTGRES_DB", Value: "postgres"},
		// The official image refuses to initialize a non-empty data directory (a mounted PVC has a
		// lost+found), so put PGDATA in a subdirectory of the mount.
		{Name: "PGDATA", Value: addonDataPath(controlplane.AddonPostgres) + "/pgdata"},
	}, nil
}

// postgresExporterImage is the pinned Postgres metrics exporter (prometheus-community, Apache-2.0).
// It runs as an always-on sidecar on the Postgres add-on pod, connects to the local server as the
// superuser, and exposes Prometheus metrics — including pg_stat_statements slow-query stats — so the
// metrics add-on's vmagent can scrape database health when it is installed (ADR-0051).
const postgresExporterImage = "quay.io/prometheuscommunity/postgres-exporter:v0.20.1"

// postgresExporterPort is the port postgres_exporter listens on (its documented default).
const postgresExporterPort int32 = 9187

// PostgresInitConfigMap is the ConfigMap whose first-boot SQL the official postgres image runs
// during initdb (mounted into /docker-entrypoint-initdb.d) for the DEFAULT environment's instance.
// It creates the pg_stat_statements extension so the metrics exporter's slow-query collector has
// data (ADR-0051). It lives in the add-on namespace and is removed with the add-on it belongs to;
// every other environment's instance gets its own, named by postgresInitConfigMap (ADR-0067 §1).
const PostgresInitConfigMap = "burrow-postgres-init"

// postgresExporterContainer builds the always-on postgres_exporter sidecar. It connects to the local
// server over localhost as the Burrow superuser, with the password sourced by secretKeyRef from the
// burrow-postgres Secret — never inlined into the pod spec (ADR-0031) — and enables the
// stat_statements collector so slow-query metrics are exported. Its footprint is tiny (32Mi request,
// 64Mi limit) so the always-on cost is negligible.
func postgresExporterContainer(instance string) corev1.Container {
	return corev1.Container{
		Name:  "metrics-exporter",
		Image: postgresExporterImage,
		// Enable the pg_stat_statements collector (disabled by default) so slow-query stats export.
		Args: []string{"--collector.stat_statements"},
		Env: []corev1.EnvVar{
			// The exporter connects over the pod loopback to the co-located server; sslmode=disable
			// because the traffic never leaves the pod. The "postgres" maintenance database always
			// exists, and pg_stat_statements is a shared, cluster-wide view reachable from it.
			{Name: "DATA_SOURCE_URI", Value: "localhost:5432/postgres?sslmode=disable"},
			{Name: "DATA_SOURCE_USER", Value: PostgresSuperuser},
			{Name: "DATA_SOURCE_PASS", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: instance},
					Key:                  PostgresPasswordKey,
				},
			}},
		},
		Ports: []corev1.ContainerPort{{ContainerPort: postgresExporterPort}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
	}
}

// ensurePostgresInitScript creates (idempotently) the ConfigMap holding the first-boot SQL that
// creates the pg_stat_statements extension, and returns the pod volume + mount that hands it to the
// official image's /docker-entrypoint-initdb.d hook. The script runs only on a fresh initdb; on a
// re-install over an existing volume it is a harmless no-op (CREATE EXTENSION IF NOT EXISTS).
func (a *Adapter) ensurePostgresInitScript(ctx context.Context, instance string, labels map[string]string) (corev1.Volume, corev1.VolumeMount, error) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: postgresInitConfigMap(instance), Namespace: a.addonNamespace, Labels: labels},
		Data:       map[string]string{"10-pg-stat-statements.sql": "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;\n"},
	}
	if _, err := a.client.CoreV1().ConfigMaps(a.addonNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return corev1.Volume{}, corev1.VolumeMount{}, fmt.Errorf("kube: creating postgres init config %q: %w", postgresInitConfigMap(instance), err)
	}
	vol := corev1.Volume{Name: "init", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: postgresInitConfigMap(instance)}}}}
	mount := corev1.VolumeMount{Name: "init", MountPath: "/docker-entrypoint-initdb.d"}
	return vol, mount, nil
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

// DeleteAddon tears an add-on's workload down and, only when deleteData is set, destroys its data
// volume with it. The default is data-preserving: the PVC of a stateful add-on outlives the removal,
// so `addon remove postgres` stops the instance without destroying every attached app's database
// (ADR-0025/0031). The retained volume keeps its resource name, which is also the add-on name, so a
// re-install lands on exactly the same claim and the instance comes back with its data.
//
// What is retained is deliberately consistent: for Postgres the superuser Secret and the init
// ConfigMap are kept alongside the volume, because the official image only honours POSTGRES_PASSWORD
// during initdb. A reinstall over a retained volume never re-runs initdb, so a FRESHLY generated
// superuser password would not take — burrowd, the metrics exporter, and the backup Jobs would all
// be locked out of a database that is still there. Keeping the volume therefore means keeping the
// credential that opens it; the two are deleted together or kept together, never split.
func (a *Adapter) DeleteAddon(ctx context.Context, name string, deleteData bool) (controlplane.AddonRemoval, error) {
	removal := controlplane.AddonRemoval{Namespace: a.addonNamespace}
	deps := a.client.AppsV1().Deployments(a.addonNamespace)
	// The instance's own label says what it is. Reading it beats comparing the name against a
	// derived one: with an instance per environment (ADR-0067 §1) the Postgres instances are
	// burrow-postgres, burrow-postgres-staging, and so on, and a removal must recognize every one of
	// them without reconstructing which environment it came from.
	dep, err := deps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return removal, fmt.Errorf("kube: addon %q: %w", name, controlplane.ErrNotFound)
	} else if err != nil {
		return removal, fmt.Errorf("kube: reading addon %q: %w", name, err)
	}
	isPostgres := dep.Labels[addonLabel] == string(controlplane.AddonPostgres)
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
		// A Postgres instance owns its superuser Secret (ADR-0031) and its pg_stat_statements
		// init-script ConfigMap (ADR-0051), both named after the instance. They exist to open and
		// initialize THIS volume, so they go exactly when it does — and another environment's
		// instance keeps its own, which is the point of the credential being per-instance
		// (ADR-0067 §1).
		if isPostgres {
			_ = a.client.CoreV1().Secrets(a.addonNamespace).Delete(ctx, name, metav1.DeleteOptions{})
			_ = a.client.CoreV1().ConfigMaps(a.addonNamespace).Delete(ctx, postgresInitConfigMap(name), metav1.DeleteOptions{})
		}
	} else if _, err := pvcs.Get(ctx, name, metav1.GetOptions{}); err == nil {
		removal.RetainedDataVolume = name
	}

	// The backup volume ALWAYS survives, including a data-deleting removal (ADR-0032). Backups
	// outliving the database they were taken from is the whole point of taking them, and it is what
	// makes destroying the data survivable: the recorded backup rows in the control-plane database
	// still resolve to dumps that are still there. It is reported rather than silently left so the
	// operator knows the storage is still allocated and can reclaim it deliberately.
	if isPostgres {
		if _, err := pvcs.Get(ctx, controlplane.PostgresBackupVolume, metav1.GetOptions{}); err == nil {
			removal.RetainedBackupVolume = controlplane.PostgresBackupVolume
		}
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
			Name:      pvc.Name,
			Namespace: a.addonNamespace,
			Addon:     addon,
			Role:      role,
			Size:      addonVolumeSize(pvc),
			// A data claim keeps the add-on's resource name, so a reinstall creates over it and the
			// instance comes back with its data (ADR-0064 §1). The backup claim is mounted by the
			// backup/restore Jobs instead, so a reinstall does not adopt it.
			ReinstallAdopts: role == controlplane.AddonVolumeData,
			CreatedAt:       pvc.CreationTimestamp.Time,
		})
	}
	return out, nil
}

// addonVolumeOwner attributes a claim to the add-on it serves and says what it holds. The add-on
// label is authoritative. The backup claim is the one exception: it is created on the backup path
// (ensureBackupPVC) and claims created before that path carried the add-on label have only their
// name — which is safe to match on because it is a compiled-in constant
// (controlplane.PostgresBackupVolume), not a prefix guess about what a user might have named
// something.
func addonVolumeOwner(pvc *corev1.PersistentVolumeClaim) (controlplane.AddonType, string, bool) {
	if pvc.Name == controlplane.PostgresBackupVolume {
		return controlplane.AddonPostgres, controlplane.AddonVolumeBackup, true
	}
	if t := pvc.Labels[addonLabel]; t != "" {
		return controlplane.AddonType(t), controlplane.AddonVolumeData, true
	}
	return "", "", false
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
	case controlplane.AddonPostgres:
		// Run the shared Postgres instance with lean server settings suited to a low-traffic
		// metadata store (see LeanPostgresSettings), plus pg_stat_statements preloaded so the
		// always-on metrics exporter can read slow-query stats (ADR-0051). The official image's
		// entrypoint forwards args beginning with `-` to the postgres server, so these `-c key=value`
		// pairs tune it with no custom config file. shared_preload_libraries must be set at server
		// start (it cannot be loaded later), and the extension itself is created by the init script.
		return append(postgresTuningArgs(),
			"-c", "shared_preload_libraries=pg_stat_statements",
			"-c", "pg_stat_statements.track=top",
		)
	default:
		return nil
	}
}

// LeanPostgresSettings are the server settings Burrow runs its Postgres instances with — both the
// control-plane state database (ADR-0012, rendered into cmd/burrow/manifests/install.yaml.tmpl) and
// the shared Postgres add-on. Burrow's databases are low-traffic control-plane/metadata stores, so
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

// postgresTuningArgs renders LeanPostgresSettings as `postgres` container args (`-c key=value`).
func postgresTuningArgs() []string {
	args := make([]string, 0, len(LeanPostgresSettings)*2)
	for _, s := range LeanPostgresSettings {
		args = append(args, "-c", s)
	}
	return args
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
