// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/burrow-cloud/burrow/controlplane"
)

// This file is ADR-0066 §4's restore orchestration on the cluster side: recovery bootstraps a NEW
// `Cluster` from the pgBackRest repository, so a restore has to create it, wait for it, and decide
// what happens to what was there.
//
// THE RECOVERED INSTANCE COMES UP UNDER THE INSTANCE'S OWN NAME, and that decision is the shape of
// everything below. CloudNativePG cannot recover in place; but `AddonInstanceName` is not a detail a
// restore is free to move. It names the Service every DATABASE_URL resolves, the Secret the
// provisioner reads the superuser password from, the `Cluster` a removal deletes and the instance a
// physical backup is taken of — so a recovery that left a differently-named `Cluster` behind would
// leave every one of those pointing at an instance that is no longer the live one. The alternative to
// keeping the name is threading a second name through all of them, which is a larger and more fragile
// change than replacing the object.
//
// WHICH MEANS THE PRE-RESTORE STATE IS DESTROYED, and that is stated rather than hidden. CloudNativePG
// composes a claim called `<instance>-1` from the `Cluster`, and its own classifier reattaches a new
// `Cluster` of the same name to a claim left lying there rather than initializing from the recovery —
// so the old claim cannot be kept and the name reused. What makes that acceptable is the ORDERING the
// engine holds around this call: a physical backup of the current state goes into the repository, and
// is read back out of it, before this seam is entered at all (ADR-0064 §5's rule, applied to the one
// operation that destroys a database on purpose). The way back from a restore to the wrong point is a
// restore to the backup taken on the way in.

const (
	// cnpgRecoverySourceName is the `externalClusters` entry the recovery bootstrap reads from. It is
	// a name local to the `Cluster` object — CloudNativePG resolves `bootstrap.recovery.source`
	// against it — so it is a constant rather than derived: nothing outside this object ever refers
	// to it, and a derived name would only invite a reader to look for it somewhere else.
	cnpgRecoverySourceName = "burrow-pgbackrest-repository"
	// cnpgRecoveryPoll is the interval between reads of the recovering `Cluster`'s status.
	cnpgRecoveryPoll = 5 * time.Second
	// cnpgRecoveryTimeout is how long the seam waits for the recovered instance to serve before it
	// gives up WAITING — not before it gives up on the restore. A recovery restores a base backup and
	// replays write-ahead log, both proportional to the data, so a fixed deadline can only ever bound
	// the wait: the `Cluster` is left in place on a timeout and the message says to watch it, because
	// deleting a recovery that is still running would destroy the thing the operator is waiting for.
	cnpgRecoveryTimeout = controlplane.PostgresRecoveryTimeout
	// cnpgClaimRemovalTimeout bounds the wait for the pre-restore claims to actually disappear. A
	// claim is held by the pvc-protection finalizer until no pod mounts it, so the delete returns
	// immediately and the object lingers while the old instance's pod is terminating.
	cnpgClaimRemovalTimeout = controlplane.PostgresClaimRemovalTimeout
	// cnpgClaimRemovalPoll is the interval between those reads.
	cnpgClaimRemovalPoll = 2 * time.Second
)

// RestoreInstance rewinds environment req.Environment's whole Postgres instance to a point in its
// pgBackRest repository (ADR-0066 §4).
//
// The order is the safety, and every step is placed where it is because the step after it can fail:
//
//  1. The instance's own `Stanza` is read and checked against the destination the caller resolved.
//     This happens BEFORE anything is deleted, because a recovery pointed at a repository this
//     instance never wrote to does not fail — it produces an empty database — and finding that out
//     after the live instance has been removed would be the worst possible moment.
//  2. The pre-restore `Cluster` is deleted, then its claims, then the claims are WAITED for. A
//     `Cluster` created while the old claim still exists is reattached to it by CloudNativePG's own
//     classifier instead of recovering, which would silently produce the exact state the operator
//     asked to leave.
//  3. The recovery `Cluster` is created under the instance's own name and waited for.
func (a *Adapter) RestoreInstance(ctx context.Context, req controlplane.RestoreInstanceRequest) (controlplane.RestoreInstanceOutcome, error) {
	if req.Archive == nil {
		return controlplane.RestoreInstanceOutcome{}, fmt.Errorf("kube: a physical restore needs the repository to recover from: %w", controlplane.ErrInvalid)
	}
	instance, err := addonName(controlplane.AddonPostgres, req.Environment)
	if err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	if err := a.requireCloudNativePG(ctx); err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	if err := a.requireRecoverableRepository(ctx, instance, req.Archive); err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	// The superuser Secret is not recreated and not rotated. It is the credential CloudNativePG
	// reconciles `burrow_admin` to on the recovered instance (managed.roles), which is what makes the
	// provisioner's admin connection work against a data directory whose own copy of that password is
	// as old as the recovery target. Regenerating it here would be harmless and pointless; deleting it
	// would lock Burrow out of the instance it just restored.
	labels := a.postgresInstanceLabels(instance, req.Environment)
	if err := a.ensureCNPGSuperuserSecret(ctx, instance, labels); err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	// The repository credential is written FORWARD before anything is destroyed, for the reason
	// ensurePgBackRestCredentials exists at all: the Secret in the cluster is a copy of a credential
	// the registry owns, and a key rotated at the vendor since the instance was installed would leave
	// the recovery's sidecar authenticating with the old one. Discovering that after the `Cluster` and
	// its volume are gone is the worst possible moment for it. The value goes into the Secret and
	// nowhere else — never a field of a custom resource, never an argv, never an error.
	if err := a.ensurePgBackRestCredentials(ctx, instance, labels, req.Archive.Credential); err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}

	destroyed, err := a.removeInstanceForRecovery(ctx, instance)
	if err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	if err := a.createRecoveryCluster(ctx, instance, req); err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	if err := a.awaitRecoveredInstance(ctx, instance); err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	return controlplane.RestoreInstanceOutcome{Instance: instance, VolumesDestroyed: destroyed}, nil
}

// postgresInstanceLabels is the label set every resource of one Postgres instance carries, spelled
// once so the install and the restore stamp the same thing on the same objects.
func (a *Adapter) postgresInstanceLabels(instance, env string) map[string]string {
	return map[string]string{
		nameLabel:      instance,
		managedByLabel: managedByValue,
		addonLabel:     string(controlplane.AddonPostgres),
		addonEnvLabel:  env,
	}
}

// requireRecoverableRepository refuses a restore this instance has no repository to recover from,
// before anything is destroyed.
//
// A MISSING `Stanza` IS THE COMMON CASE AND IT IS REFUSED BY NAME. An instance installed with no
// object-storage provider registered has no stanza, no plugin and no archive — there is nothing in a
// repository to recover — and the message says how that instance came to be that way and what the
// per-app path is meanwhile, rather than leaving a recovery to fail with an empty database.
//
// A stanza describing a DIFFERENT repository is refused for the sharper reason. Recovery from a
// bucket this instance never archived to does not error: pgBackRest finds no backups for the stanza
// and CloudNativePG brings up whatever it can, which is an empty instance where a full one used to
// be. So the two are compared here, with the instance's own stanza as the authority, exactly as a
// physical backup compares them before writing anything.
func (a *Adapter) requireRecoverableRepository(ctx context.Context, instance string, archive *controlplane.ArchiveDestination) error {
	stanzas, err := a.pgBackRestStanzas()
	if err != nil {
		return err
	}
	// The plugin's controller has to be RUNNING, and it is checked here — before anything is
	// destroyed — rather than left to the recovery. A `Cluster` whose bootstrap names a plugin nothing
	// reconciles is accepted and then worked on by no one, so without this the instance and its volume
	// would be removed and the wait would run to its deadline over a recovery that never started.
	if warning, ok, err := a.pgBackRestAvailable(ctx); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("kube: the postgres instance %q cannot be recovered from its repository on this "+
			"cluster, and nothing was changed: %s: %w", instance, warning, controlplane.ErrInvalid)
	}
	if _, err := stanzas.Get(ctx, pgBackRestStanzaName(instance), metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("kube: the postgres instance %q has no pgBackRest repository, so there is nothing to "+
				"recover it from: it was created before an object-storage destination was registered, or the plugin "+
				"was not installed. Nothing was changed. `burrow addon restore postgres <app> --backup <id>` restores "+
				"one app from a logical dump, which does not need a repository: %w", instance, controlplane.ErrNotFound)
		}
		return fmt.Errorf("kube: reading the pgBackRest Stanza %q: %w", pgBackRestStanzaName(instance), err)
	}
	endpoint, err := pgBackRestArchiveEndpoint(archive.Config.Endpoint)
	if err != nil {
		return err
	}
	return a.checkStanzaMatchesDestination(ctx, stanzas, instance, endpoint, archive)
}

// removeInstanceForRecovery takes the pre-restore instance out of the way and returns the data claims
// it destroyed.
//
// IT DELETES THE CLAIMS RATHER THAN RETAINING THEM, which is the one place this departs from
// ADR-0064's default, and the departure is forced rather than chosen. A retained claim keeps
// CloudNativePG's own metadata deliberately (retainCNPGClaim says why), which makes it a DANGLING
// claim — and a dangling claim is precisely what a `Cluster` created again under the same name
// reattaches to instead of initializing. Keeping it would therefore not preserve the old state
// alongside the new one; it would silently cancel the recovery and bring the old state back up. The
// pre-restore state is preserved as a backup in the repository instead, which the engine has already
// taken and verified before this is reached.
//
// It tolerates an absent instance entirely: "the instance is gone" is the case a physical restore
// exists for, and an environment whose `Cluster` was deleted by hand must still be recoverable.
func (a *Adapter) removeInstanceForRecovery(ctx context.Context, instance string) ([]string, error) {
	clusters, err := a.cnpgClusters()
	if err != nil {
		return nil, err
	}
	if err := clusters.Delete(ctx, instance, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kube: removing the pre-restore CloudNativePG %s %q: %w", cnpgClusterKind, instance, err)
	}
	claims, err := a.cnpgClusterClaims(ctx, instance)
	if err != nil {
		return nil, err
	}
	destroyed := make([]string, 0, len(claims))
	for i := range claims {
		name := claims[i].Name
		if err := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("kube: removing the pre-restore data volume %q: %w", name, err)
		}
		destroyed = append(destroyed, name)
	}
	if err := a.awaitClaimsGone(ctx, instance); err != nil {
		return nil, err
	}
	return destroyed, nil
}

// awaitClaimsGone waits until no claim of the pre-restore instance is left.
//
// The wait is load-bearing rather than tidy-minded. A PersistentVolumeClaim carries the
// pvc-protection finalizer while any pod mounts it, so the delete above returns success and the
// object stays until the old instance's pod finishes terminating. Creating the recovery `Cluster`
// in that window hands it a claim CloudNativePG classifies as dangling, and a dangling claim is
// reattached rather than recovered from — the restore would appear to succeed and the instance would
// come back with exactly the state the operator asked to leave.
//
// A timeout is an error and the restore stops there. The old `Cluster` is already gone, so this is
// not a state anything is running in; it is a stuck volume, and pressing on would produce a silently
// wrong recovery rather than a visible failure.
func (a *Adapter) awaitClaimsGone(ctx context.Context, instance string) error {
	deadline := time.Now().Add(cnpgClaimRemovalTimeout)
	for {
		claims, err := a.cnpgClusterClaims(ctx, instance)
		if err != nil {
			return err
		}
		if len(claims) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("kube: the pre-restore data volume of %q was still present after %s, and a recovery "+
				"started against a volume that is still there is reattached to it instead of recovering — which would "+
				"bring the pre-restore state back up while reporting a successful restore. Nothing was recovered; the "+
				"instance's %s is already removed, so clear the volume and re-run",
				instance, cnpgClaimRemovalTimeout, cnpgClusterKind)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cnpgClaimRemovalPoll):
		}
	}
}

// createRecoveryCluster writes the `Cluster` that recovers the instance from its repository.
func (a *Adapter) createRecoveryCluster(ctx context.Context, instance string, req controlplane.RestoreInstanceRequest) error {
	spec, ok := controlplane.LookupAddon(controlplane.AddonPostgres)
	if !ok {
		return fmt.Errorf("kube: the catalog has no postgres add-on: %w", controlplane.ErrInvalid)
	}
	labels := a.postgresInstanceLabels(instance, req.Environment)
	clusters, err := a.cnpgClusters()
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": cnpgClusterAPIVersion,
		"kind":       cnpgClusterKind,
		"metadata": map[string]any{
			"name":      instance,
			"namespace": a.addonNamespace,
			"labels":    toStringMap(labels),
		},
		"spec": a.postgresRecoveryClusterSpec(spec, req.Environment, instance, labels, req),
	}}
	if _, err := clusters.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("kube: a CloudNativePG %s %q already exists, so this restore did not create the "+
				"recovering instance and must not adopt one it did not write: %w", cnpgClusterKind, instance, controlplane.ErrInvalid)
		}
		return fmt.Errorf("kube: creating the recovering CloudNativePG %s %q: %w", cnpgClusterKind, instance, err)
	}
	return nil
}

// postgresRecoveryClusterSpec composes the `Cluster` spec of a recovering instance: the ordinary
// instance spec with its `bootstrap` replaced by a recovery from the repository, plus the
// `externalClusters` entry that says where the repository is.
//
// IT IS THE SAME COMPOSITION AS AN INSTALL apart from the bootstrap, and that is deliberate. The
// resources, the tuning, the managed `burrow_admin` role, the additional Service every DATABASE_URL
// resolves and the inherited metadata the metrics add-on scrapes are all properties of a Burrow
// Postgres instance rather than of how it was created — an instance that came back from a restore
// missing any of them would be subtly not the instance it replaced. Composing it from
// postgresClusterSpec is what keeps the two from drifting.
//
// THE RECOVERED INSTANCE ARCHIVES, which is a deliberate deviation from the plugin's own restored-
// cluster example (that example carries the plugin without `isWALArchiver`). The reason is that this
// instance is not a scratch copy of a backup for inspection — it IS the environment's live database
// from the moment it comes up, and one that does not archive is a database with no backups and no
// signal saying so. It is safe to archive into the same stanza here because the pre-restore instance
// is already gone: nothing else is writing to the repository, so there is no window in which two
// instances archive into one stanza.
func (a *Adapter) postgresRecoveryClusterSpec(spec controlplane.AddonSpec, env, instance string, labels map[string]string, req controlplane.RestoreInstanceRequest) map[string]any {
	out := a.postgresClusterSpec(spec, env, instance, labels, req.Archive)
	// `bootstrap.initdb` and `bootstrap.recovery` are alternatives, not layers: an instance is either
	// initialized empty or recovered. So the bootstrap is REPLACED rather than added to.
	out["bootstrap"] = map[string]any{"recovery": recoveryBootstrap(req)}
	out["externalClusters"] = []any{map[string]any{
		"name": cnpgRecoverySourceName,
		"plugin": map[string]any{
			"name":       PgBackRestPluginName,
			"parameters": map[string]any{"stanzaRef": pgBackRestStanzaName(instance)},
		},
	}}
	return out
}

// recoveryBootstrap renders `bootstrap.recovery` for the requested target.
//
// NO TARGET IS THE NEWEST STATE. CloudNativePG with no `recoveryTarget` restores the latest base
// backup and replays every archived segment after it, which is the "the instance is gone, give me
// everything you have" answer — so it is an absence of fields rather than a field, and the engine
// refuses to reach it by default rather than by omission.
//
// A backup and a time are mutually exclusive and the engine has already refused both together; this
// prefers the backup if it somehow sees both, because a named base backup is the more specific ask.
func recoveryBootstrap(req controlplane.RestoreInstanceRequest) map[string]any {
	recovery := map[string]any{"source": cnpgRecoverySourceName}
	switch {
	case req.BackupLabel != "":
		// pgBackRest's own label for the base backup, which is what CloudNativePG passes through to
		// the plugin as the backup to restore.
		recovery["recoveryTarget"] = map[string]any{"backupID": req.BackupLabel}
	case req.TargetTime != "":
		recovery["recoveryTarget"] = map[string]any{"targetTime": req.TargetTime}
	}
	return recovery
}

// awaitRecoveredInstance waits until the recovered `Cluster` has an instance serving.
//
// A TIMEOUT DOES NOT DELETE ANYTHING. A recovery reads a base backup and replays write-ahead log,
// both proportional to how much data there is, so a wait that ran out says nothing about whether the
// recovery is going to succeed — and the `Cluster` it would delete is the recovery in progress. So
// the object is left exactly where it is and the message names it, which is the same posture a failed
// `Backup` object gets: the account of what happened is more valuable than a tidy namespace.
func (a *Adapter) awaitRecoveredInstance(ctx context.Context, instance string) error {
	deadline := time.Now().Add(cnpgRecoveryTimeout)
	for {
		cluster, found, err := a.getCNPGCluster(ctx, instance)
		if err != nil {
			return err
		}
		if found && cnpgClusterReady(cluster) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("kube: the recovering postgres instance %q was not serving after %s. The recovery is "+
				"NOT cancelled and the %s is left in place — restoring a base backup and replaying write-ahead log "+
				"takes as long as the data takes, so this is a wait that ran out rather than a recovery that failed. "+
				"Watch it with `burrow addon list`, and re-run the cutover with `burrow addon attach postgres <app>` "+
				"for each app once it is serving", instance, cnpgRecoveryTimeout, cnpgClusterKind)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cnpgRecoveryPoll):
		}
	}
}
