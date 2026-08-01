// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// These tests cover ADR-0066 §4's engine half: a physical restore rewinds a whole instance, and every
// property that keeps that from being reachable by accident.
//
// The ones that matter most are TestRestoreInstanceRefusesALogicalBackup — the mirror of slice 5's
// refusal, so the two paths point at each other rather than at nothing — and
// TestRestoreInstanceAbortsWhenTheSafetyBackupFails, which is ADR-0064 §5's ordering on the one
// operation that destroys a database on purpose: nothing is destroyed until a copy is known to exist.

// seedAttachedApp gives an app a workload and a DATABASE_URL in the environment's namespace, which is
// what makes it an app ON the instance for the blast-radius enumeration. The value is a marker, never
// asserted on as a credential.
func seedAttachedApp(t *testing.T, k *fake.Kubernetes, ns, app string) {
	t.Helper()
	view := k
	if ns != "" {
		view = k.WithNamespace(ns).(*fake.Kubernetes)
	}
	if err := view.ApplyWorkload(context.Background(), cp.WorkloadSpec{App: app, Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload %s: %v", app, err)
	}
	view.SetSecret(app, "DATABASE_URL", "postgres://old")
}

// takePhysicalBackup runs a real physical backup through the engine so the test has a recorded row of
// the exact shape a restore will be handed — rather than a hand-built one that could disagree with
// what BackupInstance actually writes.
func takePhysicalBackup(t *testing.T, e *cp.Engine, k *fake.Kubernetes, osf *fake.ObjectStoreFactory, env, label string) cp.Backup {
	t.Helper()
	k.SetPhysicalBackupLabel(label)
	seedManifest(t, osf, env, label)
	res, err := e.BackupInstance(context.Background(), cp.AddonPostgres, env, "")
	if err != nil {
		t.Fatalf("BackupInstance: %v", err)
	}
	return res.Backup
}

// TestRestoreInstanceRewindsTheInstanceAndReconnectsEveryApp is the happy path, and every assertion
// in it is one of ADR-0066 §4's four steps: the seam was asked to recover the named point, a backup of
// what was there was taken first, and every app came out re-provisioned onto the recovered instance
// and restarted.
func TestRestoreInstanceRewindsTheInstanceAndReconnectsEveryApp(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	seedAttachedApp(t, k, "", "api")
	seedAttachedApp(t, k, "", "web")
	backup := takePhysicalBackup(t, e, k, osf, cp.DefaultEnvironment, "20260801-020000F")

	res, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Backup: backup.ID, Confirm: true})
	if err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}

	// The blast radius is recorded by NAME, which is the whole reason the result carries a list.
	if got := strings.Join(res.Apps, ","); got != "api,web" {
		t.Errorf("apps = %q, want %q", got, "api,web")
	}
	if got := strings.Join(res.Reconnected, ","); got != "api,web" {
		t.Errorf("reconnected = %q, want both apps", got)
	}
	if len(res.Stranded) != 0 {
		t.Errorf("stranded = %+v, want none", res.Stranded)
	}
	// A backup of the pre-restore state was taken, and it is a real recorded row rather than a claim.
	if res.SafetyBackup == "" {
		t.Fatal("no safety backup was recorded, so this restore destroyed the only copy of the state it replaced")
	}
	safety, err := d.GetBackup(ctx, res.SafetyBackup)
	if err != nil {
		t.Fatalf("GetBackup(safety): %v", err)
	}
	if safety.Kind != cp.BackupKindPhysical || !safety.Durable() {
		t.Errorf("safety backup = %+v, want a durable physical row", safety)
	}
	// The seam was asked for the point the operator named, at the repository that was resolved.
	calls := k.RestoreInstances()
	if len(calls) != 1 {
		t.Fatalf("RestoreInstance calls = %d, want 1", len(calls))
	}
	if calls[0].BackupLabel != "20260801-020000F" {
		t.Errorf("recovery label = %q, want pgBackRest's own label for the named backup", calls[0].BackupLabel)
	}
	if calls[0].Provider != "backups" {
		t.Errorf("repository = %q, want the registered provider", calls[0].Provider)
	}
	// And the cutover went through the attach path: a fresh URL per app, written into the app's own
	// Secret. A recovered instance holds the roles as they were at the recovery point, so the value an
	// app was holding is exactly what must not survive this.
	for _, app := range []string{"api", "web"} {
		if got, _ := k.SecretValue(app, "DATABASE_URL"); got != fake.URLFor(app, cp.DefaultEnvironment) {
			t.Errorf("%s DATABASE_URL was not reissued for the recovered instance: got %q", app, got)
		}
		if _, ok := k.RestartedAt(app); !ok {
			t.Errorf("%s was not restarted onto the recovered instance", app)
		}
	}
}

// TestRestoreInstanceRefusesALogicalBackup is the mirror of the refusal slice 5 put on `addon
// restore <addon> <app>`, and the pair is the point: each path refuses the other's backup BY NAME and
// says which command to use, so an id picked off a listing under pressure cannot silently mean the
// wrong granularity in either direction.
func TestRestoreInstanceRefusesALogicalBackup(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, _ := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	seedAttachedApp(t, k, "", "api")

	dump, err := e.BackupAddon(ctx, cp.AddonPostgres, "api", "", "")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	if dump.Backup.Kind != cp.BackupKindLogical {
		t.Fatalf("kind = %q, want a logical dump to refuse", dump.Backup.Kind)
	}

	_, err = e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Backup: dump.Backup.ID, Confirm: true})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("RestoreInstance from a logical dump: got %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "burrow addon restore") {
		t.Errorf("refusal does not name the per-app command that DOES take this backup: %v", err)
	}
	// And nothing was rewound.
	if calls := k.RestoreInstances(); len(calls) != 0 {
		t.Errorf("the seam was called %d times on a refused restore, want 0", len(calls))
	}
}

// TestRestoreInstanceRefusesAnotherEnvironmentsBackup is ADR-0067 §1's isolation on this path: each
// environment has its own instance and its own repository, so another environment's base backup is
// not a source this one can recover from — and reaching it would replace production's data with
// staging's, which is the class of incident issue #339 was.
func TestRestoreInstanceRefusesAnotherEnvironmentsBackup(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, "staging", cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon(staging): %v", err)
	}
	staging := takePhysicalBackup(t, e, k, osf, "staging", "20260801-030000F")

	_, err := e.RestoreInstance(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.RestoreInstanceOptions{Backup: staging.ID, Confirm: true})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("restoring prod from staging's backup: got %v, want ErrInvalid", err)
	}
	if calls := k.RestoreInstances(); len(calls) != 0 {
		t.Errorf("the seam was called on a cross-environment restore, want 0 calls")
	}
}

// TestRestoreInstanceRefusesAnUnverifiedBackup keeps ADR-0063 §7 load-bearing at the one moment it
// cannot be corrected. A pending or failed row is a backup Burrow never read back out of the store,
// and replacing a live instance from bytes nothing has verified is how a backup regime turns out to
// have been decorative.
func TestRestoreInstanceRefusesAnUnverifiedBackup(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, _ := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	// A physical row that never completed: the read-back found nothing at the key.
	k.SetPhysicalBackupLabel("20260801-020000F")
	if _, err := e.BackupInstance(ctx, cp.AddonPostgres, "", ""); err == nil {
		t.Fatal("BackupInstance with no manifest in the store: want a failure")
	}
	rows, err := d.ListBackups(ctx, "", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListBackups = %d rows, %v; want the one failed row", len(rows), err)
	}

	_, err = e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Backup: rows[0].ID, Confirm: true})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("restoring from an unverified backup: got %v, want ErrInvalid", err)
	}
}

// TestRestoreInstanceRefusesWithNoRecoveryTarget pins the deliberate absence of a default. "The
// newest state" and "the moment before the bad migration" have the same blast radius, so choosing
// one because the caller named neither would be the most destructive default in the product.
func TestRestoreInstanceRefusesWithNoRecoveryTarget(t *testing.T) {
	ctx := context.Background()
	e, _, d, creds, _ := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)

	if _, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("no target: got %v, want ErrInvalid", err)
	}
	if _, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Latest: true, ToTime: "2026-08-01T00:00:00Z", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("two targets: got %v, want ErrInvalid", err)
	}
	if _, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{ToTime: "yesterday", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("unparseable time: got %v, want ErrInvalid", err)
	}
}

// TestRestoreInstanceRefusesWithNoObjectStorage: a physical restore reads out of an object-storage
// repository and there is no in-cluster tier for one, so an instance that archives nowhere has
// nothing to recover from. The refusal names the per-app path, which DOES have one.
func TestRestoreInstanceRefusesWithNoObjectStorage(t *testing.T) {
	ctx := context.Background()
	e, _, d, _, _ := newPhysicalBackupEngine(t)
	d.SetPolicy(permissive())
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}

	_, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Latest: true, Confirm: true})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("restore with no object storage: got %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "burrow addon restore") {
		t.Errorf("refusal does not name the path that works without object storage: %v", err)
	}
}

// TestRestoreInstanceAbortsWhenTheSafetyBackupFails is ADR-0064 §5's ordering, and it is the property
// that makes a rewind to the wrong point survivable: the copy of what is there goes into the
// repository FIRST, and if it does not get there nothing is destroyed.
func TestRestoreInstanceAbortsWhenTheSafetyBackupFails(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, _ := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	// No manifest is seeded, so the safety backup's read-back finds nothing and the backup fails.
	k.SetPhysicalBackupLabel("20260801-020000F")

	_, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Latest: true, Confirm: true})
	if err == nil {
		t.Fatal("RestoreInstance with a failing safety backup: want a refusal")
	}
	if !strings.Contains(err.Error(), "--skip-safety-backup") {
		t.Errorf("the refusal does not name the way past it for a wedged instance: %v", err)
	}
	if calls := k.RestoreInstances(); len(calls) != 0 {
		t.Fatalf("the instance was rewound after the safety backup failed: %d seam calls, want 0", len(calls))
	}
}

// TestRestoreInstanceCanSkipTheSafetyBackup: the override exists because an instance is often
// restored BECAUSE it is broken, and one that cannot take a base backup would otherwise be the one
// instance that cannot be restored. What it must never do is go quiet about it.
func TestRestoreInstanceCanSkipTheSafetyBackup(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, _ := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	k.SetPhysicalBackupLabel("20260801-020000F") // no manifest seeded: a backup here would fail

	res, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Latest: true, SkipSafetyBackup: true, Confirm: true})
	if err != nil {
		t.Fatalf("RestoreInstance --skip-safety-backup: %v", err)
	}
	if res.SafetyBackup != "" {
		t.Errorf("safety backup = %q, want none", res.SafetyBackup)
	}
	if !strings.Contains(res.SafetyBackupNote, "skip-safety-backup") {
		t.Errorf("the result does not say plainly that no copy was taken: %q", res.SafetyBackupNote)
	}
	if calls := k.RestoreInstances(); len(calls) != 1 || calls[0].BackupLabel != "" || calls[0].TargetTime != "" {
		t.Errorf("seam calls = %+v, want one recovery to the newest state (no target)", calls)
	}
}

// TestRestoreInstanceIsHeldForConfirmationNamingEveryApp: the guardrail message is the last thing a
// human reads before a rewind, so it has to state the blast radius rather than assert one. A COUNT IS
// NOT CONSENT — the names are what make somebody stop when one of them should not be on the list.
func TestRestoreInstanceIsHeldForConfirmationNamingEveryApp(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, _ := newPhysicalBackupEngine(t)
	d.SetPolicy(cp.DefaultPolicy())
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	seedAttachedApp(t, k, "", "api")
	seedAttachedApp(t, k, "", "worker")

	_, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Latest: true})
	mustGuardrail(t, err, cp.GuardrailAddonRestoreInstance)
	for _, want := range []string{"api", "worker", "burrow-postgres"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the held confirmation does not name %q: %v", want, err)
		}
	}
	if calls := k.RestoreInstances(); len(calls) != 0 {
		t.Errorf("a held restore reached the cluster: %d seam calls, want 0", len(calls))
	}
}

// TestRestoreInstanceAuditsTheBlastRadius: the audit row records what was restored, to what point,
// and which apps were affected — the three questions asked after the fact — and no credential.
func TestRestoreInstanceAuditsTheBlastRadius(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	seedAttachedApp(t, k, "", "api")
	backup := takePhysicalBackup(t, e, k, osf, cp.DefaultEnvironment, "20260801-020000F")

	if _, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Backup: backup.ID, Confirm: true}); err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}
	entries, err := d.Audit(ctx, cp.AuditFilter{})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var executed *cp.AuditEntry
	for i := range entries {
		if entries[i].Operation == "addon_restore_instance" && entries[i].Outcome == cp.AuditExecuted {
			executed = &entries[i]
		}
	}
	if executed == nil {
		t.Fatal("no executed addon_restore_instance audit row")
	}
	if executed.Args["apps"] != "api" {
		t.Errorf("audit apps = %q, want the affected apps by name", executed.Args["apps"])
	}
	if !strings.Contains(executed.Args["target"], backup.ID) {
		t.Errorf("audit target = %q, want the point that was restored to", executed.Args["target"])
	}
	if executed.Args["safety_backup"] == "" {
		t.Errorf("audit row does not record the backup taken of the state that was replaced")
	}
	if executed.Args["instance"] != "burrow-postgres" {
		t.Errorf("audit instance = %q, want the instance that was rewound", executed.Args["instance"])
	}
}

// TestRestoreInstanceReportsAnAppItCouldNotReconnect: the cutover is best-effort PER APP and never
// aborts the restore. The instance is up holding the data; stopping at the first app that would not
// reconnect would leave the rest holding stale credentials with nothing said about them.
func TestRestoreInstanceReportsAnAppItCouldNotReconnect(t *testing.T) {
	ctx := context.Background()
	_, k, d, creds, _ := newPhysicalBackupEngine(t)
	p := fake.NewProvisioner()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)), IDs: fake.NewIDs(),
		Resolver: fake.NewResolver(), Credentials: creds, DNS: fake.NewDNSFactory(),
		ObjectStore: fake.NewObjectStoreFactory(), DatabaseProvisioner: p,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	seedAttachedApp(t, k, "", "api")
	p.SetEnsureError(errors.New("role could not be created"))

	res, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Latest: true, SkipSafetyBackup: true, Confirm: true})
	if err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}
	if len(res.Stranded) != 1 || res.Stranded[0].App != "api" {
		t.Fatalf("stranded = %+v, want api reported", res.Stranded)
	}
	if !strings.Contains(res.Stranded[0].Reason, "burrow addon attach") {
		t.Errorf("the stranded app's line does not say how to reconnect it: %q", res.Stranded[0].Reason)
	}
	// And no part of the reason is a connection string.
	if strings.Contains(res.Stranded[0].Reason, "postgres://") {
		t.Errorf("a connection string reached the result: %q", res.Stranded[0].Reason)
	}
}

// TestPgBackRestLabelFromManifestKeyIsTheInverse pins the derivation a restore names its base backup
// with. It is the inverse of the key a completed physical backup records, and a change to one that
// did not change the other would leave a recovery naming a backup the repository has never heard of.
func TestPgBackRestLabelFromManifestKeyIsTheInverse(t *testing.T) {
	key := cp.PgBackRestManifestKey(cp.PgBackRestRepoPath("staging"), "burrow-postgres-staging", "20260210-101333F")
	if got := cp.PgBackRestLabelFromManifestKey(key); got != "20260210-101333F" {
		t.Errorf("label from %q = %q, want the pgBackRest label", key, got)
	}
	for _, notAKey := range []string{"", "burrow/backups/prod/api/b1.dump", "backup.manifest"} {
		if got := cp.PgBackRestLabelFromManifestKey(notAKey); got != "" {
			t.Errorf("label from %q = %q, want empty", notAKey, got)
		}
	}
}

// TestRestoreInstanceNamesTheRepositoryForTheSafetyBackupToo is the fix for a bug worth keeping a
// test on. The safety backup is a physical backup, and a physical backup resolves a destination — so
// with SEVERAL object-storage providers registered, a restore that named its repository and then let
// the safety backup resolve one from nothing would be refused for ambiguity. The refusal's own advice
// is `--skip-safety-backup`, which means the failure mode was pushing an operator into rewinding with
// no copy at all.
func TestRestoreInstanceNamesTheRepositoryForTheSafetyBackupToo(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	// A second registered destination, which is a supported state (ADR-0063 §6).
	seedObjectStoreProvider(t, d, creds, "elsewhere")
	k.SetPhysicalBackupLabel("20260801-020000F")
	seedManifest(t, osf, cp.DefaultEnvironment, "20260801-020000F")

	res, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{
		Latest: true, Destination: "backups", Confirm: true,
	})
	if err != nil {
		t.Fatalf("RestoreInstance with two providers registered and one named: %v", err)
	}
	if res.SafetyBackup == "" {
		t.Fatal("no safety backup was taken, so the rewind destroyed the only copy of what was there")
	}
	safety, err := d.GetBackup(ctx, res.SafetyBackup)
	if err != nil {
		t.Fatalf("GetBackup(safety): %v", err)
	}
	if safety.Provider != "backups" {
		t.Errorf("safety backup provider = %q, want the repository the restore named", safety.Provider)
	}
	// And with NO destination named, the ambiguity is refused rather than guessed at — with nothing
	// destroyed, since the resolution happens before the seam is reached.
	if _, err := e.RestoreInstance(ctx, cp.AddonPostgres, "", cp.RestoreInstanceOptions{Latest: true, Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("two providers and none named: got %v, want ErrInvalid", err)
	}
}
