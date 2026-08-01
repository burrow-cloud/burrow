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

// These tests cover ADR-0066 §2's engine half: Burrow asks CloudNativePG for a base backup of a whole
// instance and records what it observed.
//
// The two that matter most are TestBackupInstanceReadsTheBackupBackBeforeCompleting and
// TestRestoreRefusesAPhysicalBackup. The first is ADR-0063 §7 arriving on a repository Burrow did not
// write itself: a completed `Backup` object is the operator's word that pgBackRest exited zero, and
// the row is only allowed to say completed once the store served the result back. The second is
// ADR-0066 §4's granularity mismatch made unreachable rather than merely documented.

// newPhysicalBackupEngine is newBackupDestinationEngine with the object-store FACTORY returned as
// well, because a physical backup's read-back is verified against what that store actually holds.
func newPhysicalBackupEngine(t *testing.T) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Credentials, *fake.ObjectStoreFactory) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	creds := fake.NewCredentials()
	osf := fake.NewObjectStoreFactory()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock: fake.NewClock(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: fake.NewDNSFactory(),
		ObjectStore: osf, DatabaseProvisioner: fake.NewProvisioner(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, d, creds, osf
}

// installArchivingPostgres installs a Postgres instance with an object-storage provider registered,
// so the fake cluster's instance archives and a physical backup of it is possible.
func installArchivingPostgres(t *testing.T, e *cp.Engine, d *fake.Database, creds *fake.Credentials, env string) {
	t.Helper()
	seedObjectStoreProvider(t, d, creds, "backups")
	if _, err := e.InstallAddon(context.Background(), cp.AddonPostgres, env, cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
}

// seedManifest puts the object a completed physical backup's read-back looks for into the fake store,
// at exactly the key the engine derives, so the verification finds what a working repository holds.
func seedManifest(t *testing.T, f *fake.ObjectStoreFactory, env, label string) {
	t.Helper()
	instance, err := cp.AddonInstanceName(cp.AddonPostgres, env)
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	key := cp.PgBackRestManifestKey(env, instance, label)
	if err := f.Store.CreateBucket(context.Background(), "burrow-backups-backups"); err != nil {
		t.Fatalf("seeding the repository bucket: %v", err)
	}
	if err := f.Store.PutObject(context.Background(), "burrow-backups-backups", key, []byte("manifest")); err != nil {
		t.Fatalf("seeding the backup manifest: %v", err)
	}
}

// TestBackupInstanceRecordsAPhysicalRow asserts a successful physical backup is recorded as what it
// is: physical, belonging to NO app, at the object-store destination, and therefore Durable() — the
// predicate `--delete-data` and the backup-age signal both stand on.
func TestBackupInstanceRecordsAPhysicalRow(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	k.SetPhysicalBackupLabel("20260801-020000F")
	seedManifest(t, osf, cp.DefaultEnvironment, "20260801-020000F")

	res, err := e.BackupInstance(ctx, cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupInstance: %v", err)
	}
	b := res.Backup
	switch {
	case b.Kind != cp.BackupKindPhysical:
		t.Errorf("kind = %q, want %q", b.Kind, cp.BackupKindPhysical)
	case b.App != "":
		t.Errorf("app = %q, want empty: a physical backup covers every database on the instance", b.App)
	case b.Status != cp.BackupCompleted:
		t.Errorf("status = %q, want completed", b.Status)
	case !b.Durable():
		t.Errorf("backup %+v is not Durable(), but it completed at an object store", b)
	}
	if !strings.Contains(b.ObjectKey, "20260801-020000F") {
		t.Errorf("object key = %q, want the pgBackRest label in it", b.ObjectKey)
	}
	// And it is on the row the store holds, not only on the returned value.
	stored, err := d.GetBackup(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if stored.Kind != cp.BackupKindPhysical || stored.Status != cp.BackupCompleted || stored.ObjectKey != b.ObjectKey {
		t.Errorf("stored row = %+v, want a completed physical row carrying the object key", stored)
	}
}

// TestBackupInstanceReadsTheBackupBackBeforeCompleting is ADR-0063 §7 on the physical path: when the
// repository will not serve the backup's manifest back, NOTHING records a completed backup. The row
// is failed, with ObjectNotReadable — the reason that exists precisely because "the tool exited zero"
// and "the object is there" are two facts.
func TestBackupInstanceReadsTheBackupBackBeforeCompleting(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	k.SetPhysicalBackupLabel("20260801-020000F")
	// The manifest is deliberately NOT seeded: CloudNativePG says completed, the store has nothing.

	_ = osf
	if _, err := e.BackupInstance(ctx, cp.AddonPostgres, "", ""); err == nil {
		t.Fatal("BackupInstance must fail when the backup cannot be read back")
	}
	list, err := d.ListBackups(ctx, "", "")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("recorded backups = %d, want 1", len(list))
	}
	got := list[0]
	if got.Status != cp.BackupFailed || got.FailureReason != cp.BackupReasonObjectNotReadable {
		t.Errorf("row = %q/%q, want failed/%s", got.Status, got.FailureReason, cp.BackupReasonObjectNotReadable)
	}
	if got.Durable() {
		t.Error("a backup that could not be read back must not be Durable()")
	}
}

// TestBackupInstanceRefusesWithoutADestination asserts the physical path has no in-cluster tier. A
// logical dump falls back to the volume when no object storage is registered; a base backup has
// nowhere to fall back to, so it refuses and names both the fix and the path that still works.
func TestBackupInstanceRefusesWithoutADestination(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	_, err := e.BackupInstance(ctx, cp.AddonPostgres, "", "")
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("BackupInstance with no provider = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "burrow addon backup postgres") {
		t.Errorf("refusal must name the per-app path that still works: %v", err)
	}
	if calls := k.PhysicalBackups(); len(calls) != 0 {
		t.Errorf("no Backup object should have been created, got %d", len(calls))
	}
}

// TestBackupInstanceRecordsTheReasonTheBackupObjectGave asserts a `Backup` object that failed is
// recorded with the reason the seam reported rather than a wrapped error string — and that the
// vocabulary is the existing closed one, so a caller branches instead of parsing.
func TestBackupInstanceRecordsTheReasonTheBackupObjectGave(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	_ = osf
	k.SetPhysicalBackupFailure(cp.BackupReasonStoreUnreachable, "the instance's write-ahead log is not reaching the repository")
	k.SetError(fake.OpRunPhysicalBackup, errors.New("kube: Backup \"burrow-pg-backup-1\" is not archiving"))

	if _, err := e.BackupInstance(ctx, cp.AddonPostgres, "", ""); err == nil {
		t.Fatal("BackupInstance must fail when the Backup object does")
	}
	list, err := d.ListBackups(ctx, "", "")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 1 || list[0].Status != cp.BackupFailed {
		t.Fatalf("recorded backups = %+v, want one failed", list)
	}
	if list[0].FailureReason != cp.BackupReasonStoreUnreachable {
		t.Errorf("reason = %q, want %q: a store that is not accepting the archive is not the same failure as a backup command that would not run",
			list[0].FailureReason, cp.BackupReasonStoreUnreachable)
	}
}

// TestRestoreRefusesAPhysicalBackup is ADR-0066 §4's granularity mismatch made unreachable. Physical
// recovery rewinds the whole instance, so honouring `restore <app> --backup <physical id>` would roll
// back every other app sharing it — a cross-app data loss asked for by somebody who picked the most
// recent id off a listing.
func TestRestoreRefusesAPhysicalBackup(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	k.SetPhysicalBackupLabel("20260801-020000F")
	seedManifest(t, osf, cp.DefaultEnvironment, "20260801-020000F")

	res, err := e.BackupInstance(ctx, cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupInstance: %v", err)
	}
	err = e.RestoreAddon(ctx, cp.AddonPostgres, "web", res.Backup.ID, "", true)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("RestoreAddon from a physical backup = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "PHYSICAL") {
		t.Errorf("refusal must say what the backup IS: %v", err)
	}
	if jobs := k.RestoreJobs(); len(jobs) != 0 {
		t.Errorf("no restore Job should have run, got %d", len(jobs))
	}
	_ = d
}

// TestBackupHealthCountsAPhysicalBackup asserts the ADR-0063 §7 / ADR-0066 §5 status surface reports
// a physical backup as the last one that left the cluster. The surface is deliberately independent of
// the mechanism — it reads Burrow's own rows — and this is the assertion that it stayed that way when
// a second mechanism arrived.
func TestBackupHealthCountsAPhysicalBackup(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newPhysicalBackupEngine(t)
	installArchivingPostgres(t, e, d, creds, cp.DefaultEnvironment)
	k.SetPhysicalBackupLabel("20260801-020000F")
	seedManifest(t, osf, cp.DefaultEnvironment, "20260801-020000F")
	if _, err := e.BackupInstance(ctx, cp.AddonPostgres, "", ""); err != nil {
		t.Fatalf("BackupInstance: %v", err)
	}

	health, err := e.BackupHealth(ctx, cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if health.State != cp.BackupHealthDurable {
		t.Errorf("state = %q, want %q", health.State, cp.BackupHealthDurable)
	}
	if health.LastDurableSuccess == nil || health.LastDurableSuccess.Kind != cp.BackupKindPhysical {
		t.Errorf("last durable success = %+v, want a physical one", health.LastDurableSuccess)
	}
}
