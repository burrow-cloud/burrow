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
)

// This file holds the tests for issue #349: backups inherit ADR-0067 §1's per-environment isolation
// instead of escaping it.
//
// The hazard is worth restating, because it is what makes these assertions the point rather than a
// formality. Once the instance became per-environment, `RunBackupJob` correctly used the environment
// to pick the SERVER it dumped — and then wrote the dump into one shared claim, under one shared
// `<app>/` directory, whatever environment it came from. Staging's and production's dumps for an app
// of the same name landed on the same disk, and the backup and restore Jobs of either environment
// mounted that disk whole. The registry knew better than the volume did: the rows said which
// environment each dump came from while nothing on the disk did.
//
// That asymmetry is the dangerous shape, and it is the reason the tests below check the CLAIM and
// not only the row. A restore refused on the row alone would still have been reading bytes it should
// never have been able to reach.

// TestBackupVolumeNamePerEnvironment pins the derivation every layer shares: an environment resolves
// to a claim, the default environment resolves to the claim an existing install already has, and no
// two environments — and no environment and instance — can resolve to the same name.
func TestBackupVolumeNamePerEnvironment(t *testing.T) {
	// The default environment's claim is the name that already exists, which is what makes this
	// change move no bytes: every dump taken before backups were per-environment is on it, and it
	// keeps being the claim the default environment mounts.
	def, err := cp.BackupVolumeName(cp.AddonPostgres, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("BackupVolumeName(default): %v", err)
	}
	if def != cp.PostgresBackupVolume {
		t.Errorf("default-environment backup claim = %q, want %q (no existing dump may move)", def, cp.PostgresBackupVolume)
	}

	staging, err := cp.BackupVolumeName(cp.AddonPostgres, "staging")
	if err != nil {
		t.Fatalf("BackupVolumeName(staging): %v", err)
	}
	if staging == def {
		t.Fatalf("staging and the default environment resolved to the same backup claim %q — the shared claim is still expressible", staging)
	}

	// No name is produced twice, across environments AND across the two families that share the
	// add-on namespace. The second half is the one worth asserting: an environment name is a
	// DNS-1123 label and an instance name is `burrow-<type>[-<env>]`, so a claim family separated
	// from the instance by a hyphen would let an environment called `staging-backups` name its
	// instance exactly what `staging`'s backup claim is called.
	seen := map[string]string{}
	envs := []string{cp.DefaultEnvironment, "staging", "preprod", "dev", "staging-backups", "backups-staging", "prod-backups"}
	for _, env := range envs {
		for _, typ := range []cp.AddonType{cp.AddonPostgres, cp.AddonLogs, cp.AddonMetrics, cp.AddonCache} {
			instance, ierr := cp.AddonInstanceName(typ, env)
			if ierr != nil {
				t.Fatalf("AddonInstanceName(%s, %s): %v", typ, env, ierr)
			}
			if prev, dup := seen[instance]; dup {
				t.Errorf("name %q is produced by both %s and the %s instance of %s", instance, prev, typ, env)
			}
			seen[instance] = "the " + string(typ) + " instance of " + env

			claim, cerr := cp.BackupVolumeName(typ, env)
			if cerr != nil {
				t.Fatalf("BackupVolumeName(%s, %s): %v", typ, env, cerr)
			}
			if prev, dup := seen[claim]; dup {
				t.Errorf("name %q is produced by both %s and the %s backup claim of %s", claim, prev, typ, env)
			}
			seen[claim] = "the " + string(typ) + " backup claim of " + env
		}
	}

	// An unnamed environment is an error, not a synonym for the default: a signature that can omit
	// it is a signature that will omit it, and the claim it would land on holds another
	// environment's dumps (ADR-0067 §1).
	if _, err := cp.BackupVolumeName(cp.AddonPostgres, ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("BackupVolumeName with no environment = %v, want ErrInvalid", err)
	}
	if _, err := cp.BackupVolumeName(cp.AddonPostgres, "Not A Label"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("BackupVolumeName with a malformed environment = %v, want ErrInvalid", err)
	}
}

// TestBackupsEnvironmentIsReserved asserts the one environment name that would collide with the
// default environment's backup claim is refused — and refused by the accessor a front end checks
// BEFORE it creates a namespace, not only by the engine afterwards.
//
// Without it, `burrow env add backups` followed by installing Postgres would create the instance's
// data claim under a name that already exists. That is not an error: the create returns
// AlreadyExists and the instance comes up on the volume holding every dump.
func TestBackupsEnvironmentIsReserved(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEnvPostgresEngine(t, "burrow-apps")

	if _, err := e.AddEnvironment(ctx, "backups", "burrow-apps-backups"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("AddEnvironment(backups) = %v, want ErrInvalid", err)
	}
	found := false
	for _, name := range cp.ReservedEnvironmentNames() {
		if name == "backups" {
			found = true
		}
	}
	if !found {
		t.Errorf("ReservedEnvironmentNames() = %v, want it to include \"backups\" — a caller that refuses before it provisions reads this list", cp.ReservedEnvironmentNames())
	}
	// The collision it closes, stated as the assertion: an environment of that name would name its
	// instance what the default environment's backup claim is called.
	instance, err := cp.AddonInstanceName(cp.AddonPostgres, "backups")
	if err != nil {
		t.Fatalf("AddonInstanceName(backups): %v", err)
	}
	if instance != cp.PostgresBackupVolume {
		t.Errorf("the reservation guards %q, but the instance of an environment called \"backups\" is %q — one of the two moved", cp.PostgresBackupVolume, instance)
	}
}

// TestBackupsInTwoEnvironmentsLandOnTwoClaims is THE test for issue #349: the same app name backed
// up in two environments must end up on two claims, and neither environment's Job may mount the
// other's.
//
// It is written to fail against the old code rather than to pass against the new. Before this change
// both backups recorded the same path on the same shared claim; the assertion that the two claims
// differ is what would have caught it.
func TestBackupsInTwoEnvironmentsLandOnTwoClaims(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	prod, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", cp.DefaultEnvironment, "")
	if err != nil {
		t.Fatalf("BackupAddon(prod): %v", err)
	}
	staging, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "staging", "")
	if err != nil {
		t.Fatalf("BackupAddon(staging): %v", err)
	}

	if prod.Backup.Volume != cp.PostgresBackupVolume {
		t.Errorf("the default environment's backup landed on %q, want %q", prod.Backup.Volume, cp.PostgresBackupVolume)
	}
	if prod.Backup.Volume == staging.Backup.Volume {
		t.Fatalf("both environments' dumps landed on the same claim %q — one disk still holds both (issue #349)", prod.Backup.Volume)
	}
	// The PATH within a claim is deliberately unchanged, which is why the claim has to carry the
	// isolation: on one shared volume these two dumps would have been siblings in one directory.
	if !strings.HasPrefix(prod.Backup.Path, "/backups/web/") || !strings.HasPrefix(staging.Backup.Path, "/backups/web/") {
		t.Errorf("paths = %q and %q, want both under /backups/web/", prod.Backup.Path, staging.Backup.Path)
	}

	// Both claims exist in the cluster, each attributed to its own environment — a backup taken in
	// staging must not have created production's claim, or the reverse.
	vols, err := k.AddonVolumes(ctx)
	if err != nil {
		t.Fatalf("AddonVolumes: %v", err)
	}
	byName := map[string]cp.AddonVolume{}
	for _, v := range vols {
		byName[v.Name] = v
	}
	for _, want := range []struct{ claim, env string }{
		{prod.Backup.Volume, cp.DefaultEnvironment},
		{staging.Backup.Volume, "staging"},
	} {
		got, ok := byName[want.claim]
		if !ok {
			t.Fatalf("claim %q was never created; cluster holds %v", want.claim, vols)
		}
		if got.Role != cp.AddonVolumeBackup {
			t.Errorf("claim %q holds %q, want %q", want.claim, got.Role, cp.AddonVolumeBackup)
		}
		if got.Environment != want.env {
			t.Errorf("claim %q is attributed to environment %q, want %q", want.claim, got.Environment, want.env)
		}
	}

	// And the rows agree with the disk: what the registry says about a dump is where it actually is.
	stored, err := d.GetBackup(ctx, staging.Backup.ID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if stored.Volume != staging.Backup.Volume || stored.Environment != "staging" {
		t.Errorf("stored row = %q on %q, want staging on %q", stored.Environment, stored.Volume, staging.Backup.Volume)
	}
}

// TestRestoreRefusesAnotherEnvironmentsBackup asserts the restore of a dump taken in one environment
// into another is refused — in BOTH directions, and by the row as well as by the volume.
//
// The two checks are not redundant. The environment on the row says which instance a dump was taken
// from; the claim says which disk it is on. A restore that only checked the first would still be
// mounting a volume it has no business reading.
func TestRestoreRefusesAnotherEnvironmentsBackup(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	prod, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", cp.DefaultEnvironment, "")
	if err != nil {
		t.Fatalf("BackupAddon(prod): %v", err)
	}
	staging, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "staging", "")
	if err != nil {
		t.Fatalf("BackupAddon(staging): %v", err)
	}

	// Staging's dump into production, and production's into staging. Neither is a valid source, and
	// the one that would overwrite live production data is the reason this is refused rather than
	// warned about.
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", staging.Backup.ID, cp.DefaultEnvironment, true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("restoring staging's dump into the default environment = %v, want ErrInvalid", err)
	}
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", prod.Backup.ID, "staging", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("restoring the default environment's dump into staging = %v, want ErrInvalid", err)
	}
	if jobs := k.RestoreJobs(); len(jobs) != 0 {
		t.Fatalf("a refused restore must not run a Job, got %+v", jobs)
	}

	// Each environment can still restore its own, which is the property the refusal must not cost.
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", staging.Backup.ID, "staging", true); err != nil {
		t.Fatalf("restoring staging's own dump: %v", err)
	}
	jobs := k.RestoreJobs()
	if len(jobs) != 1 || jobs[0].Env != "staging" {
		t.Fatalf("RestoreJobs = %+v, want one call in staging", jobs)
	}
}

// TestRestoreRefusesADumpOnAnotherClaim covers the population the environment check alone cannot
// see: a backup taken in a NON-default environment before backups were per-environment. Its row
// says `staging` and its bytes are on the shared claim, which staging no longer mounts.
//
// The refusal is the decision: reaching that dump would mean mounting the volume holding every other
// environment's dumps into staging's Job, which is the isolation this closed. So it is refused
// naming the claim the dump is actually on — a legible refusal rather than a Job that reports a
// missing file, and nothing is destroyed either way.
func TestRestoreRefusesADumpOnAnotherClaim(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	// A row exactly as the pre-change code would have left it, and as migration 00027 backfills it:
	// taken in staging, written to the one claim that existed.
	legacy := cp.Backup{
		ID:          "bk-legacy",
		App:         "web",
		Environment: "staging",
		CreatedAt:   time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC),
		Volume:      cp.PostgresBackupVolume,
		Path:        cp.BackupPath("web", "bk-legacy"),
		Status:      cp.BackupCompleted,
		Destination: cp.BackupDestinationCluster,
	}
	if err := d.RecordBackup(ctx, legacy); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", legacy.ID, "staging", true)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("restore of a dump on the shared claim = %v, want ErrInvalid", err)
	}
	// The refusal has to say WHERE the dump is, or the operator is left with a backup they cannot
	// find and no reason given.
	if !strings.Contains(err.Error(), cp.PostgresBackupVolume) {
		t.Errorf("refusal does not name the claim the dump is on: %v", err)
	}
	if jobs := k.RestoreJobs(); len(jobs) != 0 {
		t.Errorf("a refused restore must not run a Job, got %+v", jobs)
	}

	// The same row in the DEFAULT environment is on the claim that environment mounts, so it
	// restores exactly as it always did. That is the whole population an existing install has.
	kept := legacy
	kept.ID, kept.Environment = "bk-kept", cp.DefaultEnvironment
	kept.Path = cp.BackupPath("web", kept.ID)
	if err := d.RecordBackup(ctx, kept); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", kept.ID, cp.DefaultEnvironment, true); err != nil {
		t.Fatalf("a backup recorded before this change must still restore: %v", err)
	}
}

// TestRemoveRetainsThisEnvironmentsBackupClaim asserts a data-deleting removal reports the backup
// claim of the environment it acted in (ADR-0064 §4). Naming the compiled-in constant would tell an
// operator tearing down staging that production's dumps were what survived.
func TestRemoveRetainsThisEnvironmentsBackupClaim(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	instance, err := cp.AddonInstanceName(cp.AddonPostgres, "staging")
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, "staging", true); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	backup, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "staging", "")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}

	res, err := e.RemoveAddon(ctx, instance, cp.RemoveAddonOptions{DeleteData: true, Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if res.RetainedBackupVolume != backup.Backup.Volume {
		t.Errorf("retained backup volume = %q, want staging's own claim %q", res.RetainedBackupVolume, backup.Backup.Volume)
	}
	if res.RetainedBackupVolume == cp.PostgresBackupVolume {
		t.Errorf("removing staging's instance reported the default environment's claim %q as what survived", cp.PostgresBackupVolume)
	}
}
