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

// These tests cover removing a backup: the dump and its record together, and — more importantly —
// what is left behind when only one of the two comes off. The order is the decision under test, so
// most of what is asserted here is about failure: the row is the thing that must survive a partial
// removal, because a row without bytes is visible in every listing while bytes without a row are
// findable by nobody.

// newBackupDeleteEngine builds an engine with the add-on, object-store and credential seams wired,
// and a registered Postgres instance to take backups from. It hands back the object-store factory as
// well, because half of these tests are about the object a shipped dump leaves at a vendor.
func newBackupDeleteEngine(t *testing.T) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Credentials, *fake.ObjectStoreFactory) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	creds := fake.NewCredentials()
	osf := fake.NewObjectStoreFactory()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock: fake.NewClock(time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: fake.NewDNSFactory(),
		ObjectStore: osf, DatabaseProvisioner: fake.NewProvisioner(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	installPostgresIn(t, e, cp.DefaultEnvironment)
	return e, k, d, creds, osf
}

// takeBackup runs one backup of app and returns the recorded row.
func takeBackup(t *testing.T, e *cp.Engine, app string) cp.Backup {
	t.Helper()
	res, err := e.BackupAddon(context.Background(), cp.AddonPostgres, app, "", "", "")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	return res.Backup
}

// TestDeleteBackupRemovesDumpAndRecord is the whole point of the seam: after one call, neither half
// of the backup is left. The removal is driven from what the ROW recorded — its volume and its path —
// rather than from a derivation, which is what keeps a dump taken before backups were per-environment
// reachable.
func TestDeleteBackupRemovesDumpAndRecord(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, _ := newBackupDeleteEngine(t)
	b := takeBackup(t, e, "web")

	if err := e.DeleteBackup(ctx, cp.AddonPostgres, b.ID); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}

	removals := k.BackupRemovals()
	if len(removals) != 1 {
		t.Fatalf("RemoveBackupFile calls = %d, want 1", len(removals))
	}
	if removals[0].BackupID != b.ID || removals[0].Volume != b.Volume || removals[0].Path != b.Path {
		t.Errorf("removal = %+v, want the row's own id/volume/path (%s, %s, %s)", removals[0], b.ID, b.Volume, b.Path)
	}
	if removals[0].Volume == "" || removals[0].Path == "" {
		t.Error("the removal was driven with no volume or no path, so it names no file")
	}

	if _, err := d.GetBackup(ctx, b.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("GetBackup after delete = %v, want ErrNotFound", err)
	}
	list, err := d.ListBackups(ctx, "web", "")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("ListBackups = %d rows after removing the only backup, want 0", len(list))
	}

	// Removing a backup is destructive and is recorded, so "what happened to last month's backups"
	// is answerable after the fact (ADR-0027).
	if !hasAuditOp(d, "addon_backup_delete", cp.AuditExecuted) {
		t.Error("removing a backup recorded no executed audit row")
	}
}

// TestDeleteBackupRemovesShippedObjectAndFile asserts both copies go. A durable destination is a
// TIER, not an alternative: the dump lands on the claim and is shipped from there (ADR-0063 §7), so
// a removal that only deleted the object would leave the claim filling up with the very dumps
// retention was run to clear.
func TestDeleteBackupRemovesShippedObjectAndFile(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds, osf := newBackupDeleteEngine(t)
	seedObjectStoreProvider(t, d, creds, "backups")

	b := takeBackup(t, e, "web")
	if b.Destination != cp.BackupDestinationObjectStore || b.ObjectKey == "" {
		t.Fatalf("backup = %+v, want a shipped backup with an object key", b)
	}

	if err := e.DeleteBackup(ctx, cp.AddonPostgres, b.ID); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}

	if len(k.BackupRemovals()) != 1 {
		t.Errorf("RemoveBackupFile calls = %d, want 1 — the dump on the claim is not removed by deleting the object", len(k.BackupRemovals()))
	}
	deleted := osf.Store.Deleted
	if len(deleted) != 1 || deleted[0] != b.ObjectKey {
		t.Errorf("objects deleted = %v, want [%s]", deleted, b.ObjectKey)
	}
	if _, err := d.GetBackup(ctx, b.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("GetBackup after delete = %v, want ErrNotFound", err)
	}
}

// TestDeleteBackupAlreadyGoneIsNotAnError is the idempotence the callers of this seam need. A
// retention pass retried after a crash, and two passes racing over one expired backup, both land on
// a backup that is already gone; if that were ErrNotFound every caller would wrap it in a check that
// means "fine", and one of those checks would eventually be written wrong.
func TestDeleteBackupAlreadyGoneIsNotAnError(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, _ := newBackupDeleteEngine(t)
	b := takeBackup(t, e, "web")

	if err := e.DeleteBackup(ctx, cp.AddonPostgres, b.ID); err != nil {
		t.Fatalf("first DeleteBackup: %v", err)
	}
	if err := e.DeleteBackup(ctx, cp.AddonPostgres, b.ID); err != nil {
		t.Fatalf("second DeleteBackup = %v, want nil: removing a backup that is already gone is success", err)
	}
	// An id that never existed is the same answer, for the same reason.
	if err := e.DeleteBackup(ctx, cp.AddonPostgres, "never-existed"); err != nil {
		t.Fatalf("DeleteBackup of an unknown id = %v, want nil", err)
	}

	if n := len(k.BackupRemovals()); n != 1 {
		t.Errorf("RemoveBackupFile calls = %d, want 1: a removal of a row that is gone drives no Job", n)
	}
	// And no audit row claims a second removal happened.
	if n := countAuditOp(d, "addon_backup_delete"); n != 1 {
		t.Errorf("audit rows for addon_backup_delete = %d, want 1 — a no-op must not record a deletion", n)
	}
}

// TestDeleteBackupRefusesOneStillBeingTaken protects the dump a Job is writing right now. The row's
// status alone cannot answer this, which is why the cluster is asked.
func TestDeleteBackupRefusesOneStillBeingTaken(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, _ := newBackupDeleteEngine(t)

	running := cp.Backup{
		ID: "bk-running", Kind: cp.BackupKindLogical, App: "web", Environment: cp.DefaultEnvironment,
		CreatedAt: time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC),
		Volume:    cp.PostgresBackupVolume, Path: cp.BackupPath("web", "bk-running"),
		Status: cp.BackupPending, Destination: cp.BackupDestinationCluster,
	}
	if err := d.RecordBackup(ctx, running); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	k.SetBackupJob(running.ID, true)

	err := e.DeleteBackup(ctx, cp.AddonPostgres, running.ID)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("DeleteBackup of a running backup = %v, want ErrInvalid", err)
	}
	if len(k.BackupRemovals()) != 0 {
		t.Error("the dump of a running backup was removed underneath its Job")
	}
	if _, err := d.GetBackup(ctx, running.ID); err != nil {
		t.Errorf("the row of a running backup was removed: %v", err)
	}
}

// TestDeleteBackupRemovesAbandonedPendingRow is the other half of the same question. A row left
// pending by a burrowd that restarted mid-backup (ADR-0074 §6) will never complete, and it is
// exactly the debris retention exists to clear — so it must not be protected by the status it is
// stuck in.
func TestDeleteBackupRemovesAbandonedPendingRow(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, _ := newBackupDeleteEngine(t)

	abandoned := cp.Backup{
		ID: "bk-abandoned", Kind: cp.BackupKindLogical, App: "web", Environment: cp.DefaultEnvironment,
		CreatedAt: time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC),
		Volume:    cp.PostgresBackupVolume, Path: cp.BackupPath("web", "bk-abandoned"),
		Status: cp.BackupPending, Destination: cp.BackupDestinationCluster,
	}
	if err := d.RecordBackup(ctx, abandoned); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	k.SetBackupJob(abandoned.ID, false)

	if err := e.DeleteBackup(ctx, cp.AddonPostgres, abandoned.ID); err != nil {
		t.Fatalf("DeleteBackup of an abandoned pending row: %v", err)
	}
	if _, err := d.GetBackup(ctx, abandoned.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("GetBackup after delete = %v, want ErrNotFound", err)
	}
}

// TestDeleteBackupRefusesPhysical keeps Burrow out of a pgBackRest repository. A physical backup's
// bytes are the repository's, expired by its own retention; unlinking one object out of a stanza
// damages the backups either side of it, so the refusal is the correct outcome rather than a
// limitation to work around.
func TestDeleteBackupRefusesPhysical(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, osf := newBackupDeleteEngine(t)

	physical := cp.Backup{
		ID: "bk-physical", Kind: cp.BackupKindPhysical, Environment: cp.DefaultEnvironment,
		CreatedAt: time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC),
		Status:    cp.BackupCompleted, Destination: cp.BackupDestinationObjectStore,
		Provider: "backups", ObjectKey: "burrow/base/20260811/backup_manifest",
	}
	if err := d.RecordBackup(ctx, physical); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	err := e.DeleteBackup(ctx, cp.AddonPostgres, physical.ID)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("DeleteBackup of a physical backup = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "pgBackRest") {
		t.Errorf("refusal = %q, want it to name the repository the bytes belong to", err)
	}
	if len(osf.Store.Deleted) != 0 {
		t.Errorf("objects deleted = %v, want none: nothing may be unlinked out of a stanza", osf.Store.Deleted)
	}
	if len(k.BackupRemovals()) != 0 {
		t.Error("a physical backup drove a dump removal, and it has no dump on a claim")
	}
	if _, err := d.GetBackup(ctx, physical.ID); err != nil {
		t.Errorf("the physical backup's row was removed: %v", err)
	}
}

// TestDeleteBackupKeepsRecordWhenTheDumpSurvives is the ORDER, asserted from the failure it is
// chosen for. If the bytes cannot be removed, the row stays: it is what names the volume and the key
// the next attempt needs, and a second run finishes the job because every removal here is
// idempotent.
func TestDeleteBackupKeepsRecordWhenTheDumpSurvives(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, _ := newBackupDeleteEngine(t)
	b := takeBackup(t, e, "web")

	k.SetError(fake.OpRemoveBackupFile, errors.New("kube: job \"burrow-pg-backup-rm-1\" failed"))
	if err := e.DeleteBackup(ctx, cp.AddonPostgres, b.ID); err == nil {
		t.Fatal("DeleteBackup should error when the dump could not be removed")
	}
	got, err := d.GetBackup(ctx, b.ID)
	if err != nil {
		t.Fatalf("the record was removed while its dump is still on the volume: %v", err)
	}
	if got.Volume != b.Volume || got.Path != b.Path {
		t.Errorf("row = %+v, want it to still name the dump it points at", got)
	}

	// Retried once the cluster is healthy, the same call finishes.
	k.SetError(fake.OpRemoveBackupFile, nil)
	if err := e.DeleteBackup(ctx, cp.AddonPostgres, b.ID); err != nil {
		t.Fatalf("retried DeleteBackup: %v", err)
	}
	if _, err := d.GetBackup(ctx, b.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("GetBackup after the retry = %v, want ErrNotFound", err)
	}
}

// TestDeleteBackupKeepsRecordWhenTheObjectSurvives is the same argument one level out, and it is the
// one that costs money: an object key is recorded nowhere but the row, so a row dropped after a
// failed object delete leaves a dump at a vendor that no listing mentions and no sweep can find.
func TestDeleteBackupKeepsRecordWhenTheObjectSurvives(t *testing.T) {
	ctx := context.Background()
	e, _, d, creds, osf := newBackupDeleteEngine(t)
	seedObjectStoreProvider(t, d, creds, "backups")
	b := takeBackup(t, e, "web")

	osf.Store.DeleteErr = errors.New("the vendor refused the delete")
	if err := e.DeleteBackup(ctx, cp.AddonPostgres, b.ID); err == nil {
		t.Fatal("DeleteBackup should error when the object could not be removed")
	}
	got, err := d.GetBackup(ctx, b.ID)
	if err != nil {
		t.Fatalf("the record was removed while its object is still at the vendor: %v", err)
	}
	if got.ObjectKey != b.ObjectKey || got.Provider != b.Provider {
		t.Errorf("row = %+v, want it to still name the provider and key the object is at", got)
	}
}

// TestDeleteBackupRefusesWhenTheProviderIsGone covers the shipped dump whose provider is no longer
// registered. Burrow cannot address the object, and removing the row would erase the only record of
// where the bytes are — so it refuses and says what to do instead.
func TestDeleteBackupRefusesWhenTheProviderIsGone(t *testing.T) {
	ctx := context.Background()
	e, _, d, _, _ := newBackupDeleteEngine(t)

	// A row shipped to a provider that has since been deregistered — the state left behind by
	// `provider remove` after backups had already gone to it.
	b := cp.Backup{
		ID: "bk-orphaned", Kind: cp.BackupKindLogical, App: "web", Environment: cp.DefaultEnvironment,
		CreatedAt: time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC),
		Volume:    cp.PostgresBackupVolume, Path: cp.BackupPath("web", "bk-orphaned"),
		Status: cp.BackupCompleted, Destination: cp.BackupDestinationObjectStore,
		Provider: "retired", ObjectKey: cp.BackupObjectKey("web", cp.DefaultEnvironment, "bk-orphaned"),
	}
	if err := d.RecordBackup(ctx, b); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	err := e.DeleteBackup(ctx, cp.AddonPostgres, b.ID)
	if !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("DeleteBackup with a deregistered provider = %v, want ErrNotFound", err)
	}
	if _, err := d.GetBackup(ctx, b.ID); err != nil {
		t.Errorf("the record was removed though its object could not be: %v", err)
	}
}

// TestDeleteBackupRejectsAnotherAddonType keeps the verb where the backups are.
func TestDeleteBackupRejectsAnotherAddonType(t *testing.T) {
	e, _, _, _, _ := newBackupDeleteEngine(t)
	if err := e.DeleteBackup(context.Background(), cp.AddonType("redis"), "bk1"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("DeleteBackup(redis) = %v, want ErrInvalid", err)
	}
	if err := e.DeleteBackup(context.Background(), cp.AddonPostgres, ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("DeleteBackup with no id = %v, want ErrInvalid", err)
	}
}

// hasAuditOp reports whether an audit row for op with the given outcome was recorded.
func hasAuditOp(d *fake.Database, op string, outcome cp.AuditOutcome) bool {
	for _, row := range d.AuditRows() {
		if row.Operation == op && row.Outcome == outcome {
			return true
		}
	}
	return false
}

// countAuditOp counts the audit rows recorded for op.
func countAuditOp(d *fake.Database, op string) int {
	n := 0
	for _, row := range d.AuditRows() {
		if row.Operation == op {
			n++
		}
	}
	return n
}
