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

// These tests cover ADR-0064 §5: `addon remove --delete-data` takes a final backup to the registered
// object store BEFORE it destroys anything, and a failed backup aborts the removal.
//
// The one that matters is TestRemoveAddonAbortsWhenTheFinalBackupFails. Everything else here shapes
// the behaviour; that one is the failure the record exists to remove — a command that destroyed the
// data after failing to preserve it, leaving the operator believing a copy exists. That belief is
// only tested at the moment they go looking for it.

// newFinalBackupEngine builds an engine with the add-on, provisioner and object-store seams all
// wired, installs Postgres, registers one object-storage destination, and attaches two apps — the
// configuration in which §5 applies at all.
func newFinalBackupEngine(t *testing.T) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Provisioner, *fake.Credentials) {
	t.Helper()
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
	if _, err := e.InstallAddon(context.Background(), cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	seedObjectStoreProvider(t, d, creds, "backups")
	prov.SetAttachedApps(cp.DefaultEnvironment, "web", "api")
	return e, k, d, prov, creds
}

// assertNothingDestroyed is the assertion an aborted --delete-data has to satisfy: the data volume,
// the add-on's workload and its registry row are all exactly as they were. Any one of them missing
// means the removal got past the point the backup was supposed to gate.
func assertNothingDestroyed(t *testing.T, k *fake.Kubernetes, d *fake.Database) {
	t.Helper()
	ctx := context.Background()
	// The claim is the operator's `<instance>-1`, not the instance's own name (ADR-0066 §1).
	if _, ok := k.AddonVolume("burrow-postgres-1"); !ok {
		t.Error("the data volume was destroyed after the final backup failed — this is the data loss ADR-0064 §5 exists to prevent")
	}
	if ready, err := k.AddonReady(ctx, "burrow-postgres"); err != nil || !ready {
		t.Errorf("the add-on's workload was torn down after the final backup failed (ready=%v, err=%v)", ready, err)
	}
	if _, err := d.Addon(ctx, "burrow-postgres"); err != nil {
		t.Errorf("the registry row was deleted after the final backup failed: %v", err)
	}
}

// TestRemoveAddonTakesFinalBackupsBeforeDestroyingTheData is §5's happy path: every attached
// database is dumped to the registered object store, the removal then proceeds, and the result names
// the copies that outlived the instance.
func TestRemoveAddonTakesFinalBackupsBeforeDestroyingTheData(t *testing.T) {
	ctx := context.Background()
	e, k, _, _, _ := newFinalBackupEngine(t)

	res, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if !res.DataDeleted {
		t.Error("the data volume was not destroyed after the final backups succeeded")
	}
	if len(res.FinalBackups) != 2 {
		t.Fatalf("FinalBackups = %d, want one per attached database (api, web)", len(res.FinalBackups))
	}
	// Sorted, so the backups are taken in the same order every time and the result reads the same.
	if res.FinalBackups[0].App != "api" || res.FinalBackups[1].App != "web" {
		t.Errorf("final backups are for %q and %q, want api then web", res.FinalBackups[0].App, res.FinalBackups[1].App)
	}
	for _, b := range res.FinalBackups {
		// This pair is what "the bytes are safe" means in code: ADR-0063 §7 only lets a row say
		// completed at an object-store destination once the object was written and read back.
		if b.Status != cp.BackupCompleted {
			t.Errorf("final backup of %q has status %q, want %q", b.App, b.Status, cp.BackupCompleted)
		}
		if b.Destination != cp.BackupDestinationObjectStore {
			t.Errorf("final backup of %q went to %q, want the object store — an in-cluster dump shares a failure domain with the volume just destroyed", b.App, b.Destination)
		}
	}
	if res.FinalBackupSkipped {
		t.Error("result reports a skipped final backup on the path that took one")
	}
	// The Jobs were given the destination, so the dumps actually left the cluster.
	jobs := k.BackupJobs()
	if len(jobs) != 2 {
		t.Fatalf("RunBackupJob calls = %d, want 2", len(jobs))
	}
	for _, j := range jobs {
		if j.Dest == nil {
			t.Errorf("the final backup of %q ran with no object-storage destination", j.App)
		}
	}
}

// TestRemoveAddonAbortsWhenTheFinalBackupFails is the decisive property of ADR-0064 §5: a final
// backup that does not reach the store leaves the database, the claim and the registry row intact.
// Before this existed, --delete-data destroyed the volume regardless of whether anything had been
// preserved — the worst outcome available, because the operator asked for the data to go away AND
// believed a copy existed.
func TestRemoveAddonAbortsWhenTheFinalBackupFails(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, _ := newFinalBackupEngine(t)

	// The store answered and refused: the dump was taken and the bytes did not arrive.
	k.SetBackupSize(2048)
	k.SetBackupFailure(cp.BackupReasonStoreRejected, "the destination refused the write with 403")
	k.SetError(fake.OpRunBackupJob, errors.New("kube: job \"burrow-pg-backup-1\" failed"))

	_, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, Confirm: true})
	if err == nil {
		t.Fatal("RemoveAddon --delete-data succeeded after the final backup failed")
	}
	assertNothingDestroyed(t, k, d)

	// The refusal names the app whose database could not be preserved, the closed reason, and the
	// way out — so the operator is told WHY their removal was refused rather than that it failed.
	for _, want := range []string{"api", cp.BackupReasonStoreRejected, "nothing was removed", "--skip-final-backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// And no row claims a backup that did not happen.
	backups, lerr := d.ListBackups(ctx, "", "")
	if lerr != nil {
		t.Fatalf("ListBackups: %v", lerr)
	}
	for _, b := range backups {
		if b.Status == cp.BackupCompleted {
			t.Errorf("backup %s of %q is recorded completed after the write was refused", b.ID, b.App)
		}
	}
}

// TestRemoveAddonReportsWhyABlockedBackupJobFailed asserts a --delete-data blocked on a Job that
// cannot start says so, with the reason from ADR-0074 §2's closed set, rather than sitting out a
// deadline and reporting elapsed time. A timeout carries no signal distinguishing "slow" from
// "cannot start", so the reasonable next move on reading one is to run the same destructive command
// again.
func TestRemoveAddonReportsWhyABlockedBackupJobFailed(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, _ := newFinalBackupEngine(t)

	// What awaitJob returns for a backup pod no node can run (issue #352): the reason travels on the
	// outcome alongside the error.
	k.SetBackupFailure(cp.ReasonUnschedulable, "0/3 nodes are available: 3 Insufficient memory")
	k.SetError(fake.OpRunBackupJob, errors.New("kube: job \"burrow-pg-backup-1\" is blocked: Unschedulable"))

	_, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, Confirm: true})
	if err == nil {
		t.Fatal("RemoveAddon --delete-data succeeded with a backup Job that never started")
	}
	assertNothingDestroyed(t, k, d)
	if !strings.Contains(err.Error(), cp.ReasonUnschedulable) {
		t.Errorf("error %q does not carry the closed reason %q", err, cp.ReasonUnschedulable)
	}
	if !cp.IsIssueReason(cp.ReasonUnschedulable) {
		t.Error("the reason carried is not a member of the closed IssueReason set")
	}
	// The detail is the scheduler's own message, which is what makes the reason actionable.
	if !strings.Contains(err.Error(), "Insufficient memory") {
		t.Errorf("error %q drops the detail that says how to fix it", err)
	}
}

// TestRemoveAddonRetryAfterAFailedBackupDoesNotStack asserts a retried --delete-data neither piles
// up work nor half-deletes. A failed attempt leaves NOTHING behind to resume from — no partial
// teardown, no pending row — so the second attempt is the same attempt again, not a continuation of
// the first, and it does no more work than the first did.
func TestRemoveAddonRetryAfterAFailedBackupDoesNotStack(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, _ := newFinalBackupEngine(t)

	k.SetBackupFailure(cp.BackupReasonStoreUnreachable, "the destination did not answer after 4 attempts")
	k.SetError(fake.OpRunBackupJob, errors.New("kube: job failed"))

	opts := cp.RemoveAddonOptions{DeleteData: true, Confirm: true}
	if _, err := e.RemoveAddon(ctx, "burrow-postgres", opts); err == nil {
		t.Fatal("the first --delete-data succeeded after the final backup failed")
	}
	afterFirst := len(k.BackupJobs())
	if _, err := e.RemoveAddon(ctx, "burrow-postgres", opts); err == nil {
		t.Fatal("the retried --delete-data succeeded after the final backup failed again")
	}
	assertNothingDestroyed(t, k, d)

	// Each attempt aborts at the FIRST failure, so it dumps one database, not a growing set: a
	// partial set is work done for a removal that is not happening, and it is the shape most easily
	// misread as "the backup ran".
	if afterFirst != 1 {
		t.Errorf("the first attempt ran %d backup Jobs, want 1 (abort at the first failure)", afterFirst)
	}
	if got := len(k.BackupJobs()); got != afterFirst*2 {
		t.Errorf("two attempts ran %d backup Jobs, want %d — a retry must repeat the attempt, not extend it", got, afterFirst*2)
	}

	// Every row either failed or completed. A row stranded pending would be the residue that
	// accumulates across retries, and pending is exactly the state that reads as neither.
	backups, err := d.ListBackups(ctx, "", "")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("recorded backups = %d, want one per attempt", len(backups))
	}
	for _, b := range backups {
		if b.Status != cp.BackupFailed {
			t.Errorf("backup %s of %q has status %q, want %q", b.ID, b.App, b.Status, cp.BackupFailed)
		}
	}

	// And once the cause is fixed, the same command goes through — the retry path is not poisoned by
	// the failed attempts.
	k.SetError(fake.OpRunBackupJob, nil)
	res, err := e.RemoveAddon(ctx, "burrow-postgres", opts)
	if err != nil {
		t.Fatalf("RemoveAddon after the cause was fixed: %v", err)
	}
	if !res.DataDeleted || len(res.FinalBackups) != 2 {
		t.Errorf("after the fix: DataDeleted=%v, FinalBackups=%d, want true and 2", res.DataDeleted, len(res.FinalBackups))
	}
}

// TestRemoveAddonRefusesWhenTheInstanceWillNotSayWhatItHolds is the case that separates "no app is
// attached" from "the instance would not answer". Enumeration is best-effort for the CONFIRMATION
// (ADR-0064 §3) and must not be for the BACKUP: reading an unanswerable instance as an empty one
// destroys every database on it and reports that there was nothing to back up.
func TestRemoveAddonRefusesWhenTheInstanceWillNotSayWhatItHolds(t *testing.T) {
	ctx := context.Background()
	e, k, d, prov, _ := newFinalBackupEngine(t)
	prov.SetListError(errors.New("dial tcp: connection refused"))

	_, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, Confirm: true})
	if err == nil {
		t.Fatal("RemoveAddon --delete-data succeeded without knowing which databases it was destroying")
	}
	assertNothingDestroyed(t, k, d)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid — this is an actionable refusal, not a system failure", err)
	}
	if !strings.Contains(err.Error(), "--skip-final-backup") {
		t.Errorf("error %q does not name the way out; a wedged add-on that cannot be removed at all is the worse failure", err)
	}
	if len(k.BackupJobs()) != 0 {
		t.Error("a backup Job ran for an instance that would not say what it holds")
	}

	// The override is what keeps a wedged add-on removable, which is ADR-0064 §5's own limit on
	// itself: requiring a backup unconditionally trades data loss for an unrecoverable cluster.
	res, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, SkipFinalBackup: true, Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon --skip-final-backup over an unenumerable instance: %v", err)
	}
	if !res.DataDeleted || !res.FinalBackupSkipped {
		t.Errorf("DataDeleted=%v FinalBackupSkipped=%v, want both true", res.DataDeleted, res.FinalBackupSkipped)
	}
}

// TestRemoveAddonSkipFinalBackupSaysSoPlainly asserts the override announces itself. A removal that
// destroyed the data without a copy and did not say so is the same false assurance as a failed
// backup that was ignored — arrived at deliberately rather than by accident.
func TestRemoveAddonSkipFinalBackupSaysSoPlainly(t *testing.T) {
	ctx := context.Background()
	e, k, _, _, _ := newFinalBackupEngine(t)

	res, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, SkipFinalBackup: true, Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if !res.FinalBackupSkipped {
		t.Fatal("result does not report that no final backup was taken")
	}
	if !strings.Contains(res.FinalBackupNote, "--skip-final-backup") {
		t.Errorf("note %q does not say why no backup was taken", res.FinalBackupNote)
	}
	if len(res.FinalBackups) != 0 {
		t.Errorf("FinalBackups = %d on a skipped backup, want none", len(res.FinalBackups))
	}
	if len(k.BackupJobs()) != 0 {
		t.Error("a backup Job ran despite --skip-final-backup")
	}
}

// TestRemoveAddonWithNoObjectStoreIsUnchanged asserts ADR-0064 §5's stated limit: with nothing
// durable registered there is nowhere for a final backup to go, so --delete-data behaves exactly as
// it did — and says that no off-cluster copy was taken, since the retained backup claim (§4) is then
// the only copy there is.
func TestRemoveAddonWithNoObjectStoreIsUnchanged(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := installPostgres(t)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")

	res, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon with no object-storage provider: %v", err)
	}
	if !res.DataDeleted {
		t.Error("the volume was not destroyed; behaviour with no object store must be what it was")
	}
	if len(k.BackupJobs()) != 0 {
		t.Error("a backup Job ran with no object-storage destination to write it to")
	}
	if !res.FinalBackupSkipped || !strings.Contains(res.FinalBackupNote, "object-storage provider") {
		t.Errorf("FinalBackupSkipped=%v note=%q, want a plain statement that no off-cluster copy was taken",
			res.FinalBackupSkipped, res.FinalBackupNote)
	}
}

// TestRemoveAddonRefusesToGuessBetweenObjectStores mirrors resolveBackupDestination's refusal, for
// the same reason and with more at stake: ADR-0063 §6 allows several destinations on purpose, and
// picking one silently writes the LAST copy of every database somewhere nobody is watching.
func TestRemoveAddonRefusesToGuessBetweenObjectStores(t *testing.T) {
	ctx := context.Background()
	e, k, d, _, creds := newFinalBackupEngine(t)
	seedObjectStoreProvider(t, d, creds, "second")

	_, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, Confirm: true})
	if err == nil {
		t.Fatal("RemoveAddon --delete-data picked one of two object stores for the final backup")
	}
	assertNothingDestroyed(t, k, d)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "--backup-destination") {
		t.Errorf("error %q does not name the flag that resolves it", err)
	}
	// Naming one proceeds, and the backup goes to the one that was named.
	res, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, BackupDestination: "second", Confirm: true})
	if err != nil {
		t.Fatalf("RemoveAddon --backup-destination second: %v", err)
	}
	for _, b := range res.FinalBackups {
		if b.Provider != "second" {
			t.Errorf("final backup of %q went to provider %q, want the one that was named", b.App, b.Provider)
		}
	}
}

// TestRemoveAddonConfirmationSaysAFinalBackupIsTaken asserts the human approving the removal is told
// what will happen to the data BEFORE they approve it. "A copy is written off-cluster first and this
// is abandoned if that fails" and "this is the last moment the data exists" are different decisions,
// and a confirmation that does not distinguish them is not informed consent (ADR-0006).
func TestRemoveAddonConfirmationSaysAFinalBackupIsTaken(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newFinalBackupEngine(t)

	_, err := e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError holding the removal", err)
	}
	for _, want := range []string{"final backup", "backups", "abandoned if it fails"} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("confirmation message %q does not mention %q", g.Message, want)
		}
	}

	// And the other way: the message says plainly when no copy will be made, because that is the
	// version of this decision that cannot be taken back.
	_, err = e.RemoveAddon(ctx, "burrow-postgres", cp.RemoveAddonOptions{DeleteData: true, SkipFinalBackup: true})
	g, ok = cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError holding the removal", err)
	}
	if !strings.Contains(g.Message, "no final backup is taken") {
		t.Errorf("confirmation message %q does not say that no backup will be taken", g.Message)
	}
}
