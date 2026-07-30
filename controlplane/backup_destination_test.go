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

// These tests cover the WRITE half of ADR-0063 (§7): where a backup goes, what the row is allowed to
// claim about it, and what is recorded when it does not get there.
//
// The one that matters most is TestBackupAddonFailedWriteLeavesNoCompletedRow. Everything else here
// is about making a backup legible; that one is about the failure this issue exists to remove — a
// `Backup` row saying "succeeded" for bytes that never left the cluster, which converts a missing
// backup into a false assurance and is only ever tested at restore time.

// newBackupDestinationEngine builds an engine with both the add-on and object-store seams wired, and
// seeds one registered object-storage provider with its credential pair, so a backup has somewhere
// durable to go.
func newBackupDestinationEngine(t *testing.T) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Credentials) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	creds := fake.NewCredentials()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock: fake.NewClock(time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: fake.NewDNSFactory(),
		ObjectStore: fake.NewObjectStoreFactory(), DatabaseProvisioner: fake.NewProvisioner(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, d, creds
}

// seedObjectStoreProvider registers an object-storage provider row and its credential pair directly,
// which is what `provider add` leaves behind once the destination has verified (ADR-0063 §1).
func seedObjectStoreProvider(t *testing.T, d *fake.Database, creds *fake.Credentials, name string) {
	t.Helper()
	ctx := context.Background()
	idKey, secretKey := name+".access-key-id", name+".secret-access-key"
	if err := d.SaveProvider(ctx, cp.Provider{
		Name:         name,
		Type:         cp.ProviderS3,
		Capabilities: cp.ProviderS3.Capabilities(),
		SecretKey:    idKey,
		ObjectStore: &cp.ObjectStoreConfig{
			Endpoint:           testEndpoint,
			Region:             "us-west-002",
			Bucket:             "burrow-backups-" + name,
			AccessKeyIDKey:     idKey,
			SecretAccessKeyKey: secretKey,
		},
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	if err := creds.SetToken(ctx, idKey, testKeyID); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	if err := creds.SetToken(ctx, secretKey, testSecret); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
}

// TestBackupAddonFailedWriteLeavesNoCompletedRow is the invariant of ADR-0063 §7: when the write to
// the destination does not succeed, NOTHING records a completed backup. Not the returned result, not
// the row, not the listing. The reason is recorded instead, from the closed set, so the next reader
// learns what happened rather than that something did.
func TestBackupAddonFailedWriteLeavesNoCompletedRow(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds := newBackupDestinationEngine(t)
	seedObjectStoreProvider(t, d, creds, "backups")

	// The Job fails the way it fails when the store would not take the write: the dump was made, and
	// the bytes did not arrive.
	k.SetBackupSize(2048)
	k.SetBackupFailure(cp.BackupReasonStoreUnreachable, "the destination did not complete the write after 4 attempts")
	k.SetError(fake.OpRunBackupJob, errors.New("kube: job \"burrow-pg-backup-1\" failed"))

	if _, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", ""); err == nil {
		t.Fatal("BackupAddon should error when the backup does not reach its destination")
	}

	backups, err := d.ListBackups(ctx, "web", "")
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("ListBackups = %d rows, want 1", len(backups))
	}
	got := backups[0]
	if got.Status == cp.BackupCompleted {
		t.Fatal("a backup that did not reach the store is recorded as completed — this is the failure ADR-0063 §7 exists to remove")
	}
	if got.Status != cp.BackupFailed {
		t.Errorf("status = %q, want %q", got.Status, cp.BackupFailed)
	}
	if got.FailureReason != cp.BackupReasonStoreUnreachable {
		t.Errorf("failure reason = %q, want %q", got.FailureReason, cp.BackupReasonStoreUnreachable)
	}
	if !cp.IsBackupFailureReason(got.FailureReason) {
		t.Errorf("failure reason %q is not a member of the closed set", got.FailureReason)
	}
	// A failed backup carries no size: a length on the row would read like a partial success.
	if got.SizeBytes != 0 {
		t.Errorf("size = %d on a failed backup, want 0", got.SizeBytes)
	}
	// And the destination is still on the row, so the failure is legible as "this did not reach the
	// store" rather than as an unattributed failure.
	if got.Destination != cp.BackupDestinationObjectStore || got.Provider != "backups" {
		t.Errorf("destination = %q/%q, want object-store/backups", got.Destination, got.Provider)
	}
}

// TestBackupAddonBlockedJobRecordsItsIssueReason asserts a Job that never STARTED records the
// ADR-0074 §2 reason the waiter returned, rather than being flattened into a generic backup failure.
// The two vocabularies share the field on purpose: what a reader needs is the closed reason, and
// which set it came from is answered by IsBackupFailureReason.
func TestBackupAddonBlockedJobRecordsItsIssueReason(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds := newBackupDestinationEngine(t)
	seedObjectStoreProvider(t, d, creds, "backups")

	k.SetBackupFailure(cp.ReasonUnschedulable, "no node has room for the backup Job")
	k.SetError(fake.OpRunBackupJob, errors.New("kube: job blocked"))

	if _, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", ""); err == nil {
		t.Fatal("BackupAddon should error when the Job cannot start")
	}
	backups, _ := d.ListBackups(ctx, "web", "")
	if len(backups) != 1 || backups[0].FailureReason != cp.ReasonUnschedulable {
		t.Fatalf("failure reason = %+v, want %q", backups, cp.ReasonUnschedulable)
	}
	if cp.IsBackupFailureReason(backups[0].FailureReason) {
		t.Error("Unschedulable should not be a member of the BACKUP reason set; it is an ADR-0074 §2 IssueReason")
	}
	if !cp.IsIssueReason(backups[0].FailureReason) {
		t.Errorf("%q is not a member of either closed set", backups[0].FailureReason)
	}
}

// TestBackupAddonWritesToRegisteredDestination asserts the engine resolves the one registered
// object-storage provider, hands the Job the destination AND the credential pair read at call time,
// and records the object key on the row.
func TestBackupAddonWritesToRegisteredDestination(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds := newBackupDestinationEngine(t)
	seedObjectStoreProvider(t, d, creds, "backups")
	k.SetBackupSize(4096)

	res, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", "")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	if res.Backup.Destination != cp.BackupDestinationObjectStore {
		t.Errorf("destination = %q, want %q", res.Backup.Destination, cp.BackupDestinationObjectStore)
	}
	wantKey := cp.BackupObjectKey("web", cp.DefaultEnvironment, res.Backup.ID)
	if res.Backup.ObjectKey != wantKey {
		t.Errorf("object key = %q, want %q", res.Backup.ObjectKey, wantKey)
	}

	jobs := k.BackupJobs()
	if len(jobs) != 1 || jobs[0].Dest == nil {
		t.Fatalf("BackupJobs = %+v, want one call carrying a destination", jobs)
	}
	dest := jobs[0].Dest
	if dest.Provider != "backups" || dest.Config.Bucket != "burrow-backups-backups" || dest.Key != wantKey {
		t.Errorf("destination = %+v, want the registered provider's bucket and this backup's key", dest)
	}
	// The credential pair is read at call time from the credential store, so a rotated key is used
	// without a restart — and it is handed to the Job, not to anything that persists it.
	if dest.Credential.AccessKeyID != testKeyID || dest.Credential.SecretAccessKey != testSecret {
		t.Error("the Job was not given the registered credential pair")
	}
}

// TestBackupAddonWithNoDestinationStaysInTheCluster asserts an install with no object-storage
// provider still takes backups, and that the row SAYS they only reached the cluster. Refusing to
// back up at all would remove the weaker backup as well as the stronger one; leaving the field blank
// would let a set of in-cluster dumps read as a backup strategy.
func TestBackupAddonWithNoDestinationStaysInTheCluster(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newBackupDestinationEngine(t)
	k.SetBackupSize(2048)

	res, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", "")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	if res.Backup.Status != cp.BackupCompleted {
		t.Errorf("status = %q, want completed", res.Backup.Status)
	}
	if res.Backup.Destination != cp.BackupDestinationCluster {
		t.Errorf("destination = %q, want %q", res.Backup.Destination, cp.BackupDestinationCluster)
	}
	if res.Backup.ObjectKey != "" || res.Backup.Provider != "" {
		t.Errorf("an in-cluster backup carries an object address: %+v", res.Backup)
	}
	if jobs := k.BackupJobs(); len(jobs) != 1 || jobs[0].Dest != nil {
		t.Errorf("BackupJobs = %+v, want one call with no destination", jobs)
	}
	stored, _ := d.GetBackup(ctx, res.Backup.ID)
	if stored.Destination != cp.BackupDestinationCluster {
		t.Errorf("stored destination = %q, want cluster", stored.Destination)
	}
}

// TestBackupAddonRefusesAnAmbiguousDestination asserts that with several object-storage providers
// registered the backup names one or does not happen. Choosing silently is how backups quietly stop
// arriving at the destination somebody is watching (ADR-0063 §6).
func TestBackupAddonRefusesAnAmbiguousDestination(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds := newBackupDestinationEngine(t)
	seedObjectStoreProvider(t, d, creds, "b2")
	seedObjectStoreProvider(t, d, creds, "r2")

	_, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", "")
	if err == nil {
		t.Fatal("BackupAddon should refuse when it cannot tell which destination holds the backup")
	}
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
	// The refusal names both, so the operator can pick without going looking.
	for _, name := range []string{"b2", "r2"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name provider %q", err, name)
		}
	}
	// And it happens BEFORE anything is recorded or run: an ambiguous destination must not leave a
	// pending row nothing will ever finish.
	if jobs := k.BackupJobs(); len(jobs) != 0 {
		t.Errorf("BackupJobs = %+v, want none", jobs)
	}
	backups, _ := d.ListBackups(ctx, "", "")
	if len(backups) != 0 {
		t.Errorf("ListBackups = %+v, want no row", backups)
	}
}

// TestBackupAddonNamedDestinationChoosesIt asserts --destination selects among several, and that an
// unknown name is ErrNotFound rather than a silent fall-through to some other provider.
func TestBackupAddonNamedDestinationChoosesIt(t *testing.T) {
	ctx := context.Background()
	e, k, d, creds := newBackupDestinationEngine(t)
	seedObjectStoreProvider(t, d, creds, "b2")
	seedObjectStoreProvider(t, d, creds, "r2")

	res, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", "r2")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	if res.Backup.Provider != "r2" {
		t.Errorf("provider = %q, want r2", res.Backup.Provider)
	}
	if jobs := k.BackupJobs(); len(jobs) != 1 || jobs[0].Dest.Config.Bucket != "burrow-backups-r2" {
		t.Errorf("BackupJobs = %+v, want the r2 bucket", jobs)
	}

	if _, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", "nope"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("naming an unregistered destination = %v, want ErrNotFound", err)
	}
}

// TestBackupObjectKeyIsEnvironmentScoped pins the object layout, which two independent things depend
// on: the row the engine records and the key the Job writes. A drift between them would put the dump
// somewhere the recorded address does not point.
func TestBackupObjectKeyIsEnvironmentScoped(t *testing.T) {
	key := cp.BackupObjectKey("web", "staging", "bkp-1")
	if key != "burrow/backups/staging/web/bkp-1.dump" {
		t.Errorf("key = %q, want burrow/backups/staging/web/bkp-1.dump", key)
	}
	// An empty environment is the default one, exactly as it is on the row (ADR-0067 §2).
	if got := cp.BackupObjectKey("web", "", "bkp-1"); got != cp.BackupObjectKey("web", cp.DefaultEnvironment, "bkp-1") {
		t.Errorf("empty environment key = %q, want the default environment's", got)
	}
}

// TestBackupResultCarriesNoCredential is the standing rule, asserted rather than assumed: nothing an
// operator or an agent receives from a backup contains either half of the destination credential.
func TestBackupResultCarriesNoCredential(t *testing.T) {
	ctx := context.Background()
	e, _, d, creds := newBackupDestinationEngine(t)
	seedObjectStoreProvider(t, d, creds, "backups")

	res, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", "")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	rendered := strings.Join([]string{
		res.Backup.ID, res.Backup.App, res.Backup.Path, res.Backup.ObjectKey,
		res.Backup.Provider, string(res.Backup.Destination), res.Backup.FailureReason, res.Backup.FailureDetail,
	}, " ")
	for _, secret := range []string{testKeyID, testSecret} {
		if strings.Contains(rendered, secret) {
			t.Fatal("a backup result carries a credential value")
		}
	}
	// Nor does the audit trail, which records names and never values.
	entries, err := d.Audit(ctx, cp.AuditFilter{})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	for _, entry := range entries {
		for _, v := range entry.Args {
			if v == testKeyID || v == testSecret {
				t.Fatal("an audit row carries a credential value")
			}
		}
	}
}
