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
