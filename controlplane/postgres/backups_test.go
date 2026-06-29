// SPDX-License-Identifier: FSL-1.1-ALv2
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
		ID:        t.Name() + "-b1",
		App:       app,
		CreatedAt: time.Date(2026, 6, 25, 1, 0, 0, 0, time.UTC),
		Path:      "/backups/" + app + "/" + t.Name() + "-b1.dump",
		Status:    cp.BackupPending,
	}
	newer := cp.Backup{
		ID:        t.Name() + "-b2",
		App:       app,
		CreatedAt: time.Date(2026, 6, 25, 2, 0, 0, 0, time.UTC),
		Path:      "/backups/" + app + "/" + t.Name() + "-b2.dump",
		Status:    cp.BackupPending,
	}
	for _, b := range []cp.Backup{older, newer} {
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

	// Per-app listing, newest first.
	list, err := s.ListBackups(ctx, app)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID {
		t.Errorf("ListBackups order = %v, want [%s %s] (newest first)", ids(list), newer.ID, older.ID)
	}

	// All-apps listing includes our app's backups.
	all, err := s.ListBackups(ctx, "")
	if err != nil {
		t.Fatalf("ListBackups all: %v", err)
	}
	var seen int
	for _, b := range all {
		if b.App == app {
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("all-apps listing saw %d backups for %s, want 2", seen, app)
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
