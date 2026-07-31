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

// TestRemoveAddonKeepsTheDataOfACloudNativePGInstance is ADR-0064 §1 over the mechanism ADR-0066 §1
// introduced. The record's default does not move because the implementation did: removing a
// CloudNativePG-backed instance tears the `Cluster` down and KEEPS the disk every attached app's
// database lives on.
//
// It asserts the retained claim by name because the name is the operator's, not Burrow's. A
// `Cluster` composes one claim per instance and calls it `<instance>-1`, so a removal that reported
// (or looked for) `<instance>` would name a volume that does not exist and leave the one that does.
func TestRemoveAddonKeepsTheDataOfACloudNativePGInstance(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
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
// It matters more under this mechanism than under the other one. The operator's claims deliberately
// do not carry Burrow's selectable label while the `Cluster` owns them — a live instance's disk is
// not a retained volume — so if nothing added it at removal time the §6 listing would be empty for
// every CloudNativePG-backed instance ever removed, and "your data was kept" would be a promise with
// no way to find what was kept.
func TestRemovedCloudNativePGVolumeIsListedAsRetained(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
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

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
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
// the volume BY NAME, and under this mechanism that name is the operator's rather than the
// instance's. A confirmation that names a claim which does not exist is worse than one that names
// none — it reads as precise, and the operator approves it on that basis.
func TestRemoveAddonConfirmationNamesTheCloudNativePGClaim(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
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
// weakened by the mechanism. The final backup is a logical dump taken through the ordinary backup
// path, which reaches the instance at the endpoint every consumer resolves — so it works here for
// exactly the reason attach and `addon backup` do, and nothing about it is conditional on how the
// server is run.
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
	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
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

// TestAddonInfoMechanismRoundTripsThroughTheRegistry asserts the mechanism a removal acts on is the
// one the install recorded. It is the fact the whole removal path hangs off: the alternative is
// working the mechanism out by probing the cluster, and every way of failing to probe the cluster
// reads as "there is no such object" — which on a removal path means walking past a running database
// and deleting the row that named it.
func TestAddonInfoMechanismRoundTripsThroughTheRegistry(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{
		Mechanism: cp.AddonMechanismCloudNativePG, Confirm: true,
	})
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	stored, err := d.Addon(ctx, info.Name)
	if err != nil {
		t.Fatalf("reading the registry row back: %v", err)
	}
	if stored.Mechanism() != cp.AddonMechanismCloudNativePG {
		t.Errorf("mechanism = %q, want %q", stored.Mechanism(), cp.AddonMechanismCloudNativePG)
	}
	// Anything else is the catalog's own mechanism, including a row written before there was a
	// choice — the direction whose teardown fails safe.
	if (cp.AddonInfo{Backend: "postgres"}).Mechanism() != cp.AddonMechanismDefault {
		t.Error("a Deployment-backed instance does not report the default mechanism")
	}
}
