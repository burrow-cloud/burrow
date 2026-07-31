// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/burrow-cloud/burrow/controlplane"
)

// cnpgClusterGVR is the resource `postgresql.cnpg.io/v1 Cluster` lives at. It is addressed through
// the dynamic client rather than a generated typed client because Burrow does not import CNPG's Go
// module: doing so would drag barman-cloud, the prometheus-operator APIs, ginkgo and gomega into a
// dependency graph this project keeps small deliberately, and ADR-0066 §3 declines the barman path
// on licensing grounds — acquiring it as a build dependency to spell a struct would be an odd way
// to get it back. The same reasoning already governs the placement translation (placement.go).
var cnpgClusterGVR = schema.GroupVersionResource{Group: CNPGAPIGroup, Version: "v1", Resource: "clusters"}

// cnpgClusterKind and cnpgClusterAPIVersion are the object's identity in the write itself.
const (
	cnpgClusterKind       = "Cluster"
	cnpgClusterAPIVersion = CNPGAPIGroup + "/v1"
)

// CNPGPostgresImage is the PostgreSQL operand image a Burrow-authored `Cluster` runs. Burrow ships
// no third-party bytes: this names an image the CLUSTER pulls from the publisher who built it.
//
// Three things are deliberate about which image this is.
//
//   - It is CloudNativePG's own operand image rather than `postgres:17-alpine`. CNPG's instance
//     manager runs as PID 1 inside it and the entrypoint is the operator's, so an arbitrary
//     PostgreSQL image is not a substitution CNPG supports.
//   - It is the MINIMAL variant. CNPG's standard operand images bundle barman-cloud, which shells
//     out to GPL-3.0 tooling; ADR-0066 §3 declines barman on exactly that ground, and its rejection
//     of the WAL-G plugin ("a plugin's licence is not its image's licence") is the record saying
//     this project names images and not just repositories. The minimal image carries PostgreSQL and
//     the instance manager and no backup tooling at all — which is also the right base for §3's
//     pgBackRest plugin, since a CNPG-I plugin ships its own sidecar rather than living in this
//     image.
//   - It is PostgreSQL 17, the major version ADR-0031's `postgres:17-alpine` already runs, so
//     choosing the mechanism is not also choosing a major-version jump.
//
// It is pinned to a patch release for the reason every other image in the catalog is: an install
// that happens twice should be the same install. It moves independently of CNPGVersion — the
// operator and the operand are separately released, and CNPG supports a range of operands per
// operator — so this is not derived from the pin.
const CNPGPostgresImage = "ghcr.io/cloudnative-pg/postgresql:17.10-minimal-trixie"

// WithDynamicClient wires the client custom resources are read and written through — today the
// CloudNativePG `Cluster` behind a Postgres add-on instance (ADR-0066 §1).
//
// It is separate from the typed clientset because it is separately OPTIONAL. An Adapter built with
// New (every unit test, and any embedder that has not wired one) has no dynamic client, and every
// custom-resource path then behaves exactly as it did before CNPG existed: no `Cluster` is found,
// nothing is created, and an add-on is its Deployment. NewFromConfig wires both, so burrowd always
// has it.
//
// Returns the Adapter for chaining.
func (a *Adapter) WithDynamicClient(d dynamic.Interface) *Adapter {
	a.dynamic = d
	return a
}

// cnpgClusters is the `Cluster` resource interface in the add-on namespace, or an error when no
// dynamic client is wired. Callers that must not fail on an unwired client check a.dynamic first —
// see getCNPGCluster, which is the read path and answers "there is no Cluster" rather than erroring.
func (a *Adapter) cnpgClusters() (dynamic.ResourceInterface, error) {
	if a.dynamic == nil {
		return nil, fmt.Errorf("kube: no dynamic client is wired, so a %s cannot be created: %w",
			cnpgClusterKind, controlplane.ErrInvalid)
	}
	return a.dynamic.Resource(cnpgClusterGVR).Namespace(a.addonNamespace), nil
}

// getCNPGCluster reads the `Cluster` named name in the add-on namespace. It reports (nil, false,
// nil) — "there is no such Cluster" — for every way of not having one, and reserves the error for a
// cluster that could not be read at all.
//
// THREE ABSENCES ARE ONE ANSWER HERE, and collapsing them is the point:
//
//   - No dynamic client is wired, so this build cannot address custom resources.
//   - The CRD is not served, because CloudNativePG is not installed. A dynamic client does no
//     discovery, so this surfaces as a 404 from the API server exactly as a missing object does.
//   - The CRD is served and there is no such object.
//
// A FORBIDDEN is folded in with them, and that one needs saying out loud: burrowd installed before
// this existed holds no grant on `postgresql.cnpg.io`, so on that cluster this read is refused. It
// is also a cluster on which Burrow cannot have CREATED a `Cluster`, so "there is none" is the true
// answer, and returning an error instead would make the failure observer degrade a sweep every
// minute on every cluster that has not been upgraded (ADR-0074's coverage record is supposed to
// mean something).
func (a *Adapter) getCNPGCluster(ctx context.Context, name string) (*unstructured.Unstructured, bool, error) {
	if a.dynamic == nil {
		return nil, false, nil
	}
	u, err := a.dynamic.Resource(cnpgClusterGVR).Namespace(a.addonNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("kube: reading the CloudNativePG Cluster %q: %w", name, err)
	}
	return u, true, nil
}

// cnpgClusterReady reports whether a `Cluster` has an instance serving.
//
// It reads `status.readyInstances` rather than `status.phase`. The phase is a human-facing string
// whose values are CNPG's to change, and it passes through several healthy-but-not-serving values
// on the way up; the ready count is a number with one meaning. Zero ready instances is the state
// ADR-0077 warns reads like a slow start when it is in fact a pod that will never schedule — the
// distinction is not made here, because this seam answers "is it serving" and the failure ledger is
// where "since when" is recorded (ADR-0074).
func cnpgClusterReady(u *unstructured.Unstructured) bool {
	ready, found, err := unstructured.NestedInt64(u.Object, "status", "readyInstances")
	return err == nil && found && ready > 0
}

// deployPostgresCluster installs a Postgres add-on instance as a CloudNativePG `Cluster` rather than
// a Deployment Burrow authors (ADR-0066 §1). Burrow creates one custom resource; CNPG composes the
// StatefulSet, the pods, the volumes and the services from it.
//
// WHAT THE REST OF BURROW SEES IS UNCHANGED, and that is the whole design of this slice. An
// environment's instance is still reached at `<instance>.<addon-ns>.svc:5432`, still opened as
// `burrow_admin` with the password in the Secret named after the instance, and still dumped by the
// ADR-0032 backup Jobs. Attach, backup, restore and the app's DATABASE_URL are untouched, so the
// mechanism is a fact about how the server is run rather than a second way to be a Postgres add-on.
//
// It REFUSES rather than converts (ADR-0066 §6). An ADR-0031 instance in this environment — its
// Deployment, or the data claim a data-preserving removal left behind — means there is Postgres data
// under the name this `Cluster` would take, and CNPG does not adopt it: the `Cluster` would come up
// empty beside it and the old data would sit there unreferenced. §6 makes migration an explicit,
// one-way sequence a user runs deliberately, and this is where that is enforced.
func (a *Adapter) deployPostgresCluster(ctx context.Context, spec controlplane.AddonSpec, env, name string, labels map[string]string) (controlplane.AddonInfo, error) {
	if spec.Type != controlplane.AddonPostgres {
		return controlplane.AddonInfo{}, fmt.Errorf("kube: the %s add-on has no CloudNativePG mechanism; only postgres does (ADR-0066 §1): %w",
			spec.Type, controlplane.ErrInvalid)
	}
	if err := a.requireCloudNativePG(ctx); err != nil {
		return controlplane.AddonInfo{}, err
	}
	if err := a.requireNoDeploymentBackedInstance(ctx, name, env); err != nil {
		return controlplane.AddonInfo{}, err
	}
	if err := a.ensureCNPGSuperuserSecret(ctx, name, labels); err != nil {
		return controlplane.AddonInfo{}, err
	}

	clusters, err := a.cnpgClusters()
	if err != nil {
		return controlplane.AddonInfo{}, err
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": cnpgClusterAPIVersion,
		"kind":       cnpgClusterKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": a.addonNamespace,
			"labels":    toStringMap(labels),
		},
		"spec": a.postgresClusterSpec(spec, env, name, labels),
	}}
	if _, err := clusters.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return controlplane.AddonInfo{}, fmt.Errorf("kube: creating the CloudNativePG Cluster %q: %w", name, err)
	}

	return controlplane.AddonInfo{
		Name:        name,
		Type:        spec.Type,
		Environment: env,
		Mode:        "installed",
		// The registry records the MECHANISM here, because that is what Backend has always meant:
		// which concrete implementation serves this instance. It needs no new column and no
		// migration, and it is what `addon list` shows an operator who asks what is running their
		// database.
		Backend:      controlplane.AddonBackendCloudNativePG,
		Image:        CNPGPostgresImage,
		Endpoint:     fmt.Sprintf("%s.%s.svc:%d", name, a.addonNamespace, spec.Port),
		Capabilities: spec.Capabilities,
	}, nil
}

// requireCloudNativePG refuses an install unless a CNPG controller is actually RUNNING, using the
// same detector `burrow cluster` reports from.
//
// Present and Ready are checked separately for the reason DetectCloudNativePG keeps them apart: CRDs
// are cluster-scoped and outlive the operator that installed them, so a cluster whose cnpg-system
// namespace was deleted still ACCEPTS this `Cluster` and then reconciles it with nothing. The write
// succeeds, the object sits there, and the add-on is a registry row with no database under it. That
// is the orphan-IngressClass failure on the component holding tenant data, and it is cheaper to
// refuse it here than to diagnose it later.
func (a *Adapter) requireCloudNativePG(ctx context.Context) error {
	found, err := DetectCloudNativePG(ctx, a.client)
	if err != nil {
		return fmt.Errorf("kube: checking for CloudNativePG: %w", err)
	}
	switch {
	case !found.Present:
		return fmt.Errorf("kube: CloudNativePG is not installed on this cluster, so the postgres add-on "+
			"cannot run on it. Install the operator first: `burrow cluster postgres install` (it needs "+
			"cluster-admin, so it is an operator step): %w", controlplane.ErrInvalid)
	case !found.Ready:
		return fmt.Errorf("kube: CloudNativePG's CustomResourceDefinitions are installed but no controller "+
			"is running, so a Cluster written now would be accepted and then reconciled by nothing. "+
			"Re-run `burrow cluster postgres install` to repair the install: %w", controlplane.ErrInvalid)
	}
	return nil
}

// requireNoDeploymentBackedInstance enforces ADR-0066 §6's one-way, explicit migration at the only
// point that can see both mechanisms: an environment whose Postgres instance is already an ADR-0031
// Deployment, or whose data claim a data-preserving removal deliberately kept (ADR-0064 §1), does
// not silently become a `Cluster`.
//
// The claim is checked as well as the Deployment because the dangerous case is the one where the
// workload is already gone. `addon remove postgres` keeps the volume by default precisely so a
// reinstall comes back with its data; a reinstall that came back as an empty `Cluster` beside that
// volume would look like a successful reinstall and be a total data loss from the user's side.
func (a *Adapter) requireNoDeploymentBackedInstance(ctx context.Context, name, env string) error {
	_, err := a.client.AppsV1().Deployments(a.addonNamespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("kube: environment %q already runs a Deployment-backed postgres instance (%s). "+
			"Moving it onto CloudNativePG is a deliberate, one-way sequence — back up, install the new "+
			"instance, restore, cut over — not something an install does to a database in place "+
			"(ADR-0066 §6): %w", env, name, controlplane.ErrInvalid)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: reading addon %q: %w", name, err)
	}
	_, err = a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("kube: environment %q has a retained Deployment-backed postgres volume (%s) that "+
			"an earlier removal deliberately kept. A CloudNativePG Cluster does not adopt it: it would come "+
			"up empty beside data that is still there. Reinstall the add-on as it was, or delete that claim "+
			"once you are certain (ADR-0064 §1, ADR-0066 §6): %w", env, name, controlplane.ErrInvalid)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: reading addon volume %q: %w", name, err)
	}
	return nil
}

// ensureCNPGSuperuserSecret creates (idempotently) the per-instance superuser Secret a
// CNPG-backed instance uses, in the shape CNPG's declarative role management requires: type
// `kubernetes.io/basic-auth`, with `username` and `password`.
//
// It is the SAME Secret name, and the same `password` key, that the ADR-0031 instance uses, which is
// what lets the provisioner, the metrics path and the ADR-0032 backup Jobs reach a CNPG-backed
// instance with no change at all — they read `Data["password"]` from the Secret named after the
// instance, and that is still exactly what is there. The added `username` key is what CNPG's
// `passwordSecret` reference requires, and it holds a role name, not a secret.
//
// An existing Secret is left untouched (a re-install keeps the running database's credential), but a
// Secret in the WRONG SHAPE is refused rather than worked around: a Secret's type is immutable, so
// an Opaque one left by an ADR-0031 install cannot be converted, and CNPG would reject the role
// reference and leave `burrow_admin` uncreated on a database that otherwise looks installed.
//
// The generated password is written only into the Secret. It is never inlined into the Cluster spec,
// returned, or logged (ADR-0031).
func (a *Adapter) ensureCNPGSuperuserSecret(ctx context.Context, instance string, labels map[string]string) error {
	secrets := a.client.CoreV1().Secrets(a.addonNamespace)
	existing, err := secrets.Get(ctx, instance, metav1.GetOptions{})
	switch {
	case err == nil:
		if existing.Type != corev1.SecretTypeBasicAuth || len(existing.Data[cnpgSecretUsernameKey]) == 0 {
			return fmt.Errorf("kube: the superuser secret %s/%s exists but is not in the shape CloudNativePG's "+
				"role management requires (type %s with a %q key). A Secret's type cannot be changed, so this "+
				"one belongs to a Deployment-backed instance and the two mechanisms must not share it "+
				"(ADR-0066 §6): %w", a.addonNamespace, instance, corev1.SecretTypeBasicAuth, cnpgSecretUsernameKey,
				controlplane.ErrInvalid)
		}
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("kube: reading postgres superuser secret %q: %w", instance, err)
	}

	pw, err := generatePassword()
	if err != nil {
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: instance, Namespace: a.addonNamespace, Labels: labels},
		Type:       corev1.SecretTypeBasicAuth,
		Data: map[string][]byte{
			cnpgSecretUsernameKey: []byte(PostgresSuperuser),
			PostgresPasswordKey:   []byte(pw),
		},
	}
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		// The error names the Secret only — never the generated value.
		return fmt.Errorf("kube: creating postgres superuser secret %q: %w", instance, err)
	}
	return nil
}

// cnpgSecretUsernameKey is the key CNPG's `passwordSecret` reference reads the role name from. The
// CRD states the requirement outright: the secret must follow the `kubernetes.io/basic-auth` format
// and contain both a `username` and a `password` field.
const cnpgSecretUsernameKey = "username"

// postgresClusterSpec composes the `Cluster` spec for one environment's instance.
//
// It is written as a map rather than a struct for the reason cnpgClusterGVR gives: Burrow does not
// import CNPG's types. Every key below is a field of the pinned release's CRD
// (cnpg_placement_schema.json records the placement subtree of the same artifact), and a key that
// is not is PRUNED by the API server silently — which is why nothing here is invented and why the
// placement fragment is produced by the translation ADR-0077 built rather than spelled again.
func (a *Adapter) postgresClusterSpec(spec controlplane.AddonSpec, env, name string, labels map[string]string) map[string]any {
	out := map[string]any{
		// One replica is ADR-0066 §1's default for the small self-hoster ADR-0031 was written for.
		// Under an operator it is a number rather than a constant of the design, which is the point:
		// raising it is configuration, not a rewrite.
		"instances": int64(1),
		"imageName": CNPGPostgresImage,
		// The `postgres` superuser stays disabled (CNPG's own default). Burrow connects as
		// burrow_admin, declared below, so there is no second superuser credential in the cluster
		// that nothing uses and nothing rotates.
		"enableSuperuserAccess": false,
		"storage": map[string]any{
			"size": fmt.Sprintf("%dGi", spec.StorageGi),
		},
		// The same footprint the Deployment-backed instance declares, so choosing the mechanism does
		// not quietly change what the database costs on a small VPS. The pod runs one more process
		// than the ADR-0031 pod did — CNPG's instance manager, a few tens of megabytes — which the
		// 320Mi limit absorbs because the lean settings below keep PostgreSQL itself around 150MB.
		"resources": mustJSON(postgresResources()),
		"postgresql": map[string]any{
			// The SAME lean tuning the Deployment-backed instance is started with (LeanPostgresSettings,
			// passed there as `-c` args). Under an operator the server is not launched by Burrow, so the
			// settings move to the field CNPG reconciles them from — but they must be the same
			// settings, or a mechanism swap is silently also a retuning, and the resource limit above
			// was chosen against these values.
			"parameters": leanPostgresParameters(),
			// pg_stat_statements is preloaded here and created below, so slow-query statistics exist
			// for whatever scrapes them (ADR-0051) — under this mechanism that is CNPG's own metrics
			// exporter rather than a sidecar. It is a separate field from `parameters` because CNPG
			// manages the preload list itself and merges this into it. The pinned operand image
			// carries the module.
			"shared_preload_libraries": []any{"pg_stat_statements"},
		},
		"bootstrap": map[string]any{
			"initdb": map[string]any{
				// postInitSQL runs as a superuser against the `postgres` maintenance database — the
				// one the provisioner connects to and the one the ADR-0031 init script targeted, for
				// the same reason: the extension has to exist where the queries run.
				"postInitSQL": []any{"CREATE EXTENSION IF NOT EXISTS pg_stat_statements"},
			},
		},
		"managed": map[string]any{
			// burrow_admin is declared to the operator instead of being created by hand after the
			// database comes up. CNPG reconciles it — so it survives a failover, a restore and a
			// re-created instance, none of which would re-run a one-off bootstrap step — and the
			// password comes from the Secret Burrow already owns.
			"roles": []any{map[string]any{
				"name":           PostgresSuperuser,
				"ensure":         "present",
				"login":          true,
				"superuser":      true,
				"passwordSecret": map[string]any{"name": name},
			}},
			// THE SERVICE NAME IS THE COMPATIBILITY SEAM. Every consumer of a Postgres instance —
			// the provisioner's admin connection, the backup and restore Jobs, and the DATABASE_URL
			// written into an app's Secret — resolves `<instance>.<addon-ns>.svc:5432`. CNPG's own
			// services are `<cluster>-rw`/`-ro`/`-r`, so an additional managed service carries that
			// name onto the primary.
			//
			// It is declared to CNPG rather than created by Burrow beside the Cluster, and that is
			// deliberate: the selector that finds the primary is CNPG's, it changes between releases
			// (the `role` label is deprecated in favour of `cnpg.io/instanceRole`), and a Service
			// Burrow wrote with a copy of it would keep pointing at nothing after an upgrade. Asking
			// for `selectorType: rw` states the intent and lets the controller spell it. It also
			// means the service is torn down with the Cluster rather than left behind.
			"services": map[string]any{
				"additional": []any{map[string]any{
					"selectorType":   "rw",
					"updateStrategy": "patch",
					"serviceTemplate": map[string]any{
						"metadata": map[string]any{
							"name":   name,
							"labels": toStringMap(labels),
						},
						"spec": map[string]any{
							"ports": []any{map[string]any{
								"name":       "postgres",
								"port":       int64(spec.Port),
								"targetPort": int64(spec.Port),
								"protocol":   string(corev1.ProtocolTCP),
							}},
						},
					},
				}},
			},
		},
		// What CNPG puts on the resources it creates for this cluster. The annotations are the same
		// ones the ADR-0031 pod carries so the metrics add-on's vmagent discovers the instance
		// wherever it was installed (ADR-0051) — CNPG's instance manager already exports on 9187, so
		// there is no exporter sidecar under this mechanism.
		//
		// THE `managed-by` LABEL IS DELIBERATELY ABSENT. AddonVolumes selects Burrow's add-on claims
		// by it and then attributes an unroled claim as a DATA claim a reinstall adopts (ADR-0064
		// §1). CNPG's claims are the operator's, named `<cluster>-1`, and a reinstall does not adopt
		// them — so inheriting the label would make the volume listing state something false about
		// what a reinstall would do with them. The descriptive labels are inherited; the selectable
		// one is not.
		"inheritedMetadata": map[string]any{
			"labels": map[string]any{
				addonLabel:    string(spec.Type),
				addonEnvLabel: env,
			},
			"annotations": map[string]any{
				"prometheus.io/scrape": "true",
				"prometheus.io/port":   fmt.Sprint(postgresExporterPort),
				"prometheus.io/path":   "/metrics",
			},
		},
	}
	// The operator's placement policy for pods Burrow causes to exist but does not author
	// (ADR-0077 §2). It merges `affinity` and `topologySpreadConstraints` and NOTHING else when
	// nothing is wired, so an unwired adapter writes byte-for-byte the object above (ADR-0073 §4).
	// It cannot conflict with a key set here: this spec sets neither.
	for k, v := range a.cnpgClusterPlacement() {
		out[k] = v
	}
	return out
}

// leanPostgresParameters renders LeanPostgresSettings — the `key=value` list the Deployment-backed
// instance is started with — as the `spec.postgresql.parameters` map CNPG reconciles.
//
// It is derived from that list rather than restated, so the two mechanisms cannot drift: the comment
// on LeanPostgresSettings already asks two places to be kept in step, and a third copied by hand is
// how that stops being true. A malformed entry is skipped rather than written as a parameter with no
// value, which CNPG would reject and which would make an add-on install fail on a typo in a tuning
// constant.
func leanPostgresParameters() map[string]any {
	out := make(map[string]any, len(LeanPostgresSettings))
	for _, s := range LeanPostgresSettings {
		key, value, ok := strings.Cut(s, "=")
		if !ok || key == "" {
			continue
		}
		out[key] = value
	}
	return out
}

// toStringMap copies a label map into the `map[string]any` an unstructured object holds. Labels are
// strings on both sides; the conversion exists because unstructured refuses a typed map.
func toStringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mustJSON renders a Kubernetes value as the generic JSON shape an unstructured object holds, using
// the type's own marshalling so every field is spelled by k8s.io/api rather than by hand.
//
// It falls back to an empty object rather than panicking: the only values passed to it are
// compiled-in constants that cannot fail to marshal, and a control plane that dies at an add-on
// install because a resource quantity would not serialise is a worse answer than one that writes a
// Cluster without a resources block.
func mustJSON(v any) any {
	out, err := asJSON(v)
	if err != nil {
		return map[string]any{}
	}
	return out
}
