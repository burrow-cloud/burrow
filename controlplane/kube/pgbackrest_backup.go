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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/burrow-cloud/burrow/controlplane"
)

// This file is ADR-0066 §2 in the smallest form the record admits: a backup is a `Backup` object, a
// schedule is a `ScheduledBackup`, and Burrow watches `.status`.
//
// There is no Job here, no argv, no exec, and no superuser credential. What a reader should notice is
// how little there is: the whole of "take a physical backup" is composing one custom resource and
// reading a phase off it, because the operator does the work. That is the change ADR-0066 §2 called
// the most valuable one, and it only looks like a small file because the code that used to be needed
// is somebody else's now.

var (
	// cnpgBackupGVR and cnpgScheduledBackupGVR are CloudNativePG's own resources, in the same group
	// and version as the `Cluster` — reached through the dynamic client for cnpgClusterGVR's reason.
	cnpgBackupGVR          = schema.GroupVersionResource{Group: CNPGAPIGroup, Version: "v1", Resource: "backups"}
	cnpgScheduledBackupGVR = schema.GroupVersionResource{Group: CNPGAPIGroup, Version: "v1", Resource: "scheduledbackups"}
)

const (
	cnpgBackupKind          = "Backup"
	cnpgScheduledBackupKind = "ScheduledBackup"
	// cnpgBackupMethodPlugin is the `spec.method` that hands a backup to a CNPG-I plugin rather than
	// to CloudNativePG's own volume-snapshot or barman paths. It is the field ADR-0066 §3's licensing
	// decision is expressed in: the method is `plugin`, and the plugin named is pgBackRest's.
	cnpgBackupMethodPlugin = "plugin"
	// cnpgBackupPoll is the interval between reads of a `Backup`'s status while waiting.
	cnpgBackupPoll = 2 * time.Second
)

// CloudNativePG's `Backup` phases, as its API declares them. They are matched as strings because
// this repository does not import CNPG's types; an unrecognised phase is treated as still running,
// which is the safe direction — a backup Burrow does not understand is one it waits for rather than
// one it declares completed.
const (
	cnpgBackupPhaseCompleted           = "completed"
	cnpgBackupPhaseFailed              = "failed"
	cnpgBackupPhaseWALArchivingFailing = "walArchivingFailing"
	cnpgBackupPhaseInvalid             = "invalid backup definition"
)

// physicalBackupName is the `Backup` object's name for a backup id. It is deliberately the SAME name
// the logical path gives its Job (backupJobName): the two are different kinds in different API
// groups, so they cannot collide, and one backup id addressing one object name in both mechanisms is
// what lets the observer ask "is this pending row still running" without first knowing which kind it
// is looking at.
func physicalBackupName(backupID string) string { return backupJobName(backupID) }

// cnpgBackups and cnpgScheduledBackups are the resource interfaces in the add-on namespace, or an
// error when no dynamic client is wired.
func (a *Adapter) cnpgBackups() (dynamic.ResourceInterface, error) {
	if a.dynamic == nil {
		return nil, fmt.Errorf("kube: no dynamic client is wired, so a %s cannot be created: %w",
			cnpgBackupKind, controlplane.ErrInvalid)
	}
	return a.dynamic.Resource(cnpgBackupGVR).Namespace(a.addonNamespace), nil
}

func (a *Adapter) cnpgScheduledBackups() (dynamic.ResourceInterface, error) {
	if a.dynamic == nil {
		return nil, fmt.Errorf("kube: no dynamic client is wired, so a %s cannot be created: %w",
			cnpgScheduledBackupKind, controlplane.ErrInvalid)
	}
	return a.dynamic.Resource(cnpgScheduledBackupGVR).Namespace(a.addonNamespace), nil
}

// PhysicalBackupPresent reports whether the `Backup` object for a backup id is STILL GOING (ADR-0074
// §6). It is the physical answer to the one question a `pending` row cannot answer about itself: is
// something still working on this, or is nothing ever going to finish it?
//
// EXISTENCE IS NOT THE TEST HERE, which is where it differs from the Job path. A Job is reaped on
// success, so a Job that is gone is a backup that is over; a `Backup` object is owned by the
// `Cluster` and Burrow never deletes it, so it outlives the backup by the life of the instance.
// Asking only whether it exists would report every pending row as still running for ever, and the
// case the sweep exists for — a burrowd that restarted mid-backup, leaving the row pending while the
// operator went on and finished the backup — would never be caught. So the PHASE decides: a settled
// object is not something that will finish this row.
//
// A missing object, an unwired dynamic client, an absent CRD and a refused read are all reported as
// absent, exactly as getCNPGCluster collapses them and for the same reason: on a cluster where the
// read cannot succeed, Burrow cannot have created the object either.
func (a *Adapter) PhysicalBackupPresent(ctx context.Context, backupID string) (bool, error) {
	if a.dynamic == nil {
		return false, nil
	}
	obj, err := a.dynamic.Resource(cnpgBackupGVR).Namespace(a.addonNamespace).
		Get(ctx, physicalBackupName(backupID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("kube: reading the physical backup for %q: %w", backupID, err)
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return !cnpgBackupSettled(phase), nil
}

// cnpgBackupSettled reports whether a `Backup` phase is terminal. An unrecognised phase is NOT
// settled, which is the safe direction in both readers: the waiter keeps waiting rather than
// declaring a backup it does not understand completed, and the sweep leaves a row alone rather than
// opening a failure against a backup that is still going.
func cnpgBackupSettled(phase string) bool {
	switch phase {
	case cnpgBackupPhaseCompleted, cnpgBackupPhaseFailed, cnpgBackupPhaseWALArchivingFailing, cnpgBackupPhaseInvalid:
		return true
	default:
		return false
	}
}

// RunPhysicalBackup asks CloudNativePG for a base backup of environment env's instance and waits for
// the answer (ADR-0066 §2).
//
// It refuses BEFORE writing anything when the instance is not archiving. An instance whose `Cluster`
// carries no pgBackRest plugin has no repository to write a base backup to, and a `Backup` object
// created against it would sit in `pending` until the wait timed out — ten minutes to learn something
// a single read answers now, and the refusal can say what to do about it.
func (a *Adapter) RunPhysicalBackup(ctx context.Context, env, backupID string, archive *controlplane.ArchiveDestination) (controlplane.PhysicalBackupOutcome, error) {
	instance, err := addonName(controlplane.AddonPostgres, env)
	if err != nil {
		return controlplane.PhysicalBackupOutcome{}, err
	}
	repoPath, err := a.requireArchivingInstance(ctx, env, instance, archive)
	if err != nil {
		return controlplane.PhysicalBackupOutcome{}, err
	}

	backups, err := a.cnpgBackups()
	if err != nil {
		return controlplane.PhysicalBackupOutcome{}, err
	}
	name := physicalBackupName(backupID)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": cnpgClusterAPIVersion,
		"kind":       cnpgBackupKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": a.addonNamespace,
			"labels": map[string]any{
				nameLabel:      name,
				managedByLabel: managedByValue,
				addonLabel:     string(controlplane.AddonPostgres),
				addonEnvLabel:  env,
			},
		},
		"spec": map[string]any{
			"method":              cnpgBackupMethodPlugin,
			"pluginConfiguration": map[string]any{"name": PgBackRestPluginName},
			"cluster":             map[string]any{"name": instance},
		},
	}}
	if _, err := backups.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return controlplane.PhysicalBackupOutcome{}, fmt.Errorf("kube: creating the %s %q: %w", cnpgBackupKind, name, err)
	}
	return a.awaitPhysicalBackup(ctx, backups, name, repoPath, pgBackRestStanzaName(instance))
}

// requireArchivingInstance refuses a physical backup of an instance that has no pgBackRest repository
// behind it, naming the two ways to be in that state.
//
// The two are genuinely different problems with different fixes, which is why they are not one
// message: there is no instance at all, or there is one that was created before an object-storage
// destination was registered. The second is the ordinary one — a user installs Postgres, then decides
// they want backups — and re-running the install is what wires it, so the message says so rather than
// leaving somebody to guess that reinstalling is safe.
func (a *Adapter) requireArchivingInstance(ctx context.Context, env, instance string, archive *controlplane.ArchiveDestination) (string, error) {
	cluster, found, err := a.getCNPGCluster(ctx, instance)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("kube: environment %q has no postgres instance to back up: %w", env, controlplane.ErrNotFound)
	}
	if !cnpgClusterArchives(cluster) {
		return "", fmt.Errorf("kube: the postgres instance %q archives nowhere, so there is no repository for a "+
			"physical backup to be written to. It was created before an object-storage destination was "+
			"registered. Register one with `burrow config provider add --type s3`, then re-run `burrow addon "+
			"install postgres --env %s` to wire the instance to it: the data is untouched by that, and the "+
			"per-app `burrow addon backup postgres <app>` dumps keep working meanwhile (ADR-0066 §3): %w",
			instance, env, controlplane.ErrInvalid)
	}
	// THE INSTANCE'S OWN STANZA IS THE AUTHORITY on where this backup will land, not the destination
	// the caller resolved. They can differ — a second registered provider named on the command line,
	// or a provider re-registered against a new bucket — and the difference matters twice: the
	// read-back would look in the wrong bucket and report a good backup as unreadable, and the row
	// would record a provider that does not hold it. So the two are compared before anything is
	// created, and the repository path comes back from the stanza rather than being derived again.
	stanzas, err := a.pgBackRestStanzas()
	if err != nil {
		return "", err
	}
	endpoint, err := pgBackRestArchiveEndpoint(archive.Config.Endpoint)
	if err != nil {
		return "", err
	}
	if err := a.checkStanzaMatchesDestination(ctx, stanzas, instance, endpoint, archive); err != nil {
		return "", err
	}
	return archive.RepoPath, nil
}

// cnpgClusterArchives reports whether a `Cluster` has the pgBackRest plugin wired as its write-ahead
// log archiver. It reads the object BACK from the API server rather than trusting what Burrow
// composed, which is the whole point: a `plugins` entry the CRD did not describe would have been
// pruned on write, silently and with a 201, and the first symptom would be a base backup that never
// arrives.
func cnpgClusterArchives(u *unstructured.Unstructured) bool {
	plugins, found, err := unstructured.NestedSlice(u.Object, "spec", "plugins")
	if err != nil || !found {
		return false
	}
	for _, p := range plugins {
		entry, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := entry["name"].(string); name == PgBackRestPluginName {
			return true
		}
	}
	return false
}

// awaitPhysicalBackup polls the `Backup`'s status until it settles, and turns the terminal phase into
// an outcome.
//
// THE PHASES ARE NOT ALL ONE FAILURE. `failed` is the backup itself not happening — pgBackRest would
// not take it, or the operator would not ask — and nothing was offered to the store. `walArchivingFailing`
// is the opposite half of ADR-0063 §7's distinction: the instance is producing write-ahead log that
// the repository is not accepting, so the destination is the problem and the reason says so. Reporting
// both as "the backup failed" would send an operator to the database when the answer is at the vendor.
//
// A `Backup` that failed is LEFT IN PLACE, exactly as a failed Job is: `.status.error` is the
// operator's own rendering of what pgBackRest said, and deleting the object to keep the namespace
// tidy would delete the only account of why. A succeeded one is left too — it is owned by the
// `Cluster` and goes when the instance does, and while it exists it is what `kubectl get backups`
// shows an operator who wants to see the repository from the cluster side.
func (a *Adapter) awaitPhysicalBackup(ctx context.Context, backups dynamic.ResourceInterface, name, repoPath, stanza string) (controlplane.PhysicalBackupOutcome, error) {
	deadline := time.Now().Add(backupJobTimeout)
	for {
		obj, err := backups.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return controlplane.PhysicalBackupOutcome{}, fmt.Errorf("kube: reading the %s %q: %w", cnpgBackupKind, name, err)
		}
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		switch phase {
		case cnpgBackupPhaseCompleted:
			label := physicalBackupLabel(obj)
			if label == "" {
				// CloudNativePG says the backup completed and the plugin did not say what pgBackRest
				// called it. Without the label there is no key to read the result back at and no handle a
				// restore could name, so this is not a completed backup Burrow can stand behind.
				return controlplane.PhysicalBackupOutcome{
					Reason: controlplane.BackupReasonObjectNotReadable,
					Detail: "CloudNativePG reported the backup completed without naming the pgBackRest backup it produced, so there is no key to verify it at",
				}, fmt.Errorf("kube: %s %q completed with no backup label", cnpgBackupKind, name)
			}
			return controlplane.PhysicalBackupOutcome{Label: label, ObjectKey: controlplane.PgBackRestManifestKey(repoPath, stanza, label)}, nil
		case cnpgBackupPhaseFailed, cnpgBackupPhaseInvalid:
			return controlplane.PhysicalBackupOutcome{
				Reason: controlplane.BackupReasonDumpFailed,
				Detail: physicalBackupDetail(obj, "the base backup did not complete"),
			}, fmt.Errorf("kube: %s %q failed", cnpgBackupKind, name)
		case cnpgBackupPhaseWALArchivingFailing:
			return controlplane.PhysicalBackupOutcome{
				Reason: controlplane.BackupReasonStoreUnreachable,
				Detail: physicalBackupDetail(obj, "the instance's write-ahead log is not reaching the repository, so a base backup taken now could not be restored"),
			}, fmt.Errorf("kube: %s %q is not archiving", cnpgBackupKind, name)
		}
		if !time.Now().Before(deadline) {
			return controlplane.PhysicalBackupOutcome{
				Reason: controlplane.BackupReasonDumpFailed,
				Detail: fmt.Sprintf("the backup was still %q after %s; the object is left in place and its status carries what CloudNativePG last said", phaseOrPending(phase), backupJobTimeout),
			}, fmt.Errorf("kube: %s %q did not settle within %s", cnpgBackupKind, name, backupJobTimeout)
		}
		select {
		case <-ctx.Done():
			return controlplane.PhysicalBackupOutcome{}, ctx.Err()
		case <-time.After(cnpgBackupPoll):
		}
	}
}

// physicalBackupLabel reads pgBackRest's own name for the backup out of the `Backup`'s status.
// CloudNativePG carries a plugin's result in two fields and which one is populated is the plugin's
// choice, so both are read and the first non-empty one wins rather than Burrow depending on a
// detail of somebody else's status writer.
func physicalBackupLabel(u *unstructured.Unstructured) string {
	for _, field := range []string{"backupName", "backupId"} {
		if v, found, err := unstructured.NestedString(u.Object, "status", field); err == nil && found && v != "" {
			return v
		}
	}
	return ""
}

// physicalBackupDetail renders the one Burrow-authored line a failed backup is recorded with,
// appending CloudNativePG's own error text when there is one.
//
// That text is safe to store and to print: it is the operator's rendering of a command's failure, and
// the credential never reaches an argv or an environment variable on this path — it is in a Secret
// the sidecar reads by reference — so there is nothing in it to leak (ADR-0063 §1).
func physicalBackupDetail(u *unstructured.Unstructured, fallback string) string {
	for _, field := range []string{"error", "commandError"} {
		if v, found, err := unstructured.NestedString(u.Object, "status", field); err == nil && found && v != "" {
			return fallback + ": " + v
		}
	}
	return fallback
}

// phaseOrPending names a phase for a message, standing in for the empty status a `Backup` carries
// before the operator has looked at it.
func phaseOrPending(phase string) string {
	if phase == "" {
		return "pending"
	}
	return phase
}
