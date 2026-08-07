// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file is ADR-0066 §4's restore orchestration: "recovery bootstraps a NEW `Cluster`, so a
// physical restore creates it, waits for it, cuts `DATABASE_URL` over, and decides the fate of the
// old one. That sequence is the feature; the CR is only its first step."
//
// IT IS INSTANCE-SCOPED, AND THAT IS THE WHOLE REASON IT IS A SEPARATE VERB. `addon restore <addon>
// <app>` restores one app's database from a logical dump and refuses a physical backup by name,
// because honouring a single-app restore against one would roll back every other app sharing the
// instance. That refusal stands unchanged. What this adds is the operation the refusal points at: a
// restore that says, in its name and in its confirmation, that it acts on the INSTANCE — so the
// blast radius is chosen deliberately rather than discovered afterwards.
//
//	burrow addon restore postgres <app> --backup <id>   one app's database, from its own dump.
//	                                                    Every other app on the instance untouched.
//	burrow addon restore-instance postgres --backup <id> the whole instance, and every database on
//	                                                    it, rewound together.
//
// The two are named for what they ACT ON rather than for their mechanism, which is the same choice
// `addon backup` and `addon backup-instance` make, and for the same reason: under pressure the verb
// reached for should be selected by scope, not by remembering which of "logical" and "physical"
// means which.

// THE CUTOVER READS EACH APP'S OWN VARIABLE NAME rather than holding a constant of its own. It used
// to define `databaseURLSecretKey = "DATABASE_URL"`, which agreed with attach and detach only because
// all three hard-coded the same string; once an attach can name its variable (issue #462), a fourth
// opinion about the name is a restore that enumerates the wrong apps and rewrites the wrong key.
// Both sites below go through Database.AddonEnvKey, which answers DATABASE_URL for an attachment that
// named nothing — so an environment where nobody chose a name behaves exactly as it did.

// RestoreInstanceOptions is everything `addon restore-instance` needs beyond the add-on type and the
// environment.
//
// EXACTLY ONE RECOVERY TARGET IS REQUIRED, and defaulting is refused rather than chosen. "The newest
// state" and "the moment before the bad migration" are different operations with the same blast
// radius, and a restore that picked one because the operator named neither would be the most
// destructive default in the product.
type RestoreInstanceOptions struct {
	// Backup names a recorded PHYSICAL backup to recover exactly. The instance comes back as that
	// base backup was, with no write-ahead log replayed past it.
	Backup string
	// ToTime is an RFC3339 instant inside the repository's write-ahead-log window. The instance comes
	// back as it was at that moment — the point-in-time recovery physical backups exist for.
	ToTime string
	// Latest recovers to the furthest point the archived write-ahead log reaches, which is the
	// newest state the repository can produce.
	Latest bool
	// SkipSafetyBackup destroys the instance's current state WITHOUT first taking a physical backup
	// of it (see the safety backup below). It is for an instance too broken to back up.
	SkipSafetyBackup bool
	// Destination NAMES the object-storage provider holding this instance's repository (ADR-0063 §6).
	// Empty resolves it, which works when exactly one is registered and is refused when several are —
	// the repository a live instance is rebuilt from is not a thing to guess at. It is a registry name,
	// never a credential, and it is CHECKED against the instance's own `Stanza` before anything is
	// destroyed rather than trusted.
	Destination string
	// Confirm satisfies the addon.restore_instance guardrail's confirmation hold (ADR-0020).
	Confirm bool
}

// target renders the recovery target as the one line every message about this restore uses, so the
// confirmation, the audit row and the result all describe the same instant the same way.
func (o RestoreInstanceOptions) target() string {
	switch {
	case o.Backup != "":
		return "backup " + o.Backup
	case o.ToTime != "":
		return "the state at " + o.ToTime
	default:
		return "the newest state in the repository"
	}
}

// validate refuses zero targets and more than one, naming what was asked for either way.
func (o RestoreInstanceOptions) validate() error {
	named := 0
	for _, on := range []bool{o.Backup != "", o.ToTime != "", o.Latest} {
		if on {
			named++
		}
	}
	switch named {
	case 1:
		if o.ToTime != "" {
			if _, err := time.Parse(time.RFC3339, o.ToTime); err != nil {
				return fmt.Errorf("the recovery time %q is not an RFC3339 instant (for example 2026-08-01T14:30:00Z): %w", o.ToTime, ErrInvalid)
			}
		}
		return nil
	case 0:
		return fmt.Errorf("a physical restore has to be told WHERE to rewind to: name a recorded backup, an instant inside the write-ahead-log window, or the newest state the repository holds. Nothing is assumed, because the three have the same blast radius and a wrong guess is not recoverable: %w", ErrInvalid)
	default:
		return fmt.Errorf("a physical restore has exactly one recovery target; name a backup, a time, or the newest state, not several: %w", ErrInvalid)
	}
}

// RestoreInstanceResult reports what a physical restore did (ADR-0066 §4). It names the instance,
// where it was rewound to, what happened to the state that was there, and every app that was on it —
// never a credential and never a connection string.
type RestoreInstanceResult struct {
	Addon       AddonType `json:"addon"`
	Environment string    `json:"environment"`
	// Instance is the Postgres instance that was rewound, by the name every consumer resolves it at
	// (AddonInstanceName).
	Instance string `json:"instance"`
	// RecoveryTarget is the point that was recovered to, in the same words the confirmation used. The
	// JSON name is not `target`: `burrow`'s own result envelope carries a `target` — the control plane
	// the command acted on (ADR-0078 §4) — and two different meanings under one key is how an agent
	// composing this result says the wrong thing about where a rewind happened.
	RecoveryTarget string `json:"recovery_target"`
	// SafetyBackup is the id of the physical backup taken of the instance's state BEFORE the rewind,
	// which is the way back from a restore to the wrong point. Empty when none was taken.
	SafetyBackup string `json:"safety_backup,omitempty"`
	// SafetyBackupNote says why no safety backup was taken, when none was. It is stated as plainly as
	// a taken one: an operator who rewound a database believing a copy exists is worse off than one
	// who knows none does (ADR-0064 §5's posture, applied to the same decision).
	SafetyBackupNote string `json:"safety_backup_note,omitempty"`
	// DataVolumesDestroyed names the pre-restore instance's data claims, which the recovery replaced.
	// They cannot be retained: CloudNativePG reattaches a `Cluster` to a claim left under its name
	// instead of recovering from the repository, so keeping one would silently cancel the restore.
	DataVolumesDestroyed []string `json:"data_volumes_destroyed,omitempty"`
	// Apps is every app that was attached to the instance when the restore began, by name. It is the
	// blast radius, recorded rather than counted.
	Apps []string `json:"apps"`
	// Reconnected is the apps whose DATABASE_URL was rewritten for the recovered instance and which
	// were restarted onto it.
	Reconnected []string `json:"reconnected"`
	// Stranded is the apps the cutover could not complete, with one line each. They still hold a
	// DATABASE_URL for an instance that has been rebuilt, so their credential no longer works and the
	// operator has to re-attach them — which is what the line says.
	Stranded []StrandedApp `json:"stranded,omitempty"`
	// Verification is what Burrow found on the recovered instance before it reconnected anything —
	// the answer to "is the data actually there", which is the only question a restore is asked.
	Verification RestoreVerification `json:"verification"`
}

// StrandedApp is one app the DATABASE_URL cutover did not finish for, and why.
type StrandedApp struct {
	App    string `json:"app"`
	Reason string `json:"reason"`
}

// RestoreVerificationStatus is what Burrow was able to establish about the data on a recovered
// instance. It is a three-value answer rather than a boolean because "the instance holds nothing" and
// "the instance was not asked" are different facts, and a restore path that reports the second as the
// first is exactly the false assurance this verification exists to remove.
type RestoreVerificationStatus string

const (
	// RestoreVerificationConfirmed: the recovered instance answered and holds Burrow-provisioned app
	// databases. The recovery read something out of the repository.
	RestoreVerificationConfirmed RestoreVerificationStatus = "confirmed"
	// RestoreVerificationAbsent: the recovered instance answered and holds NONE, where apps attached
	// to it did. This is a failed restore and is returned as an error, never as a result.
	RestoreVerificationAbsent RestoreVerificationStatus = "absent"
	// RestoreVerificationUnknown: nothing could be established — the instance would not answer, or
	// there was no Burrow-provisioned database whose return could have been checked. The restore
	// stands and the result says plainly that it is unproven.
	RestoreVerificationUnknown RestoreVerificationStatus = "unknown"
)

// RestoreVerification is what the recovered instance was found to hold, before any app was
// reconnected to it. It carries database NAMES only — never a row, never a credential.
type RestoreVerification struct {
	Status RestoreVerificationStatus `json:"status"`
	// Databases is every Burrow-provisioned app database the recovered instance holds, sorted. It is
	// the evidence the status is drawn from, reported rather than summarized: an operator reading
	// "confirmed" wants to see which databases came back.
	Databases []string `json:"databases"`
	// Missing is the apps that were attached to the instance and have no database on it now. It is
	// not on its own a failure — a recovery to a point before an app was attached legitimately comes
	// back without it — but it is the list of apps whose data this restore did not return.
	Missing []string `json:"missing,omitempty"`
	// Note says, in one line, why the status is what it is, when that is not self-evident from the
	// lists. Empty on a plain confirmation.
	Note string `json:"note,omitempty"`
}

// ErrRestoreNotVerified is a restore whose recovered instance came up serving and empty: the
// `Cluster` reached Ready, and none of the data the repository was supposed to hold is on it.
//
// It is its own sentinel rather than ErrInvalid because nothing about the request was wrong — the
// backup was named correctly, the guardrail was satisfied, the recovery ran — and a caller
// distinguishing "you asked for the wrong thing" from "what you asked for produced nothing" is
// making a genuinely different decision. It is also the one restore failure that leaves the cluster
// changed, which is why the message carries the way back rather than only the diagnosis.
var ErrRestoreNotVerified = errors.New("the recovered instance holds none of the data it was restored from")

// RestoreInstanceRequest is what the Kubernetes seam is asked to do: recover environment
// Environment's Postgres instance from its own pgBackRest repository.
//
// It carries an ArchiveDestination and therefore a SECRET VALUE, so like ArchiveDestination itself it
// is an internal in-memory shape only — no JSON tags, never returned over the control-plane API,
// never audited, never logged, never placed in an error.
type RestoreInstanceRequest struct {
	// Environment selects the instance, its repository path and its credential together, exactly as
	// it does on every other add-on path (ADR-0067 §1).
	Environment string
	// BackupLabel is pgBackRest's own label for the base backup to recover — the handle the
	// repository knows it by. Empty unless a recorded backup was named.
	BackupLabel string
	// TargetTime is an RFC3339 instant to recover to. Empty unless a time was named.
	TargetTime string
	// Archive is the repository to recover FROM, resolved by the engine and checked by the adapter
	// against the instance's own `Stanza` — a recovery pointed at a bucket the instance never wrote
	// to produces an empty database rather than an error.
	Archive *ArchiveDestination
}

// RestoreInstanceOutcome is what the seam did.
type RestoreInstanceOutcome struct {
	// Instance is the `Cluster` the recovered instance came up as. It is the environment's canonical
	// instance name: the recovery replaces the instance under the name every consumer already
	// resolves, rather than standing a differently-named one up beside it.
	Instance string
	// VolumesDestroyed names the data claims of the pre-restore instance that were removed to make
	// room for the recovered one.
	VolumesDestroyed []string
}

// RestoreInstance rewinds one environment's WHOLE Postgres instance to a point in its object-storage
// repository (ADR-0066 §4).
//
// THE SEQUENCE IS THE FEATURE, and each step exists because the one before it can fail:
//
//  1. The recovery target is resolved and, for a named backup, checked to be a PHYSICAL backup of
//     THIS environment that actually reached the store. A logical dump is refused by name — it is
//     the other command's input, and the two refusals now point at each other.
//  2. Every app on the instance is enumerated BY NAME, from the apps' own Secrets rather than from
//     the database, so the blast radius is known even when the instance is the thing that is gone.
//     An unreadable answer aborts: a rewind must not be confirmed against an unknown blast radius.
//  3. The guardrail is evaluated with those names in the message. "This is destructive" is not
//     consent; "this rewinds the databases of api, web and worker" is (ADR-0064 §3).
//  4. A PHYSICAL BACKUP OF THE CURRENT STATE IS TAKEN FIRST, and a failure aborts the restore with
//     nothing touched. This is ADR-0064 §5's ordering — nothing is destroyed until a copy is known to
//     exist — applied to the one operation that destroys a database on purpose. It is also the answer
//     to "what is the fate of the old one": its bytes go into the same repository as a backup this
//     restore names, so a rewind to the wrong point is itself recoverable.
//  5. The seam replaces the instance and waits for it to serve.
//  6. THE RECOVERED INSTANCE IS ASKED WHAT IT ACTUALLY HOLDS, and a restore that recovered nothing
//     fails here rather than being reported as a success (verifyRecoveredInstance). A `Cluster` that
//     reached Ready is not a restore; the data being on it is.
//  7. Every app is re-provisioned onto the recovered instance and restarted, through the same seam
//     `addon attach` writes. The roles in a recovered instance are as they were at the recovery
//     target, so an app's old password may simply not exist any more; re-provisioning rotates it and
//     hands the app a URL that works.
//
// STEP 6 COMES BEFORE STEP 7 AND THAT ORDER IS LOAD-BEARING. Re-provisioning CREATES an app's
// database when it is not there, so a cutover run first would manufacture exactly the databases the
// verification looks for — the check would pass on an empty instance because the check itself filled
// it in. Asking first is what makes the answer evidence.
//
// It moves no secret value: the repository credential is assembled at call time and handed only to
// the seam, the regenerated connection strings go only to SetSecretValue, and the audit row records
// the add-on, environment, instance, target and affected app NAMES — never a credential and never a
// URL.
func (e *Engine) RestoreInstance(ctx context.Context, t AddonType, env string, opts RestoreInstanceOptions) (RestoreInstanceResult, error) {
	if t != AddonPostgres {
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: only the postgres add-on has physical restore: %w", t, ErrInvalid)
	}
	if err := opts.validate(); err != nil {
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: %w", t, err)
	}
	if e.dbProvisioner == nil {
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: database provisioning is not configured, so the apps on the recovered instance could not be reconnected to it: %w", t, ErrNotImplemented)
	}
	targetEnv, ns, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: %w", t, err)
	}
	instance, err := AddonInstanceName(t, targetEnv)
	if err != nil {
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: %w", t, err)
	}

	// The redacted audit args carry NAMES only: the add-on, the environment, the instance and the
	// recovery target. The apps and the safety backup are added below as they become known, so a row
	// written on an early failure still says what was being attempted.
	args := map[string]string{"addon": string(t), "env": targetEnv, "instance": instance, "target": opts.target()}

	archive, err := e.resolveArchiveDestination(ctx, opts.Destination, targetEnv)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonRestoreInstance, instance, args, err)
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: %w", t, err)
	}
	if archive == nil {
		err := fmt.Errorf("no object-storage provider is registered, so this instance has no repository to recover from. A physical restore reads base backups and archived write-ahead log out of an object store; there is no in-cluster tier for one. `burrow addon restore %s <app> --backup <id>` restores a single app from a logical dump, which does have one: %w", t, ErrInvalid)
		e.recordExecution(ctx, auditOpAddonRestoreInstance, instance, args, err)
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: %w", t, err)
	}
	args["repository"] = archive.Provider

	label, err := e.recoveryLabel(ctx, t, targetEnv, opts)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonRestoreInstance, instance, args, err)
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: %w", t, err)
	}

	apps, err := e.appsAttachedInEnvironment(ctx, ns, targetEnv)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonRestoreInstance, instance, args, err)
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: %w", t, err)
	}
	if len(apps) > 0 {
		args["apps"] = strings.Join(apps, ",")
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: loading guardrail policy: %w", t, err)
	}
	if err := e.recordDecision(ctx, auditOpAddonRestoreInstance, instance, args, GuardrailAddonRestoreInstance,
		// Scoped by the INSTANCE this rewinds (ADR-0085 §1): the instance is exactly the blast
		// radius, so naming it describes the reach truthfully where naming one app would not.
		// addon.* is not EnvScopable, so the environment reaches the lookup through the instance
		// name rather than as a tier of its own (ADR-0035 phase 2c, ADR-0067 §1).
		pol.evaluateGuardrail(GuardrailScope{Env: targetEnv, Name: instance}, "addon restore-instance", GuardrailAddonRestoreInstance, opts.Confirm,
			restoreInstanceConsequence(instance, targetEnv, opts.target(), apps))); err != nil {
		return RestoreInstanceResult{}, err
	}

	// The app lists are normalized to empty rather than nil: a caller reading this answer is asking
	// which apps were affected, and a missing key reads as "unknown" where an empty list reads as
	// "none" — the same reason the guard report normalizes its own.
	result := RestoreInstanceResult{
		Addon:          t,
		Environment:    targetEnv,
		Instance:       instance,
		RecoveryTarget: opts.target(),
		Apps:           emptyIfNil(apps),
		Reconnected:    []string{},
	}

	// The safety backup, before anything is destroyed (ADR-0064 §5's ordering).
	safety, note, err := e.safetyBackup(ctx, t, targetEnv, instance, archive.Provider, opts)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonRestoreInstance, instance, args, err)
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s: %w", t, err)
	}
	result.SafetyBackup, result.SafetyBackupNote = safety, note
	if safety != "" {
		args["safety_backup"] = safety
	}

	outcome, err := e.k8s.RestoreInstance(ctx, RestoreInstanceRequest{
		Environment: targetEnv,
		BackupLabel: label,
		TargetTime:  opts.ToTime,
		Archive:     archive,
	})
	if err != nil {
		e.recordExecution(ctx, auditOpAddonRestoreInstance, instance, args, err)
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s in environment %s: %w", t, targetEnv, err)
	}
	result.DataVolumesDestroyed = emptyIfNil(outcome.VolumesDestroyed)

	// The proof, and it is taken BEFORE the cutover for the reason the doc comment above states: the
	// cutover creates the databases it cannot find, so a check run after it would be reading its own
	// handiwork.
	result.Verification = e.verifyRecoveredInstance(ctx, targetEnv, apps)
	args["verified"] = string(result.Verification.Status)
	if result.Verification.Status == RestoreVerificationAbsent {
		err := emptyRecoveryError(t, targetEnv, instance, result)
		e.recordExecution(ctx, auditOpAddonRestoreInstance, instance, args, err)
		return RestoreInstanceResult{}, fmt.Errorf("restore instance %s in environment %s: %w", t, targetEnv, err)
	}

	// The cutover. A failure here does not fail the restore: the instance is up with the data on it,
	// and reporting the whole operation as failed would send an operator to repeat the rewind rather
	// than to re-attach the one app that did not reconnect.
	reconnected, stranded := e.cutOverApps(ctx, t, targetEnv, ns, apps)
	result.Reconnected, result.Stranded = emptyIfNil(reconnected), stranded
	if len(result.Stranded) > 0 {
		args["stranded"] = strings.Join(strandedNames(result.Stranded), ",")
	}
	e.recordExecution(ctx, auditOpAddonRestoreInstance, instance, args, nil)
	return result, nil
}

// recoveryLabel turns the requested target into pgBackRest's own label for the base backup to
// recover, or "" for a time-based or latest recovery.
//
// A NAMED BACKUP IS CHECKED THREE WAYS, and each check exists because of a specific way of being
// handed the wrong id off a listing:
//
//   - It must be PHYSICAL. A logical dump is the input to `addon restore <addon> <app>`, and this is
//     the mirror of that command's refusal of a physical backup: the two paths refuse each other's
//     backups by name, so whichever one is reached for first says which one to use instead.
//   - It must belong to THIS environment. Each environment has its own instance and its own
//     repository path (ADR-0067 §1), so another environment's base backup is not a source this
//     instance's stanza can even see.
//   - It must have COMPLETED at the object store. A row that is pending or failed is a backup Burrow
//     could not read back, and recovering from bytes that were never verified is the failure ADR-0063
//     §7 exists to prevent, arriving at the one moment it cannot be corrected.
func (e *Engine) recoveryLabel(ctx context.Context, t AddonType, targetEnv string, opts RestoreInstanceOptions) (string, error) {
	if opts.Backup == "" {
		return "", nil
	}
	backup, err := e.db.GetBackup(ctx, opts.Backup)
	if err != nil {
		return "", fmt.Errorf("backup %q: %w", opts.Backup, err)
	}
	if backup.Kind != BackupKindPhysical {
		return "", fmt.Errorf("backup %q is a logical dump of app %q's database, not a physical backup of environment %s's instance. Restoring one app from it is `burrow addon restore %s %s --backup %s`, which touches no other app; this command rewinds the whole instance and needs a backup taken with `burrow addon backup-instance %s`: %w",
			backup.ID, backup.App, targetEnv, t, backup.App, backup.ID, t, ErrInvalid)
	}
	if bEnv := envName(backup.Environment); bEnv != targetEnv {
		return "", fmt.Errorf("backup %q was taken from environment %q, not %q. Each environment has its own instance and its own repository, so another environment's base backup is not a source this one can recover from: %w",
			backup.ID, bEnv, targetEnv, ErrInvalid)
	}
	if !backup.Durable() {
		return "", fmt.Errorf("backup %q is recorded as %q, so Burrow never read it back out of the repository and cannot say the bytes are there. Recovering from it would replace a live instance with a backup nothing has verified; pick one `burrow addon backups %s` shows as completed: %w",
			backup.ID, backup.Status, t, ErrInvalid)
	}
	label := PgBackRestLabelFromManifestKey(backup.ObjectKey)
	if label == "" {
		return "", fmt.Errorf("backup %q completed but carries no repository location, so there is no pgBackRest backup for a recovery to name. Recover to a time inside the write-ahead-log window instead, or to the newest state the repository holds: %w",
			backup.ID, ErrInvalid)
	}
	return label, nil
}

// safetyBackup takes a physical backup of the instance's CURRENT state before the rewind destroys
// it, and returns its id — or, when one was deliberately not taken, the line saying so.
//
// THE FAILURE ABORTS THE RESTORE. That is the whole of the safety and it is ADR-0064 §5's ordering
// read onto this operation: a rewind with a fresh off-cluster copy of what was there is a decision
// that can be undone by rewinding to that copy, and a rewind without one is the moment the current
// state stops existing. So the copy is made first and nothing is destroyed until it is known to be
// in the repository — the same bar every physical backup clears, since BackupInstance reads the
// manifest back before it records completion.
//
// SkipSafetyBackup exists for the same reason `--skip-final-backup` does: the instance is often being
// restored BECAUSE it is broken, and an instance that cannot take a base backup would otherwise be
// the one instance that cannot be restored. It is reported in the result rather than left implicit.
func (e *Engine) safetyBackup(ctx context.Context, t AddonType, targetEnv, instance, provider string, opts RestoreInstanceOptions) (id, note string, err error) {
	if opts.SkipSafetyBackup {
		return "", "no backup of the pre-restore state was taken (--skip-safety-backup): the state that was on this instance before the rewind is gone", nil
	}
	// An environment with no add-on registered has no current state to lose. This is asked of the
	// REGISTRY rather than inferred from the backup's error, and the difference matters: "there is no
	// instance" and "the instance would not produce a backup" arrive as the same ErrNotFound once the
	// read-back is involved, and reading the second as the first is how a rewind proceeds with no copy
	// while reporting that none was needed.
	installed, err := e.postgresInstalled(ctx, targetEnv, instance)
	if err != nil {
		return "", "", err
	}
	if !installed {
		return "", "no backup of the pre-restore state was taken: the environment had no Postgres instance to back up", nil
	}
	// The repository the caller RESOLVED, named explicitly. Re-resolving it from nothing would refuse
	// for ambiguity on a cluster with several providers registered — and the refusal's own advice is
	// --skip-safety-backup, so a spurious failure here would push an operator into rewinding with no
	// copy at all.
	res, err := e.BackupInstance(ctx, t, targetEnv, provider)
	if err == nil {
		return res.Backup.ID, "", nil
	}
	return "", "", fmt.Errorf("the instance's current state could not be backed up before rewinding it, so nothing was restored and nothing was destroyed. Fix the backup path and re-run, or pass --skip-safety-backup to rewind WITHOUT a copy of what is there now (for an instance too broken to dump): %w", err)
}

// verifyRecoveredInstance asks the instance that just came up which Burrow-provisioned app databases
// it holds, and turns the answer into the one thing a restore is actually asked to prove.
//
// THIS EXISTS BECAUSE A RECOVERY THAT RECOVERED NOTHING LOOKS EXACTLY LIKE ONE THAT WORKED. pgBackRest
// finding no base backup for the stanza is not an error: CloudNativePG initializes an empty instance,
// the `Cluster` reaches Ready, the seam's wait returns, and every signal Burrow surfaces reads green
// over a database with nothing in it. A repository the instance never wrote to, a stanza pointed
// somewhere else, an object lifecycle rule that outran the retention window — each arrives as the
// same silent success. So the instance is asked, and the answer is the restore's outcome.
//
// THE EVIDENCE IS THE DATABASES, NOT THE ROWS, and that limit is stated rather than papered over.
// Burrow does not know what any app's data should look like, so it cannot check content; what it does
// know is that `addon attach` created a database owned by an app_<app> role for every attached app,
// and that a recovered instance carrying those databases read something out of the repository while
// one carrying none did not. That is a floor, not a proof of completeness — a restore to a point
// mid-way through the window still needs an operator to look at their own data — and it catches the
// specific failure that is otherwise invisible.
//
// THE EXPECTATION COMES FROM THE APPS' OWN SECRETS, not from a registry row or from the pre-restore
// instance. It is the same list the guardrail named in its confirmation, gathered by
// appsAttachedInEnvironment for the same reason: it is available when the instance is gone, which is
// the case a physical restore exists for.
func (e *Engine) verifyRecoveredInstance(ctx context.Context, targetEnv string, apps []string) RestoreVerification {
	found, known := e.appDatabases(ctx, targetEnv)
	if !known {
		// An unanswerable instance is NOT reported as verified, for the reason verifyPhysicalBackup
		// refuses to record a backup it could not read back: an invariant that could not be checked
		// and is reported as holding is worse than one nobody claimed. It does not fail the restore
		// either — the instance is up, the cutover will report per app what it could not do, and a
		// hard failure here would send an operator to repeat a rewind over a recovery that may be
		// perfectly good.
		return RestoreVerification{
			Status:    RestoreVerificationUnknown,
			Databases: []string{},
			Note:      "the recovered instance could not be asked which databases it holds, so Burrow cannot say the data came back; check one app's own data before relying on this restore",
		}
	}
	if len(found) > 0 {
		v := RestoreVerification{Status: RestoreVerificationConfirmed, Databases: found, Missing: missingDatabases(apps, found)}
		if len(v.Missing) > 0 {
			v.Note = "these apps were attached to the instance and have no database on it now, which is what a recovery to a point before they were attached looks like — if that is not what was asked for, the data they held is not in this recovery"
		}
		return v
	}
	if len(apps) == 0 {
		return RestoreVerification{
			Status:    RestoreVerificationUnknown,
			Databases: []string{},
			Note:      "no app was attached to this instance, so there was no Burrow-provisioned database whose return could be checked; the instance came up serving and nothing more than that is claimed",
		}
	}
	return RestoreVerification{
		Status:    RestoreVerificationAbsent,
		Databases: []string{},
		Missing:   append([]string(nil), apps...),
	}
}

// missingDatabases is the attached apps with no database on the recovered instance.
func missingDatabases(apps, found []string) []string {
	present := make(map[string]bool, len(found))
	for _, db := range found {
		present[db] = true
	}
	var missing []string
	for _, app := range apps {
		if !present[app] {
			missing = append(missing, app)
		}
	}
	return missing
}

// emptyRecoveryError is what a restore that recovered nothing says.
//
// IT NAMES THE WAY BACK, because this is the one restore failure that has already changed the
// cluster: the pre-restore instance and its volume are gone, and an error that only diagnosed the
// problem would leave an operator holding an empty database with no stated next move. The safety
// backup taken on the way in is exactly that move, so its id is in the message rather than only in
// the result — the result is not returned on an error path, and this is the moment its most
// important field is needed.
//
// It also says why no app was reconnected, which is the deliberate part. Re-provisioning every app
// onto an empty instance would hand each of them a working credential for a database created fresh
// under its own name, and the apps would start writing into it: a restore that recovered nothing
// would become a restore that quietly discarded everything. Leaving each app's own connection-string
// variable alone leaves them failing to authenticate, which is loud, reversible, and true.
func emptyRecoveryError(t AddonType, targetEnv, instance string, res RestoreInstanceResult) error {
	back := res.SafetyBackupNote
	if res.SafetyBackup != "" {
		back = fmt.Sprintf("the state that was on this instance before the rewind is backup %s: `burrow addon restore-instance %s --backup %s --env %s` puts it back",
			res.SafetyBackup, t, res.SafetyBackup, targetEnv)
	}
	return fmt.Errorf("the %s instance %q was rebuilt and came up serving, and it holds NO database for any of the %d app(s) attached to it (%s). "+
		"That is what recovering from a repository holding nothing for this instance looks like from the outside: the base backup is not found, an empty instance is initialized, and the recovery reports success over a database with nothing in it. "+
		"NO APP WAS RECONNECTED, deliberately — re-provisioning them here would create their databases fresh and empty under their own names and the apps would begin writing into them, turning a restore that recovered nothing into one that discarded everything. "+
		"Each app's connection-string variable is untouched, so they fail to connect rather than quietly finding an empty database. "+
		"%s. If an instance without these databases is genuinely what was asked for, attach each app with `burrow addon attach %s <app> --env %s`: %w",
		t, instance, len(res.Apps), strings.Join(res.Apps, ", "), back, t, targetEnv, ErrRestoreNotVerified)
}

// postgresInstalled reports whether the environment has a Postgres add-on in the registry — the
// question "is there a current state for a safety backup to copy". The registry is the source of
// truth for what add-ons exist (ADR-0025), so this is one read rather than a cluster probe.
func (e *Engine) postgresInstalled(ctx context.Context, targetEnv, instance string) (bool, error) {
	addons, err := e.db.Addons(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the add-on registry: %w", err)
	}
	for _, a := range addons {
		// A row written before add-ons were per-environment is the default environment's, since that
		// is the only instance that could have existed then (ADR-0067 §3).
		if a.Type == AddonPostgres && a.Mode == "installed" && envName(a.Environment) == targetEnv {
			return true, nil
		}
	}
	// The registry saying no is not the end of it, because the two can disagree in one direction that
	// matters: a row deleted by hand over a database that is still running. Skipping the safety backup
	// there would destroy a live instance while reporting there was nothing to copy. A cluster that
	// answers "an instance is serving" therefore overrides the registry; a cluster that will not answer
	// leaves the registry's answer standing, since a restore must stay possible when nothing responds.
	if ready, err := e.k8s.AddonReady(ctx, instance); err == nil && ready {
		return true, nil
	}
	return false, nil
}

// appsAttachedInEnvironment lists, by name, every app in the environment holding a Burrow-provisioned
// DATABASE_URL — the concrete set a rewind of the instance affects.
//
// IT ASKS THE APPS, NOT THE DATABASE, and that is the difference from the enumeration a removal uses.
// A removal asks the instance who is attached and degrades when the instance will not answer, because
// a wedged add-on must stay removable. A restore is reached for when the instance is the thing that
// is broken, so an enumeration that needed the instance would be at its least reliable exactly when it
// matters. Attachment is also visible from the app's side — `addon attach` writes the key into the
// app's own Secret — so this answer is available with the database down.
//
// Only key NAMES are read; no value is fetched, logged or returned (ADR-0028). An unreadable listing
// is an ERROR rather than an empty answer: the caller is about to ask a human to approve a rewind of
// every app on the instance, and a confirmation that silently understates the blast radius is worse
// than one that could not be produced.
//
// THE CANDIDATE SET IS THE UNION of the workloads running in the environment and the apps the registry
// says Burrow has deployed there, because either alone misses a case: an app whose workload was
// deleted still holds the Secret, and an app deployed for the first time since the registry was last
// written is running before it is recorded. The residual gap is stated rather than papered over — an
// app that was attached and NEVER deployed appears in neither, so it keeps a connection string the
// recovered instance no longer honours until someone re-attaches it. Closing that needs a seam that
// lists Secrets in a namespace, which this does not add.
func (e *Engine) appsAttachedInEnvironment(ctx context.Context, ns, targetEnv string) ([]string, error) {
	k := e.k8s.WithNamespace(ns)
	workloads, err := k.ListWorkloads(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading which apps are on this instance: %w", err)
	}
	candidates := map[string]bool{}
	for _, w := range workloads {
		candidates[w.App] = true
	}
	managed, err := e.db.ManagedApps(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading which apps are on this instance: %w", err)
	}
	for _, m := range managed {
		if envName(m.Env) == targetEnv {
			candidates[m.App] = true
		}
	}
	names := make([]string, 0, len(candidates))
	for app := range candidates {
		names = append(names, app)
	}
	sort.Strings(names)

	var attached []string
	for _, app := range names {
		// The variable this app's attachment was written under — not a fixed name, since an app that
		// attached under its own is attached just as much as one that took the default (issue #462).
		want, err := e.db.AddonEnvKey(ctx, string(AddonPostgres), app, targetEnv)
		if err != nil {
			return nil, fmt.Errorf("reading which apps are on this instance: %s: %w", app, err)
		}
		keys, err := k.SecretKeys(ctx, app)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("reading which apps are on this instance: %s: %w", app, err)
		}
		for _, key := range keys {
			if key == want {
				attached = append(attached, app)
				break
			}
		}
	}
	return attached, nil
}

// cutOverApps points every app at the recovered instance and restarts it onto it (ADR-0066 §4's
// "cuts DATABASE_URL over").
//
// IT IS THE ATTACH PATH, NOT A COPY OF IT. Each app is re-provisioned with EnsureAppDatabase and the
// result written with SetSecretValue — the same two calls AttachAddon makes, in the same order —
// because the cutover has to leave an app in exactly the state a fresh attach would, and a second
// implementation of "give this app a working URL" would be a second thing to keep correct.
//
// RE-PROVISIONING IS REQUIRED RATHER THAN OPTIONAL, which is the part worth stating. A recovered
// instance carries the roles as they were at the RECOVERY TARGET: a password rotated since is not
// there, and an app attached since has no role at all. The URL an app is holding therefore may simply
// not authenticate any more, and it would fail at the app's next connection rather than here.
// EnsureAppDatabase rewrites the role's password and waits until the instance actually accepts it, so
// an app that comes out of this reconnected has a credential that works against what is on the
// instance now, and one that does not is reported as stranded here rather than discovered later.
//
// WHAT IT DOES NOT REPAIR is a database the rewind went back past the creation of. Provisioning is
// declarative now (ADR-0066 §2), and the operator re-applies an object when its spec changes — which
// a re-provision of an unchanged attachment does not do. Such an app fails the credential check
// above and is reported stranded with the re-attach that fixes it, rather than silently reconnected
// to a database that is not there.
//
// A failure is recorded per app rather than aborting. The instance is up and holds the data; stopping
// at the first app that would not reconnect would leave the rest holding stale credentials with
// nothing reported about them.
func (e *Engine) cutOverApps(ctx context.Context, t AddonType, targetEnv, ns string, apps []string) (reconnected []string, stranded []StrandedApp) {
	k := e.k8s.WithNamespace(ns)
	for _, app := range apps {
		// The key THIS app is attached under. Writing the default here would leave an app that chose
		// its own name reading a credential the recovery point no longer honours while a second,
		// unread variable held the working one (issue #462).
		key, err := e.db.AddonEnvKey(ctx, string(AddonPostgres), app, targetEnv)
		if err != nil {
			stranded = append(stranded, StrandedApp{App: app, Reason: fmt.Sprintf("the variable its connection string is written under could not be read, so nothing was rewritten for it; re-attach it with `burrow addon attach %s %s --env %s`", t, app, targetEnv)})
			continue
		}
		// The returned url is a SECRET value: from here it is handed only to SetSecretValue and never
		// logged, audited, or returned.
		url, err := e.dbProvisioner.EnsureAppDatabase(ctx, app, targetEnv)
		if err != nil {
			stranded = append(stranded, StrandedApp{App: app, Reason: fmt.Sprintf("its database and role could not be re-provisioned on the recovered instance; re-attach it with `burrow addon attach %s %s --env %s`", t, app, targetEnv)})
			continue
		}
		if err := k.SetSecretValue(ctx, app, key, url); err != nil {
			// The error names the app and key only — never the value.
			stranded = append(stranded, StrandedApp{App: app, Reason: fmt.Sprintf("its %s could not be written; re-attach it with `burrow addon attach %s %s --env %s`", key, t, app, targetEnv)})
			continue
		}
		if err := k.RestartWorkload(ctx, app, e.clock.Now()); err != nil && !errors.Is(err, ErrNotFound) {
			// The credential IS written, so this app is connected — it is the roll that did not
			// happen, and the pods are still holding the previous value until something restarts them.
			stranded = append(stranded, StrandedApp{App: app, Reason: "its connection string was rewritten but the app could not be restarted onto it, so its pods are still using the previous one"})
			continue
		}
		reconnected = append(reconnected, app)
	}
	return reconnected, stranded
}

// emptyIfNil normalizes a nil app list to an empty one, so the JSON shape is stable.
func emptyIfNil(apps []string) []string {
	if apps == nil {
		return []string{}
	}
	return apps
}

// strandedNames renders the stranded apps as names for the audit row.
func strandedNames(stranded []StrandedApp) []string {
	out := make([]string, len(stranded))
	for i, s := range stranded {
		out[i] = s.App
	}
	return out
}

// restoreInstanceConsequence renders what the rewind will actually do, for the guardrail's held
// confirmation (ADR-0064 §3's standard applied here): the instance by name, the environment, the
// point it is going back to, and the apps whose databases go with it — BY NAME, not counted.
//
// A count is not consent. "3 apps are affected" tells an operator nothing they can weigh; "api, web
// and worker all go back to that point" is the sentence that makes someone stop when one of those
// names should not be on the list.
func restoreInstanceConsequence(instance, env, target string, apps []string) string {
	msg := fmt.Sprintf("rewinding the whole postgres instance %q in environment %s to %s", instance, env, target)
	switch len(apps) {
	case 0:
		return msg + " (no app is attached to it)"
	case 1:
		return msg + fmt.Sprintf(" — the database of %s goes back to that point with it, and everything written since is gone", apps[0])
	default:
		return msg + fmt.Sprintf(" — the databases of all %d attached apps (%s) go back to that point with it, and everything written since is gone", len(apps), strings.Join(apps, ", "))
	}
}
