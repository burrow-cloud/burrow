// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestInstallAddonOnCloudNativePGRecordsTheMechanism asserts the registry remembers which mechanism
// an instance was stood up on. It is recorded as the Backend — "which concrete implementation serves
// this add-on", which is what that field has always meant — so the fact survives a burrowd restart
// with no new column and no migration behind it.
func TestInstallAddonOnCloudNativePGRecordsTheMechanism(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
	if err != nil {
		t.Fatalf("InstallAddon on CloudNativePG: %v", err)
	}
	if info.Backend != cp.AddonBackendCloudNativePG {
		t.Errorf("backend = %q, want %q", info.Backend, cp.AddonBackendCloudNativePG)
	}
	stored, err := d.Addon(ctx, info.Name)
	if err != nil {
		t.Fatalf("reading the registry row back: %v", err)
	}
	if stored.Backend != cp.AddonBackendCloudNativePG {
		t.Errorf("the registry recorded backend %q; the mechanism must outlive the process that chose it", stored.Backend)
	}
}

// TestInstallAddonOnCloudNativePGIsPostgresOnly asserts the mechanism is refused for an add-on it
// has no meaning for, at the engine rather than deeper down: CloudNativePG runs PostgreSQL, and
// "install the cache on CloudNativePG" is a sentence with no referent.
func TestInstallAddonOnCloudNativePGIsPostgresOnly(t *testing.T) {
	e, _, _, _ := newPostgresEngine(t)

	_, err := e.InstallAddon(context.Background(), cp.AddonCache, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("InstallAddon(cache, cloudnative-pg) error = %v, want ErrInvalid", err)
	}
}

// TestInstallAddonConfirmationNamesTheMechanism asserts a held confirmation says which of the two
// materially different things "install the postgres add-on" now means. A confirmation that does not
// state what is about to happen is not informed consent (ADR-0006), and the mechanism decides how
// the database is run, backed up and removed.
func TestInstallAddonConfirmationNamesTheMechanism(t *testing.T) {
	e, _, _, _ := newPostgresEngine(t)

	_, err := e.InstallAddon(context.Background(), cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG,
	})
	if err == nil {
		t.Fatal("an unconfirmed install was not held")
	}
	if !strings.Contains(err.Error(), string(cp.AddonMechanismCloudNativePG)) {
		t.Errorf("the held confirmation does not name the mechanism: %v", err)
	}
}

// TestRemoveAddonRefusesACloudNativePGInstance states this slice's boundary as behaviour rather than
// as prose. Removing a CloudNativePG-backed instance is not built: ADR-0064 §1 makes a removal KEEP
// the data by default, and under this mechanism the volumes belong to the operator — they carry the
// `Cluster` as their owner, so deleting it takes them with it, and a fresh `Cluster` does not adopt
// what survives if it does not. Neither branch of `addon remove` is expressible yet.
//
// So it refuses, and refuses EARLY: before the guardrail, before any final backup, and before the
// first destructive call. Nothing is changed, which is the property that makes the gap survivable
// rather than a trap.
func TestRemoveAddonRefusesACloudNativePGInstance(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}

	_, err = e.RemoveAddon(ctx, info.Name, cp.RemoveAddonOptions{Confirm: true, DeleteData: true})
	if !errors.Is(err, cp.ErrNotImplemented) {
		t.Fatalf("RemoveAddon error = %v, want ErrNotImplemented", err)
	}
	if _, err := d.Addon(ctx, info.Name); err != nil {
		t.Errorf("the registry row was removed by a refused removal: %v", err)
	}
}
