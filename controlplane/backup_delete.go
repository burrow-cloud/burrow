// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
)

// A backup is TWO THINGS: the dump — a file on a claim, and for a durable destination an object at a
// vendor — and the row in the control-plane registry that records it. Every path in this package
// creates both together (backupApp, BackupInstance); until this file there was no path that removed
// both together, and either half removed on its own is a lie of a different shape. A dump deleted
// off the volume leaves a row offering a restore from bytes that are gone, discovered during the
// incident the backup existed for. A row deleted on its own leaves an object nobody can name, since
// the key was only ever knowable from the row — it is billed monthly and found by nobody.
//
// So there is no honest way to expire a backup one half at a time, and that is why a retention
// number was, until this, a number nothing could act on. This file is the seam that makes acting on
// one possible. The POLICY is not here: how long a backup is kept, and what decides it, is the
// caller's — a retention rule resolved per install, or per tenant in a product built over this API.
// This is the mechanism a policy needs and nothing more.

// DeleteBackup removes a recorded backup — the dump AND the row — as one operation (ADR-0032).
//
// THE ORDER IS THE DESIGN, and it is chosen for what each failure leaves behind rather than for what
// the happy path looks like. The bytes go first and the row goes last:
//
//   - Row first, then the bytes, is the order that loses things. The moment the row is gone the
//     object key is unrecoverable — it is recorded nowhere else — so a failure after that point
//     leaves a dump at a vendor that no listing mentions, no sweep can find, and the operator keeps
//     paying for. The failure is SILENT and permanent.
//   - Bytes first, then the row, fails into a row that says a backup exists where none does. That is
//     wrong too, and it is wrong VISIBLY: the row is in every listing, it names its volume and its
//     key, and running the removal again finishes the job — because both byte removals are
//     idempotent. A wrong answer that repairs itself on retry beats a right-looking one that leaked.
//
// Within the bytes, the local copy goes before the durable one, on the same reasoning one level
// down: a dump on a claim can be found by looking at the claim, and an object can only be found
// through the row that names it, so the row must outlive the object.
//
// REMOVING A BACKUP THAT IS ALREADY GONE IS SUCCESS. An unknown id returns nil rather than
// ErrNotFound: this operation is asked for the state it found, and a caller pruning on a schedule
// necessarily retries after a crash and necessarily races another pass. Making them tell "removed"
// and "already removed" apart puts an errors.Is at every call site to mean "fine".
//
// It is NOT reachable from the agent control channel or the HTTP API, and carries no guardrail code
// for that reason. A guardrail is a hold on a DISCRETIONARY act — the confirmation ADR-0006 puts in
// front of a caller who asked for something destructive — and expiring a backup the retention rule
// says is expired is not a caller's discretion; a disposition set to `confirm` would either be
// answered by the policy loop itself, which is a confirmation of nothing, or hold the loop forever.
// The guardrail belongs with the verb, so it arrives when a `backup delete` verb does, scoped and
// named for the person invoking it.
//
// A backup STILL BEING TAKEN is refused, and pending alone is not what decides it: a row is pending
// while its Job runs, and it is also pending forever when the Job died with it (ADR-0074 §6). Those
// are opposite situations — one must not be touched, the other is exactly the debris a prune is for
// — so the cluster is asked which one this is. A PHYSICAL backup is refused outright: its bytes are
// inside a pgBackRest repository whose contents are the repository's to expire, and unlinking one
// object out of a stanza damages the backups either side of it.
func (e *Engine) DeleteBackup(ctx context.Context, t AddonType, backupID string) error {
	if t != AddonPostgres {
		return fmt.Errorf("delete backup %s: only the postgres add-on has backups: %w", t, ErrInvalid)
	}
	if backupID == "" {
		return fmt.Errorf("delete backup %s: a backup id is required: %w", t, ErrInvalid)
	}

	backup, err := e.db.GetBackup(ctx, backupID)
	if errors.Is(err, ErrNotFound) {
		// Already gone. Nothing was removed, so nothing is recorded: an audit row here would report a
		// deletion that did not happen, once per pass, for as long as the caller keeps retrying.
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete backup %s: backup %q: %w", t, backupID, err)
	}

	// The redacted audit args name the add-on, app, environment, backup and destination — never a
	// credential, never the object key's bucket (ADR-0027, ADR-0063 §1).
	args := map[string]string{"addon": string(t), "app": backup.App, "env": envName(backup.Environment), "backup": backupID, "kind": string(backupKind(backup))}
	if backup.Destination != "" {
		args["destination"] = string(backup.Destination)
	}
	// The audit TARGET is the app for a per-app dump. A physical row names no app, and is refused
	// below anyway.
	target := backup.App
	if target == "" {
		target = string(t)
	}

	if err := e.checkBackupRemovable(ctx, t, backup); err != nil {
		e.recordExecution(ctx, auditOpAddonBackupDelete, target, args, err)
		return err
	}

	if err := e.removeBackupBytes(ctx, t, backup); err != nil {
		e.recordExecution(ctx, auditOpAddonBackupDelete, target, args, err)
		return err
	}

	// LAST, for the reason at the top of this comment: everything ahead of it is idempotent, so a
	// failure here leaves a row that a second run removes, and the row names the bytes meanwhile.
	if err := e.db.DeleteBackup(ctx, backupID); err != nil {
		err = fmt.Errorf("delete backup %s: the dump was removed and its record could not be: %w", t, err)
		e.recordExecution(ctx, auditOpAddonBackupDelete, target, args, err)
		return err
	}
	e.recordExecution(ctx, auditOpAddonBackupDelete, target, args, nil)
	return nil
}

// checkBackupRemovable refuses the two backups that must not be removed underneath the thing using
// them: one that is still being taken, and a physical base backup, whose bytes belong to a
// pgBackRest repository rather than to Burrow.
func (e *Engine) checkBackupRemovable(ctx context.Context, t AddonType, b Backup) error {
	if b.Kind == BackupKindPhysical {
		return fmt.Errorf("delete backup %s: backup %q is a PHYSICAL base backup of environment %s's whole instance, held inside a pgBackRest repository. Its contents expire by the repository's own retention, and removing one object out of a stanza damages the backups either side of it, so it cannot be removed here: %w",
			t, b.ID, envName(b.Environment), ErrInvalid)
	}
	if b.Status != BackupPending {
		return nil
	}
	// Pending is two different situations wearing one status, and the registry cannot tell them
	// apart: a backup whose Job is running right now, and a row left behind by a burrowd that
	// restarted mid-backup (ADR-0074 §6). Removing the first would delete a dump out from under the
	// Job writing it; refusing the second would make abandoned rows the one thing retention could
	// never clean up, which is the population it most needs to.
	present, err := e.k8s.BackupJobPresent(ctx, b.ID)
	if err != nil {
		return fmt.Errorf("delete backup %s: backup %q is pending and the cluster could not say whether its Job is still running, so it is left alone: %w", t, b.ID, err)
	}
	if present {
		return fmt.Errorf("delete backup %s: backup %q is still being taken — its Job is running, and removing the dump now would delete the file it is writing. Wait for it to finish, or for it to fail: %w",
			t, b.ID, ErrInvalid)
	}
	return nil
}

// removeBackupBytes removes the dump itself: the file on the claim first, then the object at the
// vendor. Both are idempotent, so the pair may be retried whole.
//
// A backup with a durable destination has BOTH — the dump always lands on the claim and is shipped
// from there (ADR-0063 §7) — which is why this is not a switch on the destination. Treating the two
// as alternatives would leave the file behind on every object-store backup ever expired, filling the
// claim that retention was run to keep from filling.
func (e *Engine) removeBackupBytes(ctx context.Context, t AddonType, b Backup) error {
	if b.Path != "" {
		volume := b.Volume
		if volume == "" {
			// A row with no claim recorded predates per-environment backups; the shared claim is the
			// only one its dump can be on, which is what migration 00027 backfilled.
			volume = PostgresBackupVolume
		}
		if err := e.k8s.RemoveBackupFile(ctx, b.ID, volume, b.Path); err != nil {
			return fmt.Errorf("delete backup %s: removing the dump %q from volume %q: %w", t, b.Path, volume, err)
		}
	}
	if b.Destination != BackupDestinationObjectStore || b.ObjectKey == "" {
		return nil
	}
	if err := e.removeBackupObject(ctx, b); err != nil {
		// The row is deliberately still there when this returns: it is the only record of the key
		// this failed to remove, and dropping it would turn a retryable failure into a permanent
		// leak at the vendor.
		return fmt.Errorf("delete backup %s: removing the object for backup %q, whose record is kept so the object it names can still be found: %w", t, b.ID, err)
	}
	return nil
}

// removeBackupObject deletes the shipped dump from the provider named on the row. The provider is
// read from the ROW rather than resolved afresh, for the reason the row records it in the first
// place: a backup went to the destination registered when it was taken, and resolving today's would
// delete a key out of whichever bucket happens to be configured now.
//
// A provider the row names and the registry no longer holds is a REFUSAL, not a shrug. The bytes are
// at a vendor Burrow can no longer address, and removing the row would leave the only record of
// where they are nowhere at all.
func (e *Engine) removeBackupObject(ctx context.Context, b Backup) error {
	if e.objectStore == nil {
		return fmt.Errorf("this build has no object-storage adapter wired, so the shipped dump cannot be removed: %w", ErrInvalid)
	}
	if b.Provider == "" {
		return fmt.Errorf("the row says this backup was shipped to an object store but names no provider, so the object cannot be addressed: %w", ErrInvalid)
	}
	p, err := e.db.Provider(ctx, b.Provider)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("the provider %q that holds this backup is no longer registered, so the object cannot be removed; re-register it, or delete the object at the vendor and remove the record afterwards: %w", b.Provider, ErrNotFound)
		}
		return fmt.Errorf("reading the provider %q: %w", b.Provider, err)
	}
	if p.ObjectStore == nil {
		return fmt.Errorf("the provider %q serves no object storage any more, so the object cannot be removed: %w", b.Provider, ErrInvalid)
	}
	// Read at call time, so a rotated key is picked up with no restart (ADR-0023). The pair goes to
	// the adapter and nowhere else — never to the audit row, never to an error.
	cred, err := e.ObjectStoreCredentialFor(ctx, p)
	if err != nil {
		return err
	}
	store, err := e.objectStore.ObjectStore(p.ObjectStore.Endpoint, p.ObjectStore.Region, cred)
	if err != nil {
		return fmt.Errorf("the provider %q's endpoint could not be addressed: %w", b.Provider, err)
	}
	// DeleteObject is idempotent at the seam: an object already gone is success, so a retried prune
	// does not fail on the half it finished last time. The vendor's own words are not relayed — a
	// response body can carry request identifiers, bucket policies, and occasionally the credential
	// that was rejected (ADR-0063 §1).
	if err := store.DeleteObject(ctx, p.ObjectStore.Bucket, b.ObjectKey); err != nil {
		return fmt.Errorf("the provider %q would not remove the object at %q: %w", b.Provider, b.ObjectKey, err)
	}
	return nil
}

// backupKind reads a row's kind, filling in the one a blank can only mean: a row written before the
// two mechanisms coexisted is a logical dump, because nothing else existed to write it.
func backupKind(b Backup) BackupKind {
	if b.Kind == "" {
		return BackupKindLogical
	}
	return b.Kind
}
