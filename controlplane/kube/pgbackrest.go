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
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane"
)

// This file is ADR-0066 §3: the base-backup and write-ahead-log path is pgBackRest, reached through a
// CNPG-I plugin, and NOT barman.
//
// The licensing is what decided the shape and is worth restating where the identifier lives. The
// better-known plugin shells out to barman, which is GPL-3.0; this one drives pgBackRest, which is
// MIT, and is itself Apache-2.0. Burrow ships neither — it points the cluster at the publisher's own
// release manifest and the cluster pulls the publisher's own images, which is the same posture as the
// CloudNativePG operator itself.
//
// WHAT BURROW WRITES, AND WHAT IT DOES NOT. Burrow writes one `Stanza` per environment, adds one
// entry to the `Cluster`'s plugin list, and creates `Backup` and `ScheduledBackup` objects. It runs
// no backup tool, constructs no Job, and handles no superuser credential on this path: the plugin's
// sidecar runs beside PostgreSQL, archives the write-ahead log, and takes the base backups. That is
// the whole of ADR-0066 §2's argument — backup correctness stops being this project's to get wrong.
//
// The plugin is EXPERIMENTAL, which is its maintainers' word. Nothing here treats it as more than
// that: an install with no object-storage provider registered gets exactly the `Cluster` it got
// before, with no plugin, no sidecar and no archiving, and physical backups of such an instance are
// refused by name rather than attempted.

const (
	// PgBackRestPluginName is the plugin's CNPG-I name — the string a `Cluster`, a `Backup` and a
	// `ScheduledBackup` all name it by — and also its API group, which is where its `Stanza` lives.
	// The two being the same string is the plugin's choice, not a coincidence this code relies on;
	// PgBackRestAPIGroup exists so a reader of either use is not guessing.
	PgBackRestPluginName = "pgbackrest.dalibo.com"
	// PgBackRestAPIGroup is the API group the plugin's own custom resources live under. Its presence
	// in API-group discovery means the plugin's CRDs are installed — the same no-RBAC signal
	// CloudNativePG and cert-manager are detected by.
	PgBackRestAPIGroup = PgBackRestPluginName
	// PgBackRestVersion is the plugin release Burrow targets, applied from the publisher's own tag.
	PgBackRestVersion = "0.0.3"
	// PgBackRestControllerDeployment and PgBackRestNamespace are where the release manifest puts the
	// plugin's controller: alongside the CloudNativePG operator, because the operator connects to it
	// over a Service in that namespace.
	PgBackRestControllerDeployment = "pgbackrest-controller"
	PgBackRestNamespace            = CNPGNamespace
	// pgBackRestControllerSelector matches that Deployment by the label its manifest carries, in any
	// namespace — the same posture as the CloudNativePG and ingress probes, which find a controller
	// wherever it was installed rather than only where Burrow would have put it.
	pgBackRestControllerSelector = "app=" + PgBackRestControllerDeployment
)

// PgBackRestManifestURL is the release artifact for a plugin version: the CRDs, the RBAC, the
// controller Deployment and its cert-manager certificates in one document, at the publisher's own
// tag. Burrow ships no third-party bytes; it applies what the publisher serves.
func PgBackRestManifestURL(version string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/dalibo/cnpg-plugin-pgbackrest/v%s/manifest.yaml", version)
}

// DetectPgBackRest reports the pgBackRest plugin's situation: whether its CRDs are served and whether
// its controller is actually running.
//
// Present and Ready are separate for DetectCloudNativePG's reason, which is sharper here. A CRD is
// cluster-scoped and outlives the controller that installed it, and a `Stanza` written against a
// served CRD with no controller behind it is accepted and then reconciled by nothing — so the
// instance comes up, `archive_command` hands write-ahead log to a sidecar that was never injected,
// and the first sign is a database that will not archive.
//
// NO VERSION IS REPORTED, unlike CloudNativePG's detector, and the absence is deliberate rather than
// an omission. The plugin's release artifact does not carry its own version anywhere Burrow can read
// back — the controller image tag in a tagged manifest has been seen to lag the tag it was published
// under — so a version read off the running Deployment would be a claim Burrow cannot stand behind
// about the component holding the backups. What Burrow targets is a constant; what is running is
// reported as present or not.
//
// It needs no new RBAC: API-group discovery needs none, and the cluster-wide apps/deployments
// get/list the capability ClusterRole already holds covers the rest.
func DetectPgBackRest(ctx context.Context, client kubernetes.Interface) (controlplane.PgBackRestCapability, error) {
	out := controlplane.PgBackRestCapability{Pinned: PgBackRestVersion}

	groups, err := client.Discovery().ServerGroups()
	if err != nil {
		return controlplane.PgBackRestCapability{}, fmt.Errorf("kube: discovering API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == PgBackRestAPIGroup {
			out.Present = true
			break
		}
	}

	deps, err := client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: pgBackRestControllerSelector,
	})
	if err != nil {
		return controlplane.PgBackRestCapability{}, fmt.Errorf("kube: listing pgbackrest plugin controller deployments: %w", err)
	}
	for i := range deps.Items {
		if deps.Items[i].Status.ReadyReplicas > 0 {
			out.Ready = true
		}
	}
	return out, nil
}

// pgBackRestStanzaGVR is where the plugin's `Stanza` lives. Like the `Cluster` it is addressed
// through the dynamic client rather than a generated typed client, for the reason on cnpgClusterGVR:
// this repository does not import CloudNativePG's Go module, and it is not about to import a
// plugin's either to spell a struct.
var pgBackRestStanzaGVR = schema.GroupVersionResource{Group: PgBackRestAPIGroup, Version: "v1", Resource: "stanzas"}

const (
	pgBackRestStanzaKind       = "Stanza"
	pgBackRestStanzaAPIVersion = PgBackRestAPIGroup + "/v1"
)

// pgBackRestSecretName is the Secret in the add-on namespace holding one instance's object-storage
// credential pair, for the plugin's sidecar to read by reference.
//
// It is per INSTANCE, not one shared Secret, so an environment's sidecar is given the credential for
// its own repository and no listing of Secrets makes another environment's reachable by accident
// (ADR-0067 §1). It is a different Secret from the one a backup Job mounts (backupCredSecretName),
// which is per Job and deleted with it: this one is as long-lived as the instance, because the
// write-ahead-log path needs it continuously and not only while a backup is running.
func pgBackRestSecretName(instance string) string { return instance + "-pgbackrest" }

// pgBackRestStanzaName is the `Stanza` object for an instance, and also the pgBackRest stanza name
// inside it. One per instance is not Burrow's convention but pgBackRest's own: a stanza is the
// configuration of one PostgreSQL cluster, and the plugin's documentation states outright that there
// are as many `Stanza` objects as `Cluster` objects.
func pgBackRestStanzaName(instance string) string { return instance }

// pgBackRestScheduledBackupName is the `ScheduledBackup` for an instance. It is suffixed rather than
// sharing the instance's name so `kubectl get scheduledbackups` reads as a list of schedules rather
// than of instances.
func pgBackRestScheduledBackupName(instance string) string { return instance + "-schedule" }

// pgBackRestSchedule is when an instance's base backup is taken, in CloudNativePG's six-field cron
// (the leading field is SECONDS, which is the difference from crontab and the mistake worth not
// making): 02:00 every day, in the operator's timezone.
//
// It is a constant rather than configuration in this slice, and that is a smaller claim than it
// looks. What a schedule has to be is regular and off-peak; which hour it is matters far less than
// that write-ahead-log archiving is continuous underneath it, which it is — the recovery floor is the
// archive window, not the interval between base backups. Making the hour configurable is an
// ADR-0068 occupant and is not one this needed to wait for.
const pgBackRestSchedule = "0 0 2 * * *"

// pgBackRestArchiveEndpoint renders an object-storage endpoint as pgBackRest's `repo1-s3-endpoint`:
// a host, optionally with a port, and no scheme.
//
// PGBACKREST ONLY SPEAKS TLS TO S3. There is no plaintext option — `repo1-storage-verify-tls=n`
// disables certificate verification and not TLS itself — so an `http://` endpoint is REFUSED here
// rather than silently upgraded. Silently upgrading it would produce an instance that comes up, an
// archive that never arrives, and a first symptom of `walArchivingFailing` an hour later; refusing at
// the point of configuration says which endpoint and why.
func pgBackRestArchiveEndpoint(endpoint string) (string, error) {
	e := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	switch {
	case strings.HasPrefix(e, "https://"):
		return strings.TrimPrefix(e, "https://"), nil
	case strings.HasPrefix(e, "http://"):
		return "", fmt.Errorf("kube: the object-storage endpoint %q is plaintext http, and pgBackRest reaches "+
			"an S3 repository over TLS only, so this instance cannot archive to it. Register the destination "+
			"with its https endpoint: %w", endpoint, controlplane.ErrInvalid)
	case e == "":
		return "", fmt.Errorf("kube: the object-storage destination has no endpoint: %w", controlplane.ErrInvalid)
	default:
		// Already a bare host. The provider registry requires a URL, so this is only reachable from a
		// caller that built the destination by hand; accept it rather than inventing a scheme to strip.
		return e, nil
	}
}

// pgBackRestRegion is the region the repository's requests are signed for. pgBackRest requires one
// and the CRD requires it non-empty, while a vendor with no meaningful region lets a provider be
// registered without it — so the same fallback the S3 world uses everywhere else stands in, rather
// than an install failing on a field neither the user nor the vendor cares about.
func pgBackRestRegion(region string) string {
	if strings.TrimSpace(region) == "" {
		return "us-east-1"
	}
	return region
}

// stanzaSpec composes the `Stanza` for one instance's repository.
//
// RETENTION IS BURROW'S WINDOW AND NOTHING ELSE'S. `fullType: time` with `full: <days>` is pgBackRest
// expiring full backups older than a number of DAYS, and the number handed to it is the retention
// declared on the object-storage provider — the same one ADR-0063 §3 refuses a contradicting bucket
// lifecycle rule against. So there is one window, written into both places, rather than a pgBackRest
// retention and a Burrow retention that agree until they do not.
//
// `archive` retention is deliberately NOT set. pgBackRest's default expires write-ahead log only in
// step with the full backups that need it; setting an archive count expires segments more
// aggressively and destroys point-in-time recovery within a window whose backups still list fine —
// which is precisely the failure ADR-0063 §3 exists to prevent, arriving from inside the repository
// instead of from the bucket's lifecycle rules.
func stanzaSpec(instance, endpoint string, archive *controlplane.ArchiveDestination) map[string]any {
	repo := map[string]any{
		"bucket":   archive.Config.Bucket,
		"endpoint": endpoint,
		"region":   pgBackRestRegion(archive.Config.Region),
		"repoPath": archive.RepoPath,
		// Path-style addressing, matching the adapter Burrow's own probes and logical backups use, so
		// a destination verified at `provider add` is addressed the same way by the component that
		// archives to it. Virtual-host style also requires a bucket name that is a valid DNS label,
		// which the registry does not enforce.
		"uriStyle": "path",
		// Verify the endpoint's certificate. It is the default pgBackRest would take anyway; it is
		// written because a repository holding every backup is not the place to inherit a default.
		"verifyTLS": true,
		"secretRef": map[string]any{
			"accessKeyId":     map[string]any{"name": pgBackRestSecretName(instance), "key": objectStoreAccessKeyIDFile},
			"secretAccessKey": map[string]any{"name": pgBackRestSecretName(instance), "key": objectStoreSecretAccessKeyFile},
		},
	}
	if days := archive.RetentionDays(); days > 0 {
		repo["retentionPolicy"] = map[string]any{
			"full":     int64(days),
			"fullType": "time",
		}
	}
	return map[string]any{
		"stanzaConfiguration": map[string]any{
			"name":           pgBackRestStanzaName(instance),
			"s3Repositories": []any{repo},
		},
	}
}

// ensurePgBackRestCredentials writes the destination credential PAIR into a Secret in the add-on
// namespace that the plugin's sidecar reads by reference (ADR-0063 §1, ADR-0057 §4's shape).
//
// The values go straight into the Secret's data and nowhere else: they are never a field of the
// `Stanza`, never an environment variable in the `Cluster`, never an argv, and never an error —
// a write failure names the Secret only. The `Stanza` carries a REFERENCE to two keys in it, which
// is what the plugin's own CRD asks for and is why the credential does not have to travel through
// the custom resource to reach pgBackRest.
//
// An existing Secret is UPDATED rather than left alone, which is the opposite of what the superuser
// Secret does and is right for the opposite reason: the superuser password is the database's own and
// re-generating it would lock Burrow out, while this one is a copy of a credential the registry owns,
// and a rotated key that never reached the sidecar is an archive that stops silently.
func (a *Adapter) ensurePgBackRestCredentials(ctx context.Context, instance string, labels map[string]string, cred controlplane.ObjectStoreCredential) error {
	name := pgBackRestSecretName(instance)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.addonNamespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			objectStoreAccessKeyIDFile:     []byte(cred.AccessKeyID),
			objectStoreSecretAccessKeyFile: []byte(cred.SecretAccessKey),
		},
	}
	secrets := a.client.CoreV1().Secrets(a.addonNamespace)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("kube: writing archive destination credentials %s/%s: %w", a.addonNamespace, name, err)
		}
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("kube: updating archive destination credentials %s/%s: %w", a.addonNamespace, name, err)
		}
	}
	return nil
}

// ensurePgBackRestArchive puts everything an instance needs to archive in place, BEFORE the `Cluster`
// that references it is written: the credential Secret, the `Stanza` naming the repository, and the
// `ScheduledBackup` that takes the daily base backup.
//
// The ordering is the same one ensureBackupCredentials keeps and for the same reason: a pod whose
// referenced Secret does not exist yet reports a configuration error the kubelet holds it in, so
// creating the Secret after the workload races the operator into a failure that looks like a bug in
// the plugin. Here the `Cluster` is written by the caller afterwards, so everything it names exists
// before the operator reads it.
//
// It is IDEMPOTENT on the `Stanza` and the `ScheduledBackup`: an already-existing object is left as
// it is. Re-running an install to wire a destination registered later is the case that matters, and
// it is the credential — the one thing that legitimately changes — that is updated rather than
// skipped.
func (a *Adapter) ensurePgBackRestArchive(ctx context.Context, instance string, labels map[string]string, archive *controlplane.ArchiveDestination) error {
	endpoint, err := pgBackRestArchiveEndpoint(archive.Config.Endpoint)
	if err != nil {
		return err
	}
	if err := a.requirePgBackRest(ctx); err != nil {
		return err
	}
	if err := a.ensurePgBackRestCredentials(ctx, instance, labels, archive.Credential); err != nil {
		return err
	}

	stanzas, err := a.pgBackRestStanzas()
	if err != nil {
		return err
	}
	stanza := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": pgBackRestStanzaAPIVersion,
		"kind":       pgBackRestStanzaKind,
		"metadata": map[string]any{
			"name":      pgBackRestStanzaName(instance),
			"namespace": a.addonNamespace,
			"labels":    toStringMap(labels),
		},
		"spec": stanzaSpec(instance, endpoint, archive),
	}}
	if _, err := stanzas.Create(ctx, stanza, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating the pgBackRest Stanza %q: %w", pgBackRestStanzaName(instance), err)
	}
	return a.ensureScheduledBackup(ctx, instance, labels)
}

// ensureScheduledBackup creates the `ScheduledBackup` that takes an instance's base backup on a
// schedule (ADR-0066 §2). It is a custom resource like every other object on this path: Burrow does
// not run a cron, hold a timer, or wake up to take a backup.
//
// `backupOwnerReference: cluster` makes each `Backup` the schedule produces owned by the `Cluster`,
// so Kubernetes garbage-collects the objects when the instance goes. That deletes the RECORDS in the
// cluster and nothing in the repository — the backups themselves are in the object store and outlive
// the instance, which is the point of them, and Burrow's own rows remain the index either way.
//
// `immediate` is left false. A base backup fired at instance-creation would race the plugin's own
// stanza initialisation, which happens when the first write-ahead-log segment is archived, and a
// failed first backup on a brand-new database is a bad first impression of a mechanism that is
// working.
func (a *Adapter) ensureScheduledBackup(ctx context.Context, instance string, labels map[string]string) error {
	scheduled, err := a.cnpgScheduledBackups()
	if err != nil {
		return err
	}
	name := pgBackRestScheduledBackupName(instance)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": cnpgClusterAPIVersion,
		"kind":       cnpgScheduledBackupKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": a.addonNamespace,
			"labels":    toStringMap(labels),
		},
		"spec": map[string]any{
			"schedule":             pgBackRestSchedule,
			"immediate":            false,
			"backupOwnerReference": "cluster",
			"method":               cnpgBackupMethodPlugin,
			"pluginConfiguration":  map[string]any{"name": PgBackRestPluginName},
			"cluster":              map[string]any{"name": instance},
		},
	}}
	if _, err := scheduled.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating the ScheduledBackup %q: %w", name, err)
	}
	return nil
}

// requirePgBackRest refuses to wire archiving unless the plugin's controller is actually RUNNING,
// using the same detector `burrow cluster` reports from.
//
// Present and Ready are checked separately for requireCloudNativePG's reason: the CRDs are
// cluster-scoped and outlive the controller, so a `Stanza` written against a served CRD with nothing
// behind it is accepted and reconciled by nothing — and the instance then comes up with an
// `archive_command` handing segments to a sidecar that was never injected. The database works and
// the backups do not, which is the failure this whole record exists to stop.
func (a *Adapter) requirePgBackRest(ctx context.Context) error {
	found, err := DetectPgBackRest(ctx, a.client)
	if err != nil {
		return fmt.Errorf("kube: checking for the pgBackRest plugin: %w", err)
	}
	switch {
	case !found.Present:
		return fmt.Errorf("kube: an object-storage destination is registered, so this instance would archive "+
			"its write-ahead log and take its base backups with the pgBackRest CloudNativePG plugin, and this "+
			"cluster has no such plugin installed. Install it with `burrow cluster postgres install`, then "+
			"install the add-on. That is an operator step run from a kubeconfig, not something the agent can "+
			"do: it installs cluster-scoped CustomResourceDefinitions and needs cluster-admin (ADR-0066 §3): %w",
			controlplane.ErrInvalid)
	case !found.Ready:
		return fmt.Errorf("kube: the pgBackRest plugin's CustomResourceDefinitions are installed but no "+
			"controller is running, so a Stanza written now would be accepted and then reconciled by nothing, "+
			"and the instance would come up handing its write-ahead log to a sidecar that was never injected. "+
			"Re-run `burrow cluster postgres install` to repair the install: %w", controlplane.ErrInvalid)
	}
	return nil
}

// pgBackRestStanzas is the `Stanza` resource interface in the add-on namespace, or an error when no
// dynamic client is wired.
func (a *Adapter) pgBackRestStanzas() (dynamic.ResourceInterface, error) {
	if a.dynamic == nil {
		return nil, fmt.Errorf("kube: no dynamic client is wired, so a %s cannot be created: %w",
			pgBackRestStanzaKind, controlplane.ErrInvalid)
	}
	return a.dynamic.Resource(pgBackRestStanzaGVR).Namespace(a.addonNamespace), nil
}

// deletePgBackRestArchive removes what ensurePgBackRestArchive created for an instance: the
// schedule, the stanza, and the credential.
//
// IT DELETES NO BACKUP. The repository is in the object store and every base backup and archived
// segment in it survives this untouched, which is ADR-0064's contract read onto the durable copy:
// removing an add-on does not destroy its backups, and the one destination that is genuinely outside
// the cluster is the last place to make an exception. What goes is the configuration that would
// otherwise keep a schedule firing at an instance that is not there.
//
// Each delete tolerates an absent object: an instance installed before archiving existed has none of
// these, and a removal must not fail on the absence of something it was about to delete.
func (a *Adapter) deletePgBackRestArchive(ctx context.Context, instance string) error {
	if a.dynamic != nil {
		if scheduled, err := a.cnpgScheduledBackups(); err == nil {
			name := pgBackRestScheduledBackupName(instance)
			if err := scheduled.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
				return fmt.Errorf("kube: deleting the ScheduledBackup %q: %w", name, err)
			}
		}
		if stanzas, err := a.pgBackRestStanzas(); err == nil {
			name := pgBackRestStanzaName(instance)
			if err := stanzas.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
				return fmt.Errorf("kube: deleting the pgBackRest Stanza %q: %w", name, err)
			}
		}
	}
	name := pgBackRestSecretName(instance)
	if err := a.client.CoreV1().Secrets(a.addonNamespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting archive destination credentials %s/%s: %w", a.addonNamespace, name, err)
	}
	return nil
}
