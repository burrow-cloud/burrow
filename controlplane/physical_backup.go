// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
)

// This file is the engine half of ADR-0066 §2: Burrow asks CloudNativePG for a base backup of a whole
// instance, and records what it observed.
//
// IT IS NOT A REPLACEMENT FOR BackupAddon, AND SAYING SO IS PART OF THE DESIGN. ADR-0066 §4 keeps
// both paths deliberately, because they answer different questions and neither covers the other's
// failure:
//
//	burrow addon backup postgres <app>   one app's database, at the moment of the dump.
//	                                     "this app's data is wrong."
//	burrow addon backup-instance postgres   the whole instance, restorable to any point in the
//	                                     write-ahead-log window. "the instance is gone."
//
// Physical recovery rewinds the entire instance, so using it to fix one app would roll back every
// other app sharing it — a cross-app data loss triggered by a single-app operation. The two commands
// are named for the thing they act on rather than for their mechanism, so the one reached for under
// pressure is chosen by scope and not by whether somebody remembers which of "logical" and "physical"
// means which.

// BackupInstance takes a PHYSICAL base backup of one environment's whole Postgres instance
// (ADR-0066 §2): Burrow creates a `postgresql.cnpg.io/v1 Backup`, CloudNativePG hands it to the
// pgBackRest plugin, and Burrow reads the result. No backup tool runs here, no Job is constructed,
// and no superuser credential is handled on this path — that is the change ADR-0066 §2 exists for.
//
// THE ROW IS NOT ALLOWED TO SAY COMPLETED FOR BYTES THAT ARE NOT READABLE. A completed `Backup`
// object is the operator's word that pgBackRest exited zero; that the object store will serve the
// result back is a separate fact, and it is the one a restore depends on. So the backup's manifest is
// read back from the store at the key the repository says it is at, and only then does the row say
// completed (ADR-0063 §7). A store that will not answer is retried by the layers underneath; a store
// that answered and could not serve the object back is recorded as such and not retried.
//
// destination names which registered object-storage provider holds the repository; empty resolves it,
// which succeeds when exactly one is registered and refuses when several are (ADR-0063 §6). Unlike a
// logical dump, no object storage at all is a REFUSAL rather than a cluster-destination fallback:
// there is no in-cluster tier for a physical backup, and an instance that archives nowhere has
// nothing to base one on.
//
// It moves no secret value: the credential is read at call time to sign one read-back request, and
// the audit row and the returned result name the add-on, environment, backup id, provider and object
// key — never a credential. Backup destroys nothing and is allowed by default.
func (e *Engine) BackupInstance(ctx context.Context, t AddonType, env, instance, destination string) (BackupResult, error) {
	if t != AddonPostgres {
		return BackupResult{}, fmt.Errorf("backup instance %s: only the postgres add-on has physical backups: %w", t, ErrInvalid)
	}
	// A physical backup is taken FROM one server and covers every database on it, so the environment
	// names which instance and is recorded on the row (ADR-0067 §1). It is resolved as a mutating
	// operation's is, because taking a base backup of the wrong environment's instance is not a
	// read-only mistake — it is a repository filling with the wrong data.
	targetEnv, _, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup instance %s: %w", t, err)
	}
	// WHICH instance's repository this base backup goes into. An environment may hold more than one
	// (ADR-0091 §1), and each keeps the repository it was created against, so the instance is what
	// selects the stanza.
	inst, err := e.resolveInstance(ctx, t, targetEnv, instance)
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup instance %s: %w", t, err)
	}

	backupID := e.ids.NewID()
	// The redacted audit args carry the add-on, environment, instance, backup and destination NAMES
	// only.
	args := map[string]string{"addon": string(t), "env": targetEnv, "instance": instanceLabel(inst), "backup": backupID, "kind": string(BackupKindPhysical)}

	// Resolved BEFORE the row is written, so an ambiguous or misnamed destination fails without
	// leaving a pending backup nothing will finish.
	archive, err := e.resolveArchiveDestination(ctx, destination, targetEnv)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonBackup, string(t), args, err)
		return BackupResult{}, fmt.Errorf("backup instance %s: %w", t, err)
	}
	if archive == nil {
		err := fmt.Errorf("no object-storage provider is registered, so this instance archives nowhere and there is no repository to take a base backup into. Register one with `burrow config provider add --type s3` and re-run `burrow addon install postgres --env %s` to wire the instance to it; `burrow addon backup postgres <app>` keeps taking per-app dumps meanwhile: %w",
			targetEnv, ErrInvalid)
		e.recordExecution(ctx, auditOpAddonBackup, string(t), args, err)
		return BackupResult{}, fmt.Errorf("backup instance %s: %w", t, err)
	}
	args["destination"] = archive.Provider

	backup := Backup{
		ID:   backupID,
		Kind: BackupKindPhysical,
		// App is deliberately EMPTY. This backup covers every database on the instance, and naming one
		// of them would make a listing claim a per-app backup exists where none does.
		Environment: targetEnv,
		CreatedAt:   e.clock.Now(),
		Status:      BackupPending,
		Destination: BackupDestinationObjectStore,
		Provider:    archive.Provider,
	}
	if err := e.db.RecordBackup(ctx, backup); err != nil {
		e.recordExecution(ctx, auditOpAddonBackup, string(t), args, err)
		return BackupResult{}, fmt.Errorf("backup instance %s: recording backup: %w", t, err)
	}

	outcome, err := e.k8s.RunPhysicalBackup(ctx, targetEnv, inst.Name, backupID, archive)
	if err != nil {
		// Best-effort, and not gated on succeeding: a failed status write leaves the row pending,
		// which still reads as a successful backup nowhere. An empty reason is left empty rather than
		// guessed — it means the backup never reached a running `Backup` object at all.
		_ = e.db.FailBackup(ctx, backupID, outcome.Reason, outcome.Detail)
		e.recordExecution(ctx, auditOpAddonBackup, string(t), args, err)
		return BackupResult{}, fmt.Errorf("backup instance %s in environment %s: %w", t, targetEnv, err)
	}

	// The key is the adapter's, derived from the instance's own `Stanza`, so the read-back looks
	// where this instance actually wrote rather than where the engine would have configured it to.
	backup.ObjectKey = outcome.ObjectKey
	if err := e.verifyPhysicalBackup(ctx, archive, backup.ObjectKey); err != nil {
		_ = e.db.FailBackup(ctx, backupID, BackupReasonObjectNotReadable,
			"CloudNativePG reported the base backup completed, and the object store would not serve its manifest back at the key the repository says it is at")
		e.recordExecution(ctx, auditOpAddonBackup, string(t), args, err)
		return BackupResult{}, fmt.Errorf("backup instance %s in environment %s: %w", t, targetEnv, err)
	}

	// The key is written before the status, so a row that says completed always carries the address
	// the bytes were verified at. RecordBackup upserts by id, which is what makes this one write
	// rather than a new seam method for one column.
	if err := e.db.RecordBackup(ctx, backup); err != nil {
		_ = e.db.FailBackup(ctx, backupID, BackupReasonNotRecorded, "the backup reached its destination but its location could not be written to the registry")
		e.recordExecution(ctx, auditOpAddonBackup, string(t), args, err)
		return BackupResult{}, fmt.Errorf("backup instance %s: recording the backup's location: %w", t, err)
	}
	if err := e.db.SetBackupStatus(ctx, backupID, BackupCompleted, 0); err != nil {
		// The bytes are in the repository and the registry did not hear about it. Say so on the row:
		// the operator has a backup they cannot see, which is the harmless direction of this failure.
		_ = e.db.FailBackup(ctx, backupID, BackupReasonNotRecorded, "the backup reached its destination but its completion could not be written to the registry")
		e.recordExecution(ctx, auditOpAddonBackup, string(t), args, err)
		return BackupResult{}, fmt.Errorf("backup instance %s: recording completion: %w", t, err)
	}
	backup.Status = BackupCompleted
	e.recordExecution(ctx, auditOpAddonBackup, string(t), args, nil)
	return BackupResult{Backup: backup}, nil
}

// verifyPhysicalBackup reads the completed backup's manifest back out of the object store — ADR-0063
// §7's "the bytes are only a backup once the store will serve them back", applied to a repository
// Burrow did not write itself.
//
// The manifest is the right object to check and not an arbitrary one: it is what pgBackRest reads
// FIRST when restoring that backup, so a backup whose manifest is unreadable is not restorable
// whatever else is in the repository. Its LENGTH is deliberately not recorded as the backup's size —
// it is a few kilobytes describing gigabytes, and a listing showing it would be worse than a listing
// showing nothing.
//
// A build with no object-storage adapter wired cannot verify, and says so rather than passing: an
// unverifiable invariant reported as verified is the failure ADR-0063 §3 names, and it is no more
// acceptable here.
func (e *Engine) verifyPhysicalBackup(ctx context.Context, archive *ArchiveDestination, key string) error {
	if e.objectStore == nil {
		return fmt.Errorf("this build has no object-storage adapter wired, so the backup could not be read back and must not be recorded as one: %w", ErrInvalid)
	}
	store, err := e.objectStore.ObjectStore(archive.Config.Endpoint, archive.Config.Region, archive.Credential)
	if err != nil {
		return fmt.Errorf("the repository's endpoint could not be addressed to read the backup back: %w", err)
	}
	if _, err := store.StatObject(ctx, archive.Config.Bucket, key); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("the backup's manifest is not at %q in the repository, so the base backup CloudNativePG reported cannot be restored from: %w", key, ErrNotFound)
		}
		// The vendor's own words are NOT relayed: a response body can carry request identifiers,
		// bucket policies and occasionally the credential that was rejected (ADR-0063 §1).
		return fmt.Errorf("the repository would not serve the backup's manifest back at %q: %w", key, err)
	}
	return nil
}
