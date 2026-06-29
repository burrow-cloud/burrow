// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// TestBackupAddonRecordsPendingThenCompleted asserts BackupAddon records the backup pending, runs
// the in-cluster Job, marks it completed with the reported size, and returns the recorded row
// (ADR-0032).
func TestBackupAddonRecordsPendingThenCompleted(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newPostgresEngine(t)
	k.SetBackupSize(2048)

	res, err := e.BackupAddon(ctx, cp.AddonPostgres, "web")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	if res.Backup.App != "web" || res.Backup.Status != cp.BackupCompleted || res.Backup.SizeBytes != 2048 {
		t.Errorf("result = %+v, want web/completed/2048", res.Backup)
	}
	if res.Backup.Path != cp.BackupPath("web", res.Backup.ID) {
		t.Errorf("path = %q, want %q", res.Backup.Path, cp.BackupPath("web", res.Backup.ID))
	}

	// The in-cluster Job ran with the app and the backup id.
	jobs := k.BackupJobs()
	if len(jobs) != 1 || jobs[0].App != "web" || jobs[0].BackupID != res.Backup.ID {
		t.Errorf("BackupJobs = %+v, want one call for web/%s", jobs, res.Backup.ID)
	}

	// The store holds the completed backup.
	got, err := d.GetBackup(ctx, res.Backup.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.Status != cp.BackupCompleted || got.SizeBytes != 2048 {
		t.Errorf("stored backup = %+v, want completed/2048", got)
	}
}

// TestBackupAddonJobFailureMarksFailed asserts a failed backup Job leaves the recorded backup in
// failed state and returns the error.
func TestBackupAddonJobFailureMarksFailed(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newPostgresEngine(t)
	k.SetError(fake.OpRunBackupJob, errors.New("boom"))

	if _, err := e.BackupAddon(ctx, cp.AddonPostgres, "web"); err == nil {
		t.Fatal("BackupAddon should error when the Job fails")
	}
	list, err := d.ListBackups(ctx, "web")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 1 || list[0].Status != cp.BackupFailed {
		t.Errorf("recorded backups = %+v, want one failed", list)
	}
}

// TestBackupRejectsBadInput rejects a non-postgres add-on and a bad app name before any Job.
func TestBackupRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)
	if _, err := e.BackupAddon(ctx, cp.AddonCache, "web"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("backup non-postgres err = %v, want ErrInvalid", err)
	}
	if _, err := e.BackupAddon(ctx, cp.AddonPostgres, "Bad_Name"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("backup bad app name err = %v, want ErrInvalid", err)
	}
	if jobs := k.BackupJobs(); len(jobs) != 0 {
		t.Errorf("no Job should run on invalid input, got %+v", jobs)
	}
}

// TestListBackupsReadsRegistry asserts ListBackups returns the recorded backups newest first and
// rejects a non-postgres add-on / bad app name.
func TestListBackupsReadsRegistry(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	first, err := e.BackupAddon(ctx, cp.AddonPostgres, "web")
	if err != nil {
		t.Fatalf("BackupAddon 1: %v", err)
	}
	second, err := e.BackupAddon(ctx, cp.AddonPostgres, "web")
	if err != nil {
		t.Fatalf("BackupAddon 2: %v", err)
	}

	list, err := e.ListBackups(ctx, cp.AddonPostgres, "web")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.Backup.ID || list[1].ID != first.Backup.ID {
		t.Errorf("ListBackups = %v, want newest-first [%s %s]", list, second.Backup.ID, first.Backup.ID)
	}

	if _, err := e.ListBackups(ctx, cp.AddonCache, ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("list non-postgres err = %v, want ErrInvalid", err)
	}
	if _, err := e.ListBackups(ctx, cp.AddonPostgres, "Bad_Name"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("list bad app err = %v, want ErrInvalid", err)
	}
}

// TestRestoreAddonConfirmGated asserts restore is held by the addon_restore confirm guardrail, runs
// the restore Job only when confirmed, and rejects an unknown or mismatched backup.
func TestRestoreAddonConfirmGated(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)

	res, err := e.BackupAddon(ctx, cp.AddonPostgres, "web")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	id := res.Backup.ID

	// Without confirm the restore guardrail holds it; no Job runs.
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", id, false); err == nil {
		t.Fatal("restore without confirm should be held by the guardrail")
	} else {
		mustGuardrail(t, err, cp.GuardrailAddonRestore)
	}
	if jobs := k.RestoreJobs(); len(jobs) != 0 {
		t.Errorf("a held restore must not run a Job, got %+v", jobs)
	}

	// With confirm it runs the restore Job.
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", id, true); err != nil {
		t.Fatalf("RestoreAddon confirmed: %v", err)
	}
	jobs := k.RestoreJobs()
	if len(jobs) != 1 || jobs[0].App != "web" || jobs[0].BackupID != id {
		t.Errorf("RestoreJobs = %+v, want one call for web/%s", jobs, id)
	}

	// An unknown backup id is ErrNotFound; a backup belonging to another app is ErrInvalid.
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", "no-such-id", true); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("restore unknown backup err = %v, want ErrNotFound", err)
	}
	other, err := e.BackupAddon(ctx, cp.AddonPostgres, "shop")
	if err != nil {
		t.Fatalf("BackupAddon shop: %v", err)
	}
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", other.Backup.ID, true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("restore mismatched-app backup err = %v, want ErrInvalid", err)
	}
}

// TestBackupRestoreAuditRedacted drives backup and restore and asserts every audit row carries only
// the {addon, app, backup} allowlist — never a path-as-credential, a connection string, or a
// password fragment. The audit log is the redaction boundary (ADR-0027/0032).
func TestBackupRestoreAuditRedacted(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	res, err := e.BackupAddon(ctx, cp.AddonPostgres, "web")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", res.Backup.ID, true); err != nil {
		t.Fatalf("RestoreAddon: %v", err)
	}

	rows, err := d.Audit(ctx, cp.AuditFilter{})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	sawBackup, sawRestore := false, false
	for _, row := range rows {
		switch row.Operation {
		case "addon_backup":
			sawBackup = true
		case "addon_restore":
			sawRestore = true
		}
		for key, v := range row.Args {
			if key != "addon" && key != "app" && key != "backup" {
				t.Errorf("audit row %s has unexpected arg key %q (only addon/app/backup allowed)", row.Operation, key)
			}
			if strings.Contains(v, "postgres://") || strings.Contains(v, "fakepw") || strings.Contains(v, "PGPASSWORD") {
				t.Errorf("audit arg %q leaks a credential: %q", key, v)
			}
		}
	}
	if !sawBackup || !sawRestore {
		t.Errorf("missing audit rows: backup=%v restore=%v", sawBackup, sawRestore)
	}
}
