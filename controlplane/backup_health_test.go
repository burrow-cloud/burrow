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

// These tests cover ADR-0063 §7's status surface and ADR-0066 §5's requirement that the backup-age
// signal comes from what BURROW observed. The mechanism the backups were taken by is deliberately
// absent from every assertion below: the surface reads Burrow's own rows, so replacing the add-on's
// mechanism (ADR-0066 §1) does not change any of these answers.

// healthNow is the moment the injected clock reports, so every age in these tests is exact.
var healthNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// newBackupHealthEngine builds an engine with the object-store seam wired and returns the database
// (to seed backup rows) and the object store (to make a destination answer, or not).
func newBackupHealthEngine(t *testing.T) (*cp.Engine, *fake.Database, *fake.ObjectStoreFactory) {
	t.Helper()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	osf := fake.NewObjectStoreFactory()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(healthNow),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(), ObjectStore: osf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, d, osf
}

// seedBackup records one backup row exactly as it would have been left behind, at a chosen age.
func seedBackup(t *testing.T, d *fake.Database, b cp.Backup) {
	t.Helper()
	if err := d.RecordBackup(context.Background(), b); err != nil {
		t.Fatalf("RecordBackup(%s): %v", b.ID, err)
	}
}

// completed builds a completed backup row at the object store, ago before the report's moment.
func completedDurable(id, app string, ago time.Duration) cp.Backup {
	return cp.Backup{
		ID: id, App: app, Environment: "prod", CreatedAt: healthNow.Add(-ago),
		Status: cp.BackupCompleted, Destination: cp.BackupDestinationObjectStore,
		Provider: "b2", ObjectKey: "backups/prod/" + app + "/" + id + ".dump", SizeBytes: 4096,
	}
}

// completedCluster builds a completed backup row that never left the cluster.
func completedCluster(id, app string, ago time.Duration) cp.Backup {
	return cp.Backup{
		ID: id, App: app, Environment: "prod", CreatedAt: healthNow.Add(-ago),
		Status: cp.BackupCompleted, Destination: cp.BackupDestinationCluster,
		Path: cp.BackupPath(app, id), SizeBytes: 2048,
	}
}

// TestBackupHealthSaysNeverWhenNothingIsRecorded asserts an install that has never backed anything
// up is reported as such, and told how to take one — rather than as an empty, healthy-looking report.
func TestBackupHealthSaysNeverWhenNothingIsRecorded(t *testing.T) {
	e, _, _ := newBackupHealthEngine(t)

	h, err := e.BackupHealth(context.Background(), cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if h.State != cp.BackupHealthNever {
		t.Errorf("state = %q, want %q", h.State, cp.BackupHealthNever)
	}
	if h.LastSuccess != nil || h.LastDurableSuccess != nil || h.LastFailure != nil {
		t.Errorf("nothing recorded should yield no observations, got %+v", h)
	}
	if !strings.Contains(h.Summary, "none recorded") {
		t.Errorf("summary = %q, want it to say nothing is recorded", h.Summary)
	}
	if h.ObservedAt != healthNow {
		t.Errorf("observed at %v, want the injected clock's %v", h.ObservedAt, healthNow)
	}
}

// TestBackupHealthReportsClusterOnlyWhenNoBackupLeftTheCluster is the distinction ADR-0063 exists to
// draw: dumps on an in-cluster volume are real backups and are not durable ones, and a health surface
// that reported them as coverage would let a wall of them read as a backup strategy.
func TestBackupHealthReportsClusterOnlyWhenNoBackupLeftTheCluster(t *testing.T) {
	e, d, _ := newBackupHealthEngine(t)
	seedBackup(t, d, completedCluster("bk-1", "web", 3*time.Hour))

	h, err := e.BackupHealth(context.Background(), cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if h.State != cp.BackupHealthClusterOnly {
		t.Errorf("state = %q, want %q", h.State, cp.BackupHealthClusterOnly)
	}
	if h.LastSuccess == nil || h.LastSuccess.ID != "bk-1" {
		t.Fatalf("last success = %+v, want bk-1", h.LastSuccess)
	}
	if h.LastDurableSuccess != nil {
		t.Errorf("an in-cluster dump is not a durable success, got %+v", h.LastDurableSuccess)
	}
	if h.LastSuccess.AgeSeconds != int64(3*time.Hour/time.Second) {
		t.Errorf("age = %ds, want %d", h.LastSuccess.AgeSeconds, int64(3*time.Hour/time.Second))
	}
	if !strings.Contains(h.Summary, "no backup has ever left this cluster") {
		t.Errorf("summary = %q, want it to say no backup left the cluster", h.Summary)
	}
}

// TestBackupHealthReportsBothAgesWhenTheNewestSuccessStayedInTheCluster is the case that makes two
// ages necessary rather than decorative: the most recent backup succeeded, and the most recent
// backup that would survive losing the cluster is much older. One number cannot say both.
func TestBackupHealthReportsBothAgesWhenTheNewestSuccessStayedInTheCluster(t *testing.T) {
	e, d, _ := newBackupHealthEngine(t)
	seedBackup(t, d, completedDurable("bk-old", "web", 48*time.Hour))
	seedBackup(t, d, completedCluster("bk-new", "web", 1*time.Hour))

	h, err := e.BackupHealth(context.Background(), cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if h.State != cp.BackupHealthDurable {
		t.Errorf("state = %q, want %q", h.State, cp.BackupHealthDurable)
	}
	if h.LastSuccess == nil || h.LastSuccess.ID != "bk-new" {
		t.Fatalf("last success = %+v, want the newest completed row bk-new", h.LastSuccess)
	}
	if h.LastDurableSuccess == nil || h.LastDurableSuccess.ID != "bk-old" {
		t.Fatalf("last durable success = %+v, want bk-old", h.LastDurableSuccess)
	}
	if h.LastDurableSuccess.AgeSeconds != int64(48*time.Hour/time.Second) {
		t.Errorf("durable age = %ds, want %d", h.LastDurableSuccess.AgeSeconds, int64(48*time.Hour/time.Second))
	}
	if !strings.Contains(h.Summary, "2d ago") {
		t.Errorf("summary = %q, want the OFF-CLUSTER age (2d), not the newest success's", h.Summary)
	}
}

// TestBackupHealthNeverCountsAPendingRowAsSuccess: a burrowd that died mid-Job leaves a pending row,
// and a surface that counted it would reset the age on a backup that never happened — the precise
// failure ADR-0063 §7 is about, arriving from inside rather than from the vendor.
func TestBackupHealthNeverCountsAPendingRowAsSuccess(t *testing.T) {
	e, d, _ := newBackupHealthEngine(t)
	seedBackup(t, d, completedDurable("bk-1", "web", 26*time.Hour))
	seedBackup(t, d, cp.Backup{
		ID: "bk-2", App: "web", Environment: "prod", CreatedAt: healthNow.Add(-time.Minute),
		Status: cp.BackupPending, Destination: cp.BackupDestinationObjectStore, Provider: "b2",
	})

	h, err := e.BackupHealth(context.Background(), cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if h.LastDurableSuccess == nil || h.LastDurableSuccess.ID != "bk-1" {
		t.Fatalf("last durable success = %+v, want bk-1 — a pending row is not a success", h.LastDurableSuccess)
	}
	if h.Pending != 1 {
		t.Errorf("pending = %d, want 1", h.Pending)
	}
	if h.LastSuccess == nil || h.LastSuccess.ID != "bk-1" {
		t.Errorf("last success = %+v, want bk-1", h.LastSuccess)
	}
}

// TestBackupHealthReportsTheLastFailureBesideTheSuccess: a recent failure after an older success is
// a different situation from never having succeeded, and the closed reason travels so the operator
// is told which problem they have.
func TestBackupHealthReportsTheLastFailureBesideTheSuccess(t *testing.T) {
	e, d, _ := newBackupHealthEngine(t)
	seedBackup(t, d, completedDurable("bk-1", "web", 30*time.Hour))
	seedBackup(t, d, cp.Backup{
		ID: "bk-2", App: "web", Environment: "prod", CreatedAt: healthNow.Add(-90 * time.Minute),
		Status: cp.BackupFailed, Destination: cp.BackupDestinationObjectStore, Provider: "b2",
		FailureReason: cp.BackupReasonStoreRejected, FailureDetail: "the store answered and refused the write",
	})

	h, err := e.BackupHealth(context.Background(), cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if h.LastFailure == nil || h.LastFailure.ID != "bk-2" {
		t.Fatalf("last failure = %+v, want bk-2", h.LastFailure)
	}
	if h.LastFailure.Reason != cp.BackupReasonStoreRejected {
		t.Errorf("reason = %q, want %q", h.LastFailure.Reason, cp.BackupReasonStoreRejected)
	}
	if h.State != cp.BackupHealthDurable || h.LastDurableSuccess == nil {
		t.Errorf("a later failure must not erase an earlier durable success: %+v", h)
	}
	if !strings.Contains(h.Summary, "failed since then") {
		t.Errorf("summary = %q, want it to note the failure after the last durable backup", h.Summary)
	}
}

// TestBackupHealthProbesEachRegisteredDestination asserts the reachability half of ADR-0063 §7: each
// registered destination is probed at the moment of the call, and one that does not answer is
// reported as unreachable WITH a Burrow-authored reason rather than being omitted.
func TestBackupHealthProbesEachRegisteredDestination(t *testing.T) {
	ctx := context.Background()
	e, _, osf := newBackupHealthEngine(t)
	if _, err := e.AddProvider(ctx, s3Request()); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	h, err := e.BackupHealth(ctx, cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if len(h.Destinations) != 1 {
		t.Fatalf("destinations = %+v, want the one registered provider", h.Destinations)
	}
	if !h.Destinations[0].Reachable {
		t.Errorf("destination = %+v, want reachable", h.Destinations[0])
	}
	if h.Destinations[0].Endpoint != testEndpoint {
		t.Errorf("endpoint = %q, want %q", h.Destinations[0].Endpoint, testEndpoint)
	}

	// A store that will not answer is reported as unreachable, with what Burrow could not do — and
	// never with the vendor's own words or the credential it was signed with.
	osf.Store.ExistsErr = errors.New("dial tcp: i/o timeout for " + testKeyID)
	h, err = e.BackupHealth(ctx, cp.AddonPostgres, "", "")
	if err != nil {
		t.Fatalf("BackupHealth after the store stopped answering: %v", err)
	}
	if len(h.Destinations) != 1 || h.Destinations[0].Reachable {
		t.Fatalf("destinations = %+v, want one unreachable", h.Destinations)
	}
	if h.Destinations[0].Detail == "" {
		t.Error("an unreachable destination must say what was observed")
	}
	for _, secret := range []string{testKeyID, testSecret} {
		if strings.Contains(h.Destinations[0].Detail, secret) {
			t.Errorf("detail %q carries a credential value", h.Destinations[0].Detail)
		}
	}
}

// TestBackupHealthNarrowsToAnAppAndEnvironment asserts the scope arguments behave exactly as the
// backups listing's do: empty spans everything, a value restricts.
func TestBackupHealthNarrowsToAnAppAndEnvironment(t *testing.T) {
	ctx := context.Background()
	e, d, _ := newBackupHealthEngine(t)
	seedBackup(t, d, completedDurable("bk-web", "web", 2*time.Hour))
	api := completedDurable("bk-api", "api", 5*time.Hour)
	api.Environment = "staging"
	seedBackup(t, d, api)

	h, err := e.BackupHealth(ctx, cp.AddonPostgres, "web", "")
	if err != nil {
		t.Fatalf("BackupHealth(web): %v", err)
	}
	if h.LastDurableSuccess == nil || h.LastDurableSuccess.ID != "bk-web" {
		t.Fatalf("narrowed to web = %+v, want bk-web", h.LastDurableSuccess)
	}
	h, err = e.BackupHealth(ctx, cp.AddonPostgres, "", "staging")
	if err != nil {
		t.Fatalf("BackupHealth(staging): %v", err)
	}
	if h.LastDurableSuccess == nil || h.LastDurableSuccess.ID != "bk-api" {
		t.Fatalf("narrowed to staging = %+v, want bk-api", h.LastDurableSuccess)
	}
}

// TestBackupHealthRejectsAnAddonWithoutBackups keeps the surface honest about what it can answer.
func TestBackupHealthRejectsAnAddonWithoutBackups(t *testing.T) {
	e, _, _ := newBackupHealthEngine(t)
	if _, err := e.BackupHealth(context.Background(), cp.AddonCache, "", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("backup health for cache = %v, want ErrInvalid", err)
	}
	if _, err := e.BackupHealth(context.Background(), cp.AddonPostgres, "Bad_Name", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("backup health for a bad app name = %v, want ErrInvalid", err)
	}
}

// TestDurableIsBothStatusAndDestination pins the predicate ADR-0064 §5's refusal and ADR-0066 §5's
// age both stand on. If it ever loosened, `--delete-data` would destroy a volume on the strength of
// a dump that never left the cluster, and the health surface would report that dump as coverage —
// one change, two silent failures, which is why the definition lives in one place.
func TestDurableIsBothStatusAndDestination(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    cp.Backup
		want bool
	}{
		{"completed at the object store", cp.Backup{Status: cp.BackupCompleted, Destination: cp.BackupDestinationObjectStore}, true},
		{"completed in the cluster only", cp.Backup{Status: cp.BackupCompleted, Destination: cp.BackupDestinationCluster}, false},
		{"completed with no destination recorded", cp.Backup{Status: cp.BackupCompleted}, false},
		{"pending at the object store", cp.Backup{Status: cp.BackupPending, Destination: cp.BackupDestinationObjectStore}, false},
		{"failed at the object store", cp.Backup{Status: cp.BackupFailed, Destination: cp.BackupDestinationObjectStore}, false},
	} {
		if got := tc.b.Durable(); got != tc.want {
			t.Errorf("%s: Durable() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
