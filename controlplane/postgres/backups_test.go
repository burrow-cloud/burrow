// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestStoreBackupsRoundTrip exercises the backup index CRUD against a real database: record a
// pending backup, read it back, transition it to completed with a size, list it (per-app and
// all-apps, newest first), and assert ErrNotFound for an unknown id (ADR-0032).
func TestStoreBackupsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-app"

	older := cp.Backup{
		ID:          t.Name() + "-b1",
		App:         app,
		Environment: cp.DefaultEnvironment,
		CreatedAt:   time.Date(2026, 6, 25, 1, 0, 0, 0, time.UTC),
		Path:        "/backups/" + app + "/" + t.Name() + "-b1.dump",
		Status:      cp.BackupPending,
	}
	newer := cp.Backup{
		ID:          t.Name() + "-b2",
		App:         app,
		Environment: cp.DefaultEnvironment,
		CreatedAt:   time.Date(2026, 6, 25, 2, 0, 0, 0, time.UTC),
		Path:        "/backups/" + app + "/" + t.Name() + "-b2.dump",
		Status:      cp.BackupPending,
	}
	// A dump of the SAME app in another environment: a separate instance, therefore separate
	// contents, and it must not turn up when the caller asks for the default environment's backups
	// (ADR-0067 §1).
	staging := cp.Backup{
		ID:          t.Name() + "-b3",
		App:         app,
		Environment: "staging",
		CreatedAt:   time.Date(2026, 6, 25, 3, 0, 0, 0, time.UTC),
		Path:        "/backups/" + app + "/" + t.Name() + "-b3.dump",
		Status:      cp.BackupPending,
	}
	for _, b := range []cp.Backup{older, newer, staging} {
		if err := s.RecordBackup(ctx, b); err != nil {
			t.Fatalf("RecordBackup %s: %v", b.ID, err)
		}
	}

	got, err := s.GetBackup(ctx, older.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.App != app || got.Status != cp.BackupPending || got.Path != older.Path {
		t.Errorf("round trip = %+v, want app=%s pending path=%s", got, app, older.Path)
	}
	if !got.CreatedAt.Equal(older.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, older.CreatedAt)
	}

	// Transition older to completed with a size.
	if err := s.SetBackupStatus(ctx, older.ID, cp.BackupCompleted, 4096); err != nil {
		t.Fatalf("SetBackupStatus: %v", err)
	}
	got, _ = s.GetBackup(ctx, older.ID)
	if got.Status != cp.BackupCompleted || got.SizeBytes != 4096 {
		t.Errorf("after SetBackupStatus = status %q size %d, want completed/4096", got.Status, got.SizeBytes)
	}

	// Per-app listing across environments, newest first.
	list, err := s.ListBackups(ctx, app, "")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 3 || list[0].ID != staging.ID || list[1].ID != newer.ID || list[2].ID != older.ID {
		t.Errorf("ListBackups order = %v, want [%s %s %s] (newest first)", ids(list), staging.ID, newer.ID, older.ID)
	}

	// Restricted to one environment, the other environment's dump of the same app is not listed.
	def, err := s.ListBackups(ctx, app, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("ListBackups default env: %v", err)
	}
	if len(def) != 2 || def[0].ID != newer.ID || def[1].ID != older.ID {
		t.Errorf("ListBackups(%s, default) = %v, want [%s %s]", app, ids(def), newer.ID, older.ID)
	}
	stg, err := s.ListBackups(ctx, app, "staging")
	if err != nil {
		t.Fatalf("ListBackups staging: %v", err)
	}
	if len(stg) != 1 || stg[0].ID != staging.ID {
		t.Errorf("ListBackups(%s, staging) = %v, want [%s]", app, ids(stg), staging.ID)
	}
	if stg[0].Environment != "staging" {
		t.Errorf("round-tripped environment = %q, want staging", stg[0].Environment)
	}

	// All-apps listing includes our app's backups.
	all, err := s.ListBackups(ctx, "", "")
	if err != nil {
		t.Fatalf("ListBackups all: %v", err)
	}
	var seen int
	for _, b := range all {
		if b.App == app {
			seen++
		}
	}
	if seen != 3 {
		t.Errorf("all-apps listing saw %d backups for %s, want 3", seen, app)
	}

	// Unknown ids are ErrNotFound for both GetBackup and SetBackupStatus.
	if _, err := s.GetBackup(ctx, t.Name()+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("GetBackup missing err = %v, want ErrNotFound", err)
	}
	if err := s.SetBackupStatus(ctx, t.Name()+"-missing", cp.BackupCompleted, 0); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("SetBackupStatus missing err = %v, want ErrNotFound", err)
	}
}

func ids(bs []cp.Backup) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

// TestStoreBackupDestinationAndFailureRoundTrip exercises the columns ADR-0063 §7 adds: where a
// backup went, and — when it did not get there — the closed reason and the Burrow-authored detail.
//
// The two transitions are asserted in the order they happen in a retry: a run that fails and is
// re-recorded as completed must not keep a stale reason beside it, and a failed row must not keep a
// size, because a length on a failed backup reads like a partial success.
func TestStoreBackupDestinationAndFailureRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-app"

	b := cp.Backup{
		ID:          t.Name() + "-b1",
		App:         app,
		Environment: cp.DefaultEnvironment,
		CreatedAt:   time.Date(2026, 7, 25, 1, 0, 0, 0, time.UTC),
		Path:        "/backups/" + app + "/b1.dump",
		Status:      cp.BackupPending,
		Destination: cp.BackupDestinationObjectStore,
		Provider:    "backups",
		ObjectKey:   "burrow/backups/prod/" + app + "/b1.dump",
	}
	if err := s.RecordBackup(ctx, b); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	got, err := s.GetBackup(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.Destination != cp.BackupDestinationObjectStore || got.Provider != "backups" || got.ObjectKey != b.ObjectKey {
		t.Errorf("destination round trip = %+v", got)
	}

	if err := s.FailBackup(ctx, b.ID, cp.BackupReasonStoreUnreachable, "the destination did not complete the write after 4 attempts"); err != nil {
		t.Fatalf("FailBackup: %v", err)
	}
	got, _ = s.GetBackup(ctx, b.ID)
	if got.Status != cp.BackupFailed || got.FailureReason != cp.BackupReasonStoreUnreachable {
		t.Errorf("after FailBackup = %+v, want failed/%s", got, cp.BackupReasonStoreUnreachable)
	}
	if got.SizeBytes != 0 {
		t.Errorf("size = %d on a failed backup, want 0", got.SizeBytes)
	}
	if got.FailureDetail == "" {
		t.Error("the detail did not survive")
	}
	// The destination is still on the failed row: "this did not reach the store" is a more useful
	// fact than "this failed".
	if got.Destination != cp.BackupDestinationObjectStore {
		t.Errorf("destination = %q on a failed backup, want it preserved", got.Destination)
	}

	// A later success clears the reason; a completed row must never carry an explanation of a
	// failure beside it.
	if err := s.SetBackupStatus(ctx, b.ID, cp.BackupCompleted, 4096); err != nil {
		t.Fatalf("SetBackupStatus: %v", err)
	}
	got, _ = s.GetBackup(ctx, b.ID)
	if got.FailureReason != "" || got.FailureDetail != "" {
		t.Errorf("a completed backup kept a failure reason: %+v", got)
	}

	// A backup recorded with no destination named reached the cluster and nowhere else; the row says
	// so rather than leaving the field blank for a reader to interpret.
	plain := cp.Backup{
		ID: t.Name() + "-b2", App: app, Environment: cp.DefaultEnvironment,
		CreatedAt: time.Date(2026, 7, 25, 2, 0, 0, 0, time.UTC),
		Path:      "/backups/" + app + "/b2.dump", Status: cp.BackupPending,
	}
	if err := s.RecordBackup(ctx, plain); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	got, _ = s.GetBackup(ctx, plain.ID)
	if got.Destination != cp.BackupDestinationCluster {
		t.Errorf("destination = %q, want %q", got.Destination, cp.BackupDestinationCluster)
	}

	if err := s.FailBackup(ctx, "no-such-backup", cp.BackupReasonDumpFailed, "x"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("FailBackup on an unknown id = %v, want ErrNotFound", err)
	}
}

// TestStoreBackupVolumeRoundTrip asserts the claim a dump was written to is stored and read back,
// and that a row recorded without one lands on the claim that predates per-environment backups
// (ADR-0067 §1, migration 00027).
//
// The fallback is the load-bearing half. `environment` says which instance a dump came FROM; only
// `volume` says which disk it is ON, and the two disagree for a dump taken in a non-default
// environment while a single shared claim still existed. A row that recorded nothing would leave a
// restore deriving the claim from the environment and looking for that dump on a volume created
// empty.
func TestStoreBackupVolumeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-app"

	staging, err := cp.BackupVolumeName(cp.AddonPostgres, "staging")
	if err != nil {
		t.Fatalf("BackupVolumeName: %v", err)
	}
	recorded := cp.Backup{
		ID:          t.Name() + "-b1",
		App:         app,
		Environment: "staging",
		CreatedAt:   time.Date(2026, 6, 25, 1, 0, 0, 0, time.UTC),
		Volume:      staging,
		Path:        "/backups/" + app + "/" + t.Name() + "-b1.dump",
		Status:      cp.BackupCompleted,
	}
	// A row written with no claim named: the only one it can be on is the claim that has ever held
	// a dump, which is what the column was backfilled to.
	unnamed := recorded
	unnamed.ID, unnamed.Volume = t.Name()+"-b2", ""

	for _, b := range []cp.Backup{recorded, unnamed} {
		if err := s.RecordBackup(ctx, b); err != nil {
			t.Fatalf("RecordBackup %s: %v", b.ID, err)
		}
	}

	got, err := s.GetBackup(ctx, recorded.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.Volume != staging {
		t.Errorf("volume = %q, want staging's own claim %q", got.Volume, staging)
	}
	got, err = s.GetBackup(ctx, unnamed.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.Volume != cp.PostgresBackupVolume {
		t.Errorf("volume of a row recorded without one = %q, want %q", got.Volume, cp.PostgresBackupVolume)
	}
}

// TestStoreBackupKindRoundTrip exercises the column migration 00028 added (ADR-0066 §4): a physical
// row belongs to no app, is on no claim, and comes back saying which mechanism took it.
//
// The two assertions that matter are the DEFAULT and the VOLUME. A row written with no kind is
// logical, which is exact rather than a guess — nothing else could have written it. And a physical
// row must NOT be backfilled onto the pre-per-environment backup claim the way a logical one is: its
// bytes are in the object store, and a row naming a volume they are not on is how a restore goes
// looking in the wrong place.
func TestStoreBackupKindRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	physical := cp.Backup{
		ID:          t.Name() + "-p1",
		Kind:        cp.BackupKindPhysical,
		Environment: cp.DefaultEnvironment,
		CreatedAt:   time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC),
		Status:      cp.BackupPending,
		Destination: cp.BackupDestinationObjectStore,
		Provider:    "backups",
		ObjectKey:   cp.PgBackRestManifestKey(cp.PgBackRestRepoPath(cp.DefaultEnvironment), "burrow-postgres", "20260801-020000F"),
	}
	unstated := cp.Backup{
		ID:          t.Name() + "-l1",
		App:         t.Name() + "-app",
		Environment: cp.DefaultEnvironment,
		CreatedAt:   time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC),
		Status:      cp.BackupPending,
	}
	for _, b := range []cp.Backup{physical, unstated} {
		if err := s.RecordBackup(ctx, b); err != nil {
			t.Fatalf("RecordBackup %s: %v", b.ID, err)
		}
	}

	got, err := s.GetBackup(ctx, physical.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.Kind != cp.BackupKindPhysical {
		t.Errorf("kind = %q, want %q", got.Kind, cp.BackupKindPhysical)
	}
	if got.App != "" {
		t.Errorf("app = %q, want empty on a physical row", got.App)
	}
	if got.Volume != "" {
		t.Errorf("volume = %q, want empty: a physical backup is on no claim", got.Volume)
	}
	if got.ObjectKey != physical.ObjectKey {
		t.Errorf("object key = %q, want %q", got.ObjectKey, physical.ObjectKey)
	}

	stated, err := s.GetBackup(ctx, unstated.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if stated.Kind != cp.BackupKindLogical {
		t.Errorf("kind = %q, want %q: a row written with no kind is a logical dump", stated.Kind, cp.BackupKindLogical)
	}
	if stated.Volume != cp.PostgresBackupVolume {
		t.Errorf("volume = %q, want the pre-per-environment claim %q", stated.Volume, cp.PostgresBackupVolume)
	}
}

// TestStoreDeleteBackup exercises the removal half of the backup index against a real database: a
// recorded row is deleted and gone from both the get and the listing, removing it again is success
// rather than ErrNotFound, and an id that never existed is the same answer. That idempotence is what
// lets a caller pruning expired backups retry after a crash without telling "I removed it" and
// "somebody else already had" apart.
func TestStoreDeleteBackup(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-app"

	b := cp.Backup{
		ID:          t.Name() + "-b1",
		App:         app,
		Environment: cp.DefaultEnvironment,
		CreatedAt:   time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC),
		Path:        "/backups/" + app + "/" + t.Name() + "-b1.dump",
		Volume:      cp.PostgresBackupVolume,
		Status:      cp.BackupCompleted,
	}
	keep := cp.Backup{
		ID:          t.Name() + "-b2",
		App:         app,
		Environment: cp.DefaultEnvironment,
		CreatedAt:   time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC),
		Path:        "/backups/" + app + "/" + t.Name() + "-b2.dump",
		Volume:      cp.PostgresBackupVolume,
		Status:      cp.BackupCompleted,
	}
	for _, rec := range []cp.Backup{b, keep} {
		if err := s.RecordBackup(ctx, rec); err != nil {
			t.Fatalf("RecordBackup %s: %v", rec.ID, err)
		}
	}

	if err := s.DeleteBackup(ctx, b.ID); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if _, err := s.GetBackup(ctx, b.ID); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("GetBackup after delete = %v, want ErrNotFound", err)
	}

	// Only the named row went.
	list, err := s.ListBackups(ctx, app, "")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 1 || list[0].ID != keep.ID {
		t.Fatalf("ListBackups = %+v, want only %s", list, keep.ID)
	}

	// Removing it a second time, and removing an id that never existed, are both success.
	if err := s.DeleteBackup(ctx, b.ID); err != nil {
		t.Errorf("second DeleteBackup = %v, want nil", err)
	}
	if err := s.DeleteBackup(ctx, t.Name()+"-never-recorded"); err != nil {
		t.Errorf("DeleteBackup of an unknown id = %v, want nil", err)
	}
	// An empty id is a caller error rather than a no-op: it names no row, and accepting it would
	// make a bug that lost the id look like a successful prune.
	if err := s.DeleteBackup(ctx, ""); err == nil {
		t.Error("DeleteBackup with an empty id = nil, want an error")
	}
}
