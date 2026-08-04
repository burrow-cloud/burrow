// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

// deployPostgresCluster installs a Postgres add-on instance, which is a CloudNativePG `Cluster` and
// nothing else (ADR-0066 §1). Burrow creates one custom resource; CNPG composes the StatefulSet, the
// pods, the volumes and the services from it.
//
// WHAT THE REST OF BURROW SEES IS UNCHANGED, and that is the design. An environment's instance is
// reached at `<instance>.<addon-ns>.svc:5432`, opened as `burrow_admin` with the password in the
// Secret named after the instance, and dumped by the ADR-0032 backup Jobs. Attach, backup, restore
// and the app's DATABASE_URL are all expressed against that contract, which is ADR-0031's and is
// what ADR-0066 keeps: only who runs the server changed.
//
// THE OPERATOR IS A PREREQUISITE, NOT A FALLBACK. A cluster without CloudNativePG is refused here,
// by name, before any object is written — because the alternative is a dynamic client that does no
// discovery returning a 404 the caller cannot tell from a missing object, and an operator being told
// their add-on failed on `clusters.postgresql.cnpg.io not found` learns nothing they can act on.
func (a *Adapter) deployPostgresCluster(ctx context.Context, spec controlplane.AddonSpec, env, name string, labels map[string]string, archive *controlplane.ArchiveDestination) (controlplane.AddonInfo, error) {
	if err := a.requireCloudNativePG(ctx); err != nil {
		return controlplane.AddonInfo{}, err
	}
	if err := a.ensureCNPGSuperuserSecret(ctx, name, labels); err != nil {
		return controlplane.AddonInfo{}, err
	}
	// Everything the `Cluster` will REFERENCE is written first: the credential Secret the plugin's
	// sidecar reads, the `Stanza` naming the repository, and the schedule. A Cluster naming a Stanza
	// that does not exist yet is the operator racing Burrow into a failure that looks like the
	// plugin's (ADR-0066 §3).
	var warning string
	var scheduleCreated bool
	resolved := archive
	if archive != nil {
		w, created, err := a.ensurePgBackRestArchive(ctx, name, labels, archive)
		if err != nil {
			return controlplane.AddonInfo{}, err
		}
		scheduleCreated = created
		// A non-empty warning means the archive was NOT wired — the plugin is not on this cluster —
		// so the `Cluster` below is composed as if no destination had been resolved. It is one
		// decision read in two places, which is why the archive is nilled here rather than tested
		// again: an instance whose spec names a plugin that does not exist would fail to reconcile.
		if w != "" {
			warning, archive = w, nil
		}
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
		"spec": a.postgresClusterSpec(spec, env, name, labels, archive),
	}}
	if _, err := clusters.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return controlplane.AddonInfo{}, fmt.Errorf("kube: creating the CloudNativePG Cluster %q: %w", name, err)
		}
		if err := a.attachArchiveToExistingCluster(ctx, clusters, name, archive); err != nil {
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
		Warning:      warning,
		// Read back AFTER everything is written, from the objects themselves rather than from what
		// this function decided to write (issue #466). `resolved` is the destination as it was before
		// the archive was possibly nilled above, so a mismatch between the repository the install
		// resolved and the one the instance actually holds is visible instead of assumed away.
		Backups: a.describeInstanceBackups(ctx, name, env, resolved, scheduleCreated, warning),
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
		return fmt.Errorf("kube: the postgres add-on runs on CloudNativePG, and this cluster has no "+
			"CloudNativePG installed. Install the operator first with `burrow cluster postgres install`, "+
			"then install the add-on. That is an operator step run from a kubeconfig, not something the "+
			"agent can do: it installs cluster-scoped CustomResourceDefinitions and needs cluster-admin "+
			"(ADR-0066): %w", controlplane.ErrInvalid)
	case !found.Ready:
		return fmt.Errorf("kube: CloudNativePG's CustomResourceDefinitions are installed but no controller "+
			"is running, so a Cluster written now would be accepted and then reconciled by nothing. "+
			"Re-run `burrow cluster postgres install` to repair the install: %w", controlplane.ErrInvalid)
	}
	return nil
}

// ensureCNPGSuperuserSecret creates (idempotently) the per-instance superuser Secret a
// CNPG-backed instance uses, in the shape CNPG's declarative role management requires: type
// `kubernetes.io/basic-auth`, with `username` and `password`.
//
// It is the Secret name, and the `password` key, ADR-0031 specified, which is what lets the
// provisioner, the metrics path and the ADR-0032 backup Jobs reach the instance with no special
// case — they read `Data["password"]` from the Secret named after the instance. The added `username`
// key is what CNPG's `passwordSecret` reference requires, and it holds a role name, not a secret.
//
// An existing Secret is left untouched (a re-install keeps the running database's credential), but a
// Secret in the WRONG SHAPE is refused rather than worked around: a Secret's type is immutable, so
// an Opaque one — which is what a Burrow release that ran Postgres as a Deployment created, and what
// a data-keeping removal under that release deliberately left behind beside the volume — cannot be
// converted, and CNPG would reject the role reference and leave `burrow_admin` uncreated on a
// database that otherwise looks installed. Refusing there is also what keeps such a leftover from
// being quietly stood beside a fresh, empty `Cluster` that does not adopt it.
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
				"one was left by a Burrow release that ran the postgres add-on as its own Deployment; the data "+
				"volume it opens is still there and a new instance would not adopt it. Reclaim or delete that "+
				"volume and this Secret deliberately before installing (ADR-0064 §1): %w",
				a.addonNamespace, instance, corev1.SecretTypeBasicAuth, cnpgSecretUsernameKey,
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
func (a *Adapter) postgresClusterSpec(spec controlplane.AddonSpec, env, name string, labels map[string]string, archive *controlplane.ArchiveDestination) map[string]any {
	out := map[string]any{
		// One replica is ADR-0066 §1's default for the small self-hoster ADR-0031 was written for.
		// Under an operator it is a number rather than a constant of the design, which is the point:
		// raising it is configuration, not a rewrite.
		"instances": int64(1),
		"imageName": spec.Image,
		// The `postgres` superuser stays disabled (CNPG's own default). Burrow connects as
		// burrow_admin, declared below, so there is no second superuser credential in the cluster
		// that nothing uses and nothing rotates.
		"enableSuperuserAccess": false,
		"storage": map[string]any{
			"size": fmt.Sprintf("%dGi", spec.StorageGi),
		},
		// The same footprint every Burrow Postgres pod declares (postgresResources), so the add-on
		// instance and the control-plane database are sized by one decision. The pod runs CNPG's
		// instance manager as well as PostgreSQL — a few tens of megabytes — which the 320Mi limit
		// absorbs because the lean settings below keep PostgreSQL itself around 150MB.
		"resources": mustJSON(postgresResources()),
		"postgresql": map[string]any{
			// The lean tuning every Burrow Postgres server runs with (LeanPostgresSettings, which the
			// control-plane database takes as `-c` args). This server is not launched by Burrow, so the
			// settings go in the field CNPG reconciles them from — the same values, because the
			// resource limit above was chosen against them.
			"parameters": leanPostgresParameters(),
			// pg_stat_statements is preloaded here and created below, so slow-query statistics exist
			// for whatever scrapes them (ADR-0051) — CNPG's instance manager exports them itself, so
			// there is no exporter sidecar. It is a separate field from `parameters` because CNPG
			// manages the preload list itself and merges this into it. The pinned operand image
			// carries the module.
			"shared_preload_libraries": []any{"pg_stat_statements"},
		},
		"bootstrap": map[string]any{
			"initdb": map[string]any{
				// postInitSQL runs as a superuser against the `postgres` maintenance database — the
				// one the provisioner connects to — because the extension has to exist where the
				// queries run.
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
		// What CNPG puts on the resources it creates for this cluster. The annotations are what the
		// metrics add-on's vmagent discovers the instance by, whichever order the two were installed
		// in (ADR-0051); CNPG's instance manager exports on 9187 itself.
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
	// The pgBackRest plugin, when this instance archives (ADR-0066 §3).
	//
	// An instance with no registered destination gets NO entry at all rather than a disabled one, so
	// the `Cluster` written on a cluster with no object storage is byte-for-byte what it was before
	// this existed.
	if archive != nil {
		out["plugins"] = []any{pgBackRestPluginEntry(name)}
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

// leanPostgresParameters renders LeanPostgresSettings — the `key=value` list the control-plane
// database is started with — as the `spec.postgresql.parameters` map CNPG reconciles.
//
// It is derived from that list rather than restated, so the two cannot drift: the comment on
// LeanPostgresSettings already asks two places to be kept in step, and a third copied by hand is how
// that stops being true. A malformed entry is skipped rather than written as a parameter with no
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

// attachArchiveToExistingCluster wires the pgBackRest plugin into a `Cluster` that already exists,
// which is what a re-run of `addon install postgres` does after an object-storage destination has
// been registered.
//
// A `Cluster` Burrow wrote is not otherwise edited in place, and this is the deliberate exception
// rather than a softening of that rule. It PATCHES ONE PATH, `spec.plugins`, and never touches the
// instance count, the storage, the image, the roles or the services — because the destination is
// configuration that legitimately arrives after the database does, and the alternative offered to a
// user who registered object storage second is to destroy and rebuild the component holding every
// app's data. CloudNativePG supports adding a plugin to a live `Cluster`; the sidecar is injected on
// the next reconcile.
//
// It is a no-op when there is nothing to attach or the plugin is already there, so a plain re-install
// stays the read-only operation it has always been.
func (a *Adapter) attachArchiveToExistingCluster(ctx context.Context, clusters dynamic.ResourceInterface, name string, archive *controlplane.ArchiveDestination) error {
	if archive == nil {
		return nil
	}
	existing, found, err := a.getCNPGCluster(ctx, name)
	if err != nil {
		return err
	}
	if !found || cnpgClusterArchives(existing) {
		return nil
	}
	// A JSON merge patch REPLACES an array wholesale, and `spec.plugins` is a list somebody else may
	// also be in — CNPG-I is a plugin interface, not a Burrow one. So the existing entries are read
	// and Burrow's is APPENDED to them; a patch that carried only Burrow's entry would silently
	// uninstall every other plugin the operator had wired.
	plugins, _, err := unstructured.NestedSlice(existing.Object, "spec", "plugins")
	if err != nil {
		return fmt.Errorf("kube: reading the CloudNativePG Cluster %q's plugins: %w", name, err)
	}
	plugins = append(plugins, pgBackRestPluginEntry(name))
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"plugins": plugins}})
	if err != nil {
		return fmt.Errorf("kube: composing the plugin patch for the CloudNativePG Cluster %q: %w", name, err)
	}
	if _, err := clusters.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("kube: attaching the pgBackRest plugin to the CloudNativePG Cluster %q: %w", name, err)
	}
	return nil
}

// pgBackRestPluginEntry is the `spec.plugins` entry that makes an instance archive. It is one
// function so the entry the composition writes and the entry the patch appends cannot drift.
func pgBackRestPluginEntry(instance string) map[string]any {
	return map[string]any{
		"name":    PgBackRestPluginName,
		"enabled": true,
		// isWALArchiver is the load-bearing field: it is what makes PostgreSQL's archive_command hand
		// every segment to the plugin's sidecar. Without it a base backup would be a copy with no
		// write-ahead log behind it, and therefore no point-in-time recovery.
		"isWALArchiver": true,
		"parameters":    map[string]any{"stanzaRef": pgBackRestStanzaName(instance)},
	}
}
