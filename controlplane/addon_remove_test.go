// SPDX-License-Identifier: Apache-2.0
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

// installPostgres installs the Postgres add-on on a fresh engine and returns the seams, so each
// removal test starts from a real registry row and a real data volume in the fake cluster.
func installPostgres(t *testing.T) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Provisioner) {
	t.Helper()
	e, k, d, prov := newPostgresEngine(t)
	if _, err := e.InstallAddon(context.Background(), cp.AddonPostgres, "", true); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	if _, ok := k.AddonVolume("burrow-postgres"); !ok {
		t.Fatal("install did not create a data volume to test removal against")
	}
	return e, k, d, prov
}

// TestRemoveAddonKeepsDataByDefault is the load-bearing property: removing an add-on tears down its
// workload but LEAVES THE DATA VOLUME. For Postgres that volume holds every attached app's database
// (ADR-0031), so a removal meant as "stop this and reinstall it cleanly" must not destroy it. The
// result names the retained volume, because a survival the caller is not told about is indistinguishable
// from data loss.
func TestRemoveAddonKeepsDataByDefault(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := installPostgres(t)

	res, err := e.RemoveAddon(ctx, "burrow-postgres", false, true)
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if vol, ok := k.AddonVolume("burrow-postgres"); !ok {
		t.Fatal("the data volume was destroyed by a removal that did not ask for it")
	} else if vol != "burrow-postgres" {
		t.Errorf("retained volume = %q, want burrow-postgres", vol)
	}
	if res.DataDeleted {
		t.Error("result reports DataDeleted for a removal that kept the data")
	}
	if res.RetainedDataVolume != "burrow-postgres" {
		t.Errorf("RetainedDataVolume = %q, want burrow-postgres", res.RetainedDataVolume)
	}
	if res.Namespace == "" {
		t.Error("the result does not say which namespace the retained volume is in")
	}
	// The registry row still goes: the add-on is removed, only its data outlives it.
	if _, err := d.Addon(ctx, "burrow-postgres"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("after remove, registry Addon err = %v, want ErrNotFound", err)
	}
}

// TestRemoveAddonDeleteDataDestroysVolume asserts the explicit opt-in does what it says: the data
// volume is destroyed and the result reports it, so the destructive path is still reachable in one
// command — it just cannot be reached by accident.
func TestRemoveAddonDeleteDataDestroysVolume(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := installPostgres(t)

	res, err := e.RemoveAddon(ctx, "burrow-postgres", true, true)
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if _, ok := k.AddonVolume("burrow-postgres"); ok {
		t.Error("--delete-data did not destroy the data volume")
	}
	if !res.DataDeleted {
		t.Error("result does not report DataDeleted after a data-deleting removal")
	}
	if res.RetainedDataVolume != "" {
		t.Errorf("RetainedDataVolume = %q, want empty after the volume was destroyed", res.RetainedDataVolume)
	}
}

// TestRemoveAddonConfirmationNamesAttachedApps is the informed-consent check. addon.remove is held
// for confirmation by default, and the message the human sees has to state what is destroyed in
// concrete terms — the volume by name and the attached apps by name — not a generic "this is
// destructive" (ADR-0006).
func TestRemoveAddonConfirmationNamesAttachedApps(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := installPostgres(t)
	prov.SetAttachedApps(cp.DefaultEnvironment, "api", "web")

	_, err := e.RemoveAddon(ctx, "burrow-postgres", true, false)
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError holding the removal", err)
	}
	if !g.NeedsConfirmation || g.Code != cp.GuardrailAddonRemove {
		t.Fatalf("guardrail = %+v, want addon.remove held for confirmation", g)
	}
	for _, want := range []string{"api", "web", "2 attached apps", "burrow-postgres"} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("confirmation message %q does not mention %q", g.Message, want)
		}
	}
	// It also has to say the backups survive, so approving does not read as "everything is gone".
	if !strings.Contains(g.Message, cp.PostgresBackupVolume) {
		t.Errorf("confirmation message %q does not say the backup volume is kept", g.Message)
	}
}

// TestRemoveAddonConfirmationSaysDataIsKept asserts the non-destructive default is stated just as
// plainly: a human asked to confirm a removal that keeps the data should be told so, or they will
// approve it believing they are losing the database.
func TestRemoveAddonConfirmationSaysDataIsKept(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := installPostgres(t)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")

	_, err := e.RemoveAddon(ctx, "burrow-postgres", false, false)
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError holding the removal", err)
	}
	if strings.Contains(g.Message, "DESTROY") {
		t.Errorf("confirmation message %q claims destruction for a removal that keeps the data", g.Message)
	}
	for _, want := range []string{"KEPT", "reinstall", "1 attached app (web)"} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("confirmation message %q does not mention %q", g.Message, want)
		}
	}
}

// TestRemoveAddonReportsAttachedApps asserts the successful result carries the attached apps too, so
// an agent can tell the human which apps just lost their database connection without re-deriving it.
func TestRemoveAddonReportsAttachedApps(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := installPostgres(t)
	prov.SetAttachedApps(cp.DefaultEnvironment, "api", "web")

	res, err := e.RemoveAddon(ctx, "burrow-postgres", false, true)
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if strings.Join(res.AttachedApps, ",") != "api,web" {
		t.Errorf("AttachedApps = %v, want [api web]", res.AttachedApps)
	}
}

// TestRemoveAddonSucceedsWhenInstanceUnreachable asserts the enumeration is best-effort: an add-on is
// often removed precisely because it is wedged, so an instance that will not answer "who is attached?"
// must not make itself unremovable.
func TestRemoveAddonSucceedsWhenInstanceUnreachable(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := installPostgres(t)
	prov.SetListError(errors.New("connection refused"))

	res, err := e.RemoveAddon(ctx, "burrow-postgres", false, true)
	if err != nil {
		t.Fatalf("RemoveAddon over an unreachable instance: %v", err)
	}
	if len(res.AttachedApps) != 0 {
		t.Errorf("AttachedApps = %v, want none when the instance could not be asked", res.AttachedApps)
	}
	// The data still survives — an unreachable instance is the last case in which to destroy a volume.
	if _, ok := k.AddonVolume("burrow-postgres"); !ok {
		t.Error("the data volume was destroyed on the unreachable-instance path")
	}
}

// TestRemoveAddonBackupVolumeSurvivesDataDeletion asserts the backup volume outlives even a
// data-deleting removal (ADR-0032) and is reported. Backups outliving the database they came from is
// the point of taking them, and it is what makes --delete-data survivable.
func TestRemoveAddonBackupVolumeSurvivesDataDeletion(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := installPostgres(t)
	if _, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", ""); err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}

	res, err := e.RemoveAddon(ctx, "burrow-postgres", true, true)
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if !res.DataDeleted {
		t.Error("result does not report the data was deleted")
	}
	if res.RetainedBackupVolume != cp.PostgresBackupVolume {
		t.Errorf("RetainedBackupVolume = %q, want %q", res.RetainedBackupVolume, cp.PostgresBackupVolume)
	}
}

// TestRemoveStatelessAddonHasNoVolume asserts an add-on with no data volume (cache) reports neither a
// retained nor a deleted one, and its confirmation message does not promise data survival it cannot
// deliver.
func TestRemoveStatelessAddonHasNoVolume(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)
	if _, err := e.InstallAddon(ctx, cp.AddonCache, "", true); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}

	if _, err := e.RemoveAddon(ctx, "burrow-cache", false, false); err != nil {
		g, ok := cp.AsGuardrail(err)
		if !ok {
			t.Fatalf("err = %v, want a held GuardrailError", err)
		}
		if !strings.Contains(g.Message, "no data volume") {
			t.Errorf("confirmation message %q does not say the add-on holds no data volume", g.Message)
		}
	}
	res, err := e.RemoveAddon(ctx, "burrow-cache", false, true)
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if res.DataDeleted || res.RetainedDataVolume != "" {
		t.Errorf("stateless removal reported a volume: %+v", res.AddonRemoval)
	}
}
