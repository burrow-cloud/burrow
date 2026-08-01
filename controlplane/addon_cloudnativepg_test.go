// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// TestRemoveAddonKeepsTheDataOfACloudNativePGInstance is ADR-0064 §1 over the mechanism ADR-0066 §1
// chose. The record's default does not move because the implementation did: removing a Postgres
// instance tears its `Cluster` down and KEEPS the disk every attached app's database lives on.
//
// It asserts the retained claim by name because the name is the operator's, not Burrow's. A
// `Cluster` composes one claim per instance and calls it `<instance>-1`, so a removal that reported
// (or looked for) `<instance>` would name a volume that does not exist and leave the one that does.
func TestRemoveAddonKeepsTheDataOfACloudNativePGInstance(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}

	res, err := e.RemoveAddon(ctx, info.Name, cp.RemoveAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if res.DataDeleted {
		t.Fatal("a removal that was not asked to destroy the data destroyed it (ADR-0064 §1)")
	}
	if want := info.Name + "-1"; res.RetainedDataVolume != want {
		t.Errorf("retained data volume = %q, want the operator's claim %q — a removal that keeps the data must name what it kept", res.RetainedDataVolume, want)
	}
	if _, err := d.Addon(ctx, info.Name); err == nil {
		t.Error("the registry row survived a successful removal")
	}
}

// TestRemovedCloudNativePGVolumeIsListedAsRetained is ADR-0064 §6: keeping data by default is only
// defensible while the leftovers are VISIBLE, and a claim nobody can find is a silent bill rather
// than a decision.
//
// It matters more for Postgres than for the add-ons Burrow deploys itself. The operator's claims
// deliberately do not carry Burrow's selectable label while the `Cluster` owns them — a live
// instance's disk is not a retained volume — so if nothing added it at removal time the §6 listing
// would be empty for every Postgres instance ever removed, and "your data was kept" would be a
// promise with no way to find what was kept.
func TestRemovedCloudNativePGVolumeIsListedAsRetained(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	// While it is installed the claim belongs to a live add-on and is not retained.
	before, err := e.RetainedAddonVolumes(ctx)
	if err != nil {
		t.Fatalf("RetainedAddonVolumes: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("a live instance's volume is listed as retained: %+v", before)
	}
	if _, err := e.RemoveAddon(ctx, info.Name, cp.RemoveAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}

	after, err := e.RetainedAddonVolumes(ctx)
	if err != nil {
		t.Fatalf("RetainedAddonVolumes: %v", err)
	}
	if len(after) != 1 || after[0].Name != info.Name+"-1" {
		t.Fatalf("retained volumes = %+v, want the removed instance's claim %q", after, info.Name+"-1")
	}
	if after[0].Addon != cp.AddonPostgres || after[0].Role != cp.AddonVolumeData {
		t.Errorf("the retained claim is attributed as %s/%s, want postgres/data", after[0].Addon, after[0].Role)
	}
}

// TestRemoveAddonDeleteDataDestroysACloudNativePGInstance asserts the destructive branch actually
// destroys — the other half of ADR-0064 §1/§2. A `--delete-data` that quietly left the operator's
// claims behind would be worse than either honest answer: the user asked for the space back, was
// told they got it, and is billed for it indefinitely.
func TestRemoveAddonDeleteDataDestroysACloudNativePGInstance(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}

	res, err := e.RemoveAddon(ctx, info.Name, cp.RemoveAddonOptions{Confirm: true, DeleteData: true})
	if err != nil {
		t.Fatalf("RemoveAddon --delete-data: %v", err)
	}
	if !res.DataDeleted {
		t.Error("--delete-data reported that no data volume was destroyed")
	}
	if res.RetainedDataVolume != "" {
		t.Errorf("--delete-data reported a retained data volume %q", res.RetainedDataVolume)
	}
	retained, err := e.RetainedAddonVolumes(ctx)
	if err != nil {
		t.Fatalf("RetainedAddonVolumes: %v", err)
	}
	if len(retained) != 0 {
		t.Errorf("a destroyed instance still has claims: %+v", retained)
	}
}

// TestRemoveAddonConfirmationNamesTheCloudNativePGClaim is ADR-0064 §3: the held confirmation states
// the volume BY NAME, and for Postgres that name is the operator's rather than the instance's. A confirmation that names a claim which does not exist is worse than one that names
// none — it reads as precise, and the operator approves it on that basis.
func TestRemoveAddonConfirmationNamesTheCloudNativePGClaim(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}

	_, err = e.RemoveAddon(ctx, info.Name, cp.RemoveAddonOptions{DeleteData: true})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError holding the removal", err)
	}
	if !strings.Contains(g.Message, info.Name+"-1") {
		t.Errorf("the confirmation %q does not name the claim it would destroy (%q)", g.Message, info.Name+"-1")
	}
}

// TestRemoveAddonTakesAFinalBackupBeforeDestroyingACloudNativePGInstance asserts ADR-0064 §5 is not
// weakened by the mechanism ADR-0066 chose. The final backup is a logical dump taken through the
// ordinary backup path, which reaches the instance at the endpoint every consumer resolves — so it
// works here for exactly the reason attach and `addon backup` do.
func TestRemoveAddonTakesAFinalBackupBeforeDestroyingACloudNativePGInstance(t *testing.T) {
	ctx := context.Background()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	creds := fake.NewCredentials()
	prov := fake.NewProvisioner()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock: fake.NewClock(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: fake.NewDNSFactory(),
		ObjectStore: fake.NewObjectStoreFactory(), DatabaseProvisioner: prov,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	seedObjectStoreProvider(t, d, creds, "backups")
	prov.SetAttachedApps(cp.DefaultEnvironment, "web", "api")

	res, err := e.RemoveAddon(ctx, info.Name, cp.RemoveAddonOptions{DeleteData: true, Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon --delete-data: %v", err)
	}
	if len(res.FinalBackups) != 2 {
		t.Fatalf("FinalBackups = %d, want one per attached database (api, web)", len(res.FinalBackups))
	}
	if !res.DataDeleted {
		t.Error("the data volume was not destroyed after the final backups succeeded")
	}
	if _, ok := k.AddonVolume(info.Name + "-1"); ok {
		t.Error("the operator's claim survived a --delete-data removal")
	}
}
