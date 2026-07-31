// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestRetainedVolumeAppearsAfterRemoval is the point of ADR-0064 §6: the volume a removal keeps must
// be findable AFTER the removal's output has scrolled away. Before this it appeared exactly once, in
// the output of the removal that created it, and the only way back to it was `kubectl get pvc`.
func TestRetainedVolumeAppearsAfterRemoval(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := installPostgres(t)

	// While it is installed the add-on owns its volume: nothing is retained.
	if retained, err := e.RetainedAddonVolumes(ctx); err != nil {
		t.Fatalf("RetainedAddonVolumes: %v", err)
	} else if len(retained) != 0 {
		t.Fatalf("an installed add-on's own volume is reported as retained: %+v", retained)
	}

	res, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if res.RetainedDataVolume == "" {
		t.Fatal("the removal kept no volume, so there is nothing for the listing to report")
	}

	retained, err := e.RetainedAddonVolumes(ctx)
	if err != nil {
		t.Fatalf("RetainedAddonVolumes: %v", err)
	}
	if len(retained) != 1 {
		t.Fatalf("retained volumes = %+v, want exactly the removed add-on's data claim", retained)
	}
	v := retained[0]
	if v.Name != "burrow-postgres" {
		t.Errorf("claim = %q, want burrow-postgres", v.Name)
	}
	// The add-on it belonged to, its size, and where it lives — the three facts ADR-0064 §6 asks for,
	// the last two being what turns "there is a claim" into a decision about a bill.
	if v.Addon != cp.AddonPostgres {
		t.Errorf("addon = %q, want postgres", v.Addon)
	}
	if v.Role != cp.AddonVolumeData {
		t.Errorf("role = %q, want %q", v.Role, cp.AddonVolumeData)
	}
	if v.Size == "" {
		t.Error("the retained volume reports no size; cost is the reason the listing exists")
	}
	if v.Namespace == "" {
		t.Error("the retained volume does not say which namespace to reclaim it in")
	}
	// ADR-0064 §1: reinstalling picks the volume back up. A listing that did not say so would leave
	// the operator to guess whether deleting the claim is a reclaim or a data loss.
	if !v.ReinstallAdopts {
		t.Error("a retained data volume should report that a reinstall adopts it")
	}
}

// TestInstalledAddonVolumeNotReportedAsRetained keeps the listing honest in the other direction: a
// live add-on's volume is in use, not left behind. Reporting it would train the reader to ignore the
// section, which is the failure ADR-0064 §6 is trying to prevent.
func TestInstalledAddonVolumeNotReportedAsRetained(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := installPostgres(t)
	if _, err := e.InstallAddon(ctx, cp.AddonLogs, cp.DefaultEnvironment, cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon(logs): %v", err)
	}

	// Remove one of the two. Only the removed add-on's claim is retained; the surviving add-on's is
	// still its own.
	if _, err := e.RemoveAddon(ctx, "burrow-logs", cp.RemoveAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("RemoveAddon(logs): %v", err)
	}
	retained, err := e.RetainedAddonVolumes(ctx)
	if err != nil {
		t.Fatalf("RetainedAddonVolumes: %v", err)
	}
	if len(retained) != 1 || retained[0].Name != "burrow-logs" {
		t.Fatalf("retained = %+v, want only burrow-logs (postgres is still installed)", retained)
	}
}

// TestRetainedVolumesEmptyWhenNothingWasRemoved asserts a cluster with no leftovers lists cleanly —
// no empty table, no phantom row, nothing to explain.
func TestRetainedVolumesEmptyWhenNothingWasRemoved(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	retained, err := e.RetainedAddonVolumes(ctx)
	if err != nil {
		t.Fatalf("RetainedAddonVolumes on an empty cluster: %v", err)
	}
	if len(retained) != 0 {
		t.Fatalf("retained = %+v, want none on a cluster that has removed nothing", retained)
	}

	// Installing and leaving it installed changes nothing: retention is about what a removal left.
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	if retained, err := e.RetainedAddonVolumes(ctx); err != nil || len(retained) != 0 {
		t.Fatalf("retained = %+v err=%v, want none while the add-on is installed", retained, err)
	}
}

// TestRetainedBackupVolumeIsReportedSeparately covers the other claim a removal leaves: the Postgres
// dump volume, which survives even a data-deleting removal (ADR-0064 §4). It is attributed to the
// add-on it served and marked as a backup, because "10Gi of dumps" and "10Gi of live database" are
// different decisions — and only one of them comes back on reinstall.
func TestRetainedBackupVolumeIsReportedSeparately(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := installPostgres(t)
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", cp.DefaultEnvironment); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if _, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", cp.DefaultEnvironment, ""); err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	// A backup claim under a still-installed add-on is in use, not retained.
	if retained, err := e.RetainedAddonVolumes(ctx); err != nil || len(retained) != 0 {
		t.Fatalf("retained = %+v err=%v, want none while postgres is installed", retained, err)
	}

	// Destroy the data volume: the dumps still outlive it, and the listing still has to say so.
	if _, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, Confirm: true}); err != nil {
		t.Fatalf("RemoveAddon --delete-data: %v", err)
	}
	retained, err := e.RetainedAddonVolumes(ctx)
	if err != nil {
		t.Fatalf("RetainedAddonVolumes: %v", err)
	}
	if len(retained) != 1 {
		t.Fatalf("retained = %+v, want only the backup claim", retained)
	}
	v := retained[0]
	if v.Name != cp.PostgresBackupVolume {
		t.Errorf("claim = %q, want %q", v.Name, cp.PostgresBackupVolume)
	}
	if v.Addon != cp.AddonPostgres {
		t.Errorf("addon = %q, want postgres", v.Addon)
	}
	if v.Role != cp.AddonVolumeBackup {
		t.Errorf("role = %q, want %q", v.Role, cp.AddonVolumeBackup)
	}
	if v.ReinstallAdopts {
		t.Error("a reinstall does not adopt the backup claim; the listing should not say it does")
	}
}
