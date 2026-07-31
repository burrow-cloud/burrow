// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// This file is ADR-0064 §5: where an object-storage provider is registered, `addon remove
// --delete-data` takes a final backup BEFORE it destroys anything, and a failed or partial backup
// aborts the removal with nothing destroyed.
//
// The ordering is the whole of the safety. A destructive command that proceeds after failing to
// preserve what it is about to destroy is the worst outcome available here: the user asked for the
// data to go away AND believed a copy existed, so the loss is only discovered at the moment they go
// looking for the copy. That is why every check below returns before the first destructive call
// rather than recording a warning.
//
// Two things decided elsewhere carry this:
//
//   - The DESTINATION IS THE OBJECT STORE, never the in-cluster backup claim. ADR-0064 §4 keeps that
//     claim alive even under --delete-data, so a dump written to it does survive — but it survives
//     in the same failure domain as the database that was just destroyed, and losing the cluster
//     loses both. More decisively, a `Backup` row only says `completed` for an object-store
//     destination once the bytes were written AND read back (ADR-0063 §7); a cluster-destination row
//     says `completed` on a Job exiting zero. Only the first is a fact strong enough to destroy a
//     volume on the strength of. Where no object store is registered, §5 does not apply and the
//     behaviour is exactly what it was.
//   - The BACKUP IS TAKEN THROUGH THE ORDINARY BACKUP PATH (backupApp), so there is one Job, one
//     wait — awaitJob's, which fails fast with a reason from the closed set when the pod cannot
//     start (ADR-0074, issue #352) — and one set of row semantics. A `--delete-data` blocked on an
//     unschedulable backup Job says `Unschedulable`, rather than sitting for ten minutes and then
//     destroying the data anyway.

// RemoveAddonOptions is everything `addon remove` needs beyond the add-on's name. It is a struct
// rather than a run of positional booleans because this is the most destructive operation in the
// product and `RemoveAddon(ctx, name, true, false, true)` is not a call anyone can review.
type RemoveAddonOptions struct {
	// DeleteData destroys the add-on's data volume as well as its workload — for Postgres, every
	// attached app's database. Its absence is the safe default (ADR-0064 §1).
	DeleteData bool
	// SkipFinalBackup destroys the data WITHOUT the final backup ADR-0064 §5 otherwise requires.
	//
	// It exists because §5 cannot be mandatory: an add-on is often removed precisely BECAUSE it is
	// wedged, and a wedged instance cannot be dumped. With no way past the backup, a broken add-on
	// would be undeletable — trading a data-loss failure for an unrecoverable cluster. It is a
	// separate flag from DeleteData, not a mode of it, so asking for it is a sentence someone had to
	// write; and every path that honours it says in the result that no backup was taken.
	SkipFinalBackup bool
	// BackupDestination names which registered object-storage provider the final backup goes to. It
	// is only needed when several are registered — ADR-0063 §6 allows that on purpose, for a user
	// migrating between vendors — and Burrow refuses to guess rather than writing the last copy of a
	// database somewhere nobody is watching.
	BackupDestination string
	// Confirm satisfies the addon.remove guardrail's confirmation hold (ADR-0020). It is NOT the
	// data-loss acknowledgement: that gate lives on the operator CLI (ADR-0064 §2).
	Confirm bool
}

// finalBackupPlan is what a data-deleting removal will do about a final backup, settled BEFORE the
// guardrail is evaluated so the confirmation a human approves states it (ADR-0064 §5).
//
// It is one decision made in one place because the answer is read three times — by the guardrail
// message, by the step that takes the backup, and by the result the caller prints — and three
// independent readings of "is a backup being taken here" is how the prompt and the behaviour drift
// apart.
type finalBackupPlan struct {
	// take is true when a final backup will be attempted before anything is destroyed.
	take bool
	// provider is the object-storage provider the backup is written to, resolved here so the backup
	// itself is never ambiguous. Empty when take is false.
	provider string
	// skipped is true when the data volume will be destroyed with NO off-cluster copy taken — by the
	// override flag, or because nothing durable is registered to write one to. It is what the result
	// reports so the output can say plainly that no backup was taken.
	skipped bool
	// note is the one-line reason, phrased for a human reading either the confirmation or the
	// removal's output.
	note string
}

// consequence is the plan's clause in the guardrail's confirmation message. It reads as part of a
// sentence about the removal, so it starts lower-case and states the fact rather than the flag.
func (p finalBackupPlan) consequence() string {
	if p.take {
		return fmt.Sprintf("a final backup of each attached database is written to the %q object store FIRST, and the removal is abandoned if it fails", p.provider)
	}
	if p.note != "" {
		return "no final backup is taken (" + p.note + ")"
	}
	return "no final backup is taken"
}

// planFinalBackup decides whether this removal takes a final backup, and to where.
//
// It is NOT best-effort. A provider read that fails returns an error rather than falling through to
// "nothing is registered", because that fallthrough destroys the volume and then reports that there
// was nowhere to back it up to — a false assurance produced by an infrastructure blip. The registry
// is burrowd's own database; if it cannot be read, the removal was not going to finish anyway.
func (e *Engine) planFinalBackup(ctx context.Context, info AddonInfo, opts RemoveAddonOptions) (finalBackupPlan, error) {
	// Nothing is being destroyed, so there is nothing to preserve first. A removal that keeps its
	// volume is the default path and gains no backup step (ADR-0064 §1).
	if !opts.DeleteData {
		return finalBackupPlan{}, nil
	}
	// Only a Postgres instance can be dumped. Another add-on's volume holds derived data Burrow has
	// no logical export for (a log or metric store's blocks), so there is no backup to take — said
	// plainly rather than left as a silent absence, because "no backup was taken" is the fact the
	// operator is entitled to before approving.
	spec, known := LookupAddon(info.Type)
	if info.Mode != "installed" || !known || spec.StorageGi == 0 {
		return finalBackupPlan{}, nil
	}
	if info.Type != AddonPostgres {
		return finalBackupPlan{
			skipped: true,
			note:    "only the postgres add-on can be dumped, so its volume is destroyed without one",
		}, nil
	}
	if opts.SkipFinalBackup {
		return finalBackupPlan{
			skipped: true,
			note:    "--skip-final-backup was passed, so the data is destroyed with no off-cluster copy",
		}, nil
	}

	stores, err := e.objectStoreProviders(ctx)
	if err != nil {
		return finalBackupPlan{}, err
	}
	switch {
	case opts.BackupDestination != "":
		for _, name := range stores {
			if name == opts.BackupDestination {
				return finalBackupPlan{take: true, provider: name}, nil
			}
		}
		return finalBackupPlan{}, fmt.Errorf("no object-storage provider named %q is registered to take the final backup to — register one with `burrow config provider add --type s3`, or pass --skip-final-backup to destroy the data without one: %w",
			opts.BackupDestination, ErrNotFound)
	case len(stores) == 0:
		// ADR-0064 §5's stated limit: with nothing durable registered there is nowhere for a final
		// backup to go, so the behaviour is what it was and the retained backup claim (§4) is the only
		// copy. The output names it.
		return finalBackupPlan{
			skipped: true,
			note:    "no object-storage provider is registered, so there is nowhere off-cluster to write one",
		}, nil
	case len(stores) == 1:
		return finalBackupPlan{take: true, provider: stores[0]}, nil
	default:
		// The same refusal resolveBackupDestination makes, for the same reason: guessing writes the
		// last copy of every database somewhere nobody is watching. Named for remove's own flag, since
		// that is the one the operator has to type.
		return finalBackupPlan{}, fmt.Errorf("several object-storage providers are registered (%s) — pass --backup-destination to say which one holds the final backup; Burrow will not guess when the copy is the last one that will exist: %w",
			strings.Join(stores, ", "), ErrInvalid)
	}
}

// objectStoreProviders returns the names of the registered object-storage providers, sorted.
func (e *Engine) objectStoreProviders(ctx context.Context) ([]string, error) {
	all, err := e.db.Providers(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading providers to plan the final backup: %w", err)
	}
	var names []string
	for _, p := range all {
		if p.Serves(CapabilityObjectStorage) && p.ObjectStore != nil {
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// finalBackupBeforeDataDeletion takes the final backup ADR-0064 §5 requires, and returns an error —
// with NOTHING destroyed — if it does not arrive. Every caller path reaches it before the first
// destructive call.
//
// appsKnown is load-bearing and is not the same question as len(apps) == 0. An instance that will
// not answer yields no apps, and treating that as "no app is attached" would destroy every database
// on a wedged instance while reporting a successful backup. So an unenumerable instance REFUSES and
// names the override, which is the case ADR-0064 §5 wrote the override for.
//
// Backups are taken one app at a time and the first failure aborts. Stopping there rather than
// carrying on and reporting a partial set matters twice over: the removal is refused either way, so
// the remaining dumps would be work done for a removal that is not happening, and a partial set is
// the shape most easily misread as "the backup ran".
func (e *Engine) finalBackupBeforeDataDeletion(ctx context.Context, info AddonInfo, apps []string, appsKnown bool, plan finalBackupPlan) ([]Backup, error) {
	if !plan.take {
		return nil, nil
	}
	if !appsKnown {
		return nil, fmt.Errorf("refusing to destroy the data volume %q: the instance would not say which databases it holds, so a final backup cannot be taken and cannot be verified — fix the add-on and retry, or pass --skip-final-backup to destroy the data without a backup: %w",
			info.Name, ErrInvalid)
	}
	env := envName(info.Environment)
	backups := make([]Backup, 0, len(apps))
	for _, app := range apps {
		backup, outcome, err := e.backupApp(ctx, app, env, plan.provider)
		if err != nil {
			return nil, finalBackupRefusal(info, app, outcome.Reason, outcome.Detail, err)
		}
		// The Backup row is what the abort is decided on, not the Job's exit status. ADR-0063 §7 only
		// lets a row say `completed` for an object-store destination once the object was written and
		// read back, so Durable() IS "the bytes are safe" — and it checks the destination as well as
		// the status because a provider deregistered between the plan and the Job would leave a
		// perfectly `completed` row for a dump that never left the cluster, which is not the fact this
		// removal is standing on. The same predicate decides what the backup-age signal counts
		// (ADR-0066 §5), so the two cannot come to different answers about the same row.
		if !backup.Durable() {
			return nil, finalBackupRefusal(info, app, backup.FailureReason, backup.FailureDetail,
				fmt.Errorf("the backup was recorded as %q at destination %q", backup.Status, backup.Destination))
		}
		backups = append(backups, backup)
	}
	return backups, nil
}

// finalBackupRefusal is the error a failed final backup aborts the removal with. It names the app
// whose database could not be preserved, the CLOSED reason the Job reported, and the fact that
// nothing was destroyed — in that order, because the operator's first question is "did I lose it".
//
// The reason travels rather than being flattened into prose so the operator is told WHY the removal
// was refused: `Unschedulable` and `StoreRejected` are different problems with different fixes, and
// "the backup failed" is the answer that sends someone to re-run the same command. Both vocabularies
// can arrive here — ADR-0074 §2's IssueReason when the Job never started, ADR-0063 §7's
// BackupFailureReason when it ran — and both are safe to print: neither a reason nor its detail ever
// carries a credential.
func finalBackupRefusal(info AddonInfo, app, reason, detail string, cause error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "the final backup of %q failed, so nothing was removed and the data volume %q is intact", app, info.Name)
	if reason != "" {
		fmt.Fprintf(&b, " (%s)", reason)
	}
	if detail != "" {
		fmt.Fprintf(&b, ": %s", detail)
	}
	b.WriteString("; fix the cause and retry, or pass --skip-final-backup to destroy the data without a backup")
	return fmt.Errorf("%s: %w", b.String(), cause)
}
