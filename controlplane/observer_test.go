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

// observerHarness bundles an engine wired to fakes with the observer under test, so a test can seed
// what the registry believes, arrange what the cluster actually has, drive one sweep, and read the
// ledger back.
type observerHarness struct {
	observer *cp.Observer
	db       *fake.Database
	k8s      *fake.Kubernetes
	clock    *fake.Clock
}

func newObserverHarness(t *testing.T, cfg cp.ObserverConfig) *observerHarness {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	c := fake.NewClock(time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC))
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: c, IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &observerHarness{observer: e.NewObserver(cfg), db: d, k8s: k, clock: c}
}

// seedDeployedApp records a deployed release, which is what makes an app one the registry says
// Burrow owns a workload for (ADR-0074 §6).
func seedDeployedApp(t *testing.T, d *fake.Database, id, app string) {
	t.Helper()
	if err := d.SaveRelease(context.Background(), cp.Release{
		ID: id, App: app, Image: "ghcr.io/u/" + app + ":1", Environment: cp.DefaultEnvironment, Status: cp.ReleaseDeployed,
	}); err != nil {
		t.Fatalf("SaveRelease: %v", err)
	}
}

// activeFailures returns the active ledger rows, oldest first.
func activeFailures(t *testing.T, d *fake.Database) []cp.Failure {
	t.Helper()
	rows, err := d.Failures(context.Background(), cp.FailureFilter{})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	return rows
}

// allFailures returns every ledger row including resolved ones.
func allFailures(t *testing.T, d *fake.Database) []cp.Failure {
	t.Helper()
	rows, err := d.Failures(context.Background(), cp.FailureFilter{IncludeResolved: true})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	return rows
}

// findFailure returns the row for one (object kind, name, reason), or fails the test.
func findFailure(t *testing.T, rows []cp.Failure, kind cp.FailureKind, name, reason string) cp.Failure {
	t.Helper()
	for _, f := range rows {
		if f.Object.Kind == kind && f.Object.Name == name && f.Reason == reason {
			return f
		}
	}
	t.Fatalf("no ledger row for %s %q with reason %q; rows: %+v", kind, name, reason, rows)
	return cp.Failure{}
}

// TestObserverRecordsAWorkloadIssue: a crash-looping app's blocking reason lands in the ledger under
// exactly the vocabulary the live status surface uses (ADR-0074 §2/§4), and a second sweep of the
// same standing failure extends the one row rather than adding a second.
func TestObserverRecordsAWorkloadIssue(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	seedDeployedApp(t, h.db, "web-1", "web")
	if err := h.k8s.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "ghcr.io/u/web:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	h.k8s.SetIssue("web", cp.IssueEvidence{
		Reason: cp.ReasonCrashLoopBackOff, Container: "web", ExitCode: 1,
		LogTail: "panic: DATABASE_URL=postgres://user:hunter2@db/app is unreachable",
	})

	start := h.clock.Now()
	h.observer.ObserveOnceForTest(ctx)

	rows := activeFailures(t, h.db)
	if len(rows) != 1 {
		t.Fatalf("expected exactly one ledger row, got %d: %+v", len(rows), rows)
	}
	f := rows[0]
	if f.Object != (cp.ObjectRef{Kind: cp.FailureApp, Name: "web", Environment: cp.DefaultEnvironment}) {
		t.Errorf("object = %+v, want the app web in prod", f.Object)
	}
	if f.Reason != cp.ReasonCrashLoopBackOff {
		t.Errorf("reason = %q, want %q", f.Reason, cp.ReasonCrashLoopBackOff)
	}
	if !f.FirstSeen.Equal(start) || !f.LastSeen.Equal(start) || f.Occurrences != 1 || !f.Active() {
		t.Errorf("first sighting = %+v, want first_seen == last_seen == %s, one occurrence, active", f, start)
	}

	// The crash-loop detail is the line Burrow wrote; the application's own log output, which the
	// live surface appends after a newline, does not enter the durable record (ADR-0074 §9).
	if got := f.Detail; got == "" {
		t.Errorf("detail is empty, want the exit code Burrow reported")
	}
	for _, secret := range []string{"hunter2", "DATABASE_URL", "\n"} {
		if strings.Contains(f.Detail, secret) {
			t.Errorf("ledger detail %q carries application output (%q); no secret value may enter a ledger row", f.Detail, secret)
		}
	}

	// A second sweep of the same standing failure is one row that says two, not two rows.
	h.clock.Advance(time.Minute)
	second := h.clock.Now()
	h.observer.ObserveOnceForTest(ctx)
	rows = activeFailures(t, h.db)
	if len(rows) != 1 {
		t.Fatalf("a standing failure produced %d rows, want one that counts: %+v", len(rows), rows)
	}
	f = rows[0]
	if !f.FirstSeen.Equal(start) {
		t.Errorf("first_seen moved to %s; the answer to \"when did it start\" must not change", f.FirstSeen)
	}
	if !f.LastSeen.Equal(second) || f.Occurrences != 2 {
		t.Errorf("second sighting: last_seen = %s, occurrences = %d; want %s and 2", f.LastSeen, f.Occurrences, second)
	}
}

// TestObserverKeepsConcurrentReasonsApart: one object with two blocking reasons at once is two rows
// with independent lifetimes, and one clearing does not close the other (ADR-0074 §5). It drives the
// ledger directly, because a single WorkloadStatus can only carry the one reason the live surface
// ranked highest — the concurrency this table exists to hold arrives across sweeps and across kinds.
func TestObserverKeepsConcurrentReasonsApart(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	ref := cp.ObjectRef{Kind: cp.FailureApp, Name: "web", Environment: cp.DefaultEnvironment}
	at := h.clock.Now()
	for _, reason := range []string{cp.ReasonOOMKilled, cp.ReasonUnschedulable} {
		if err := h.db.RecordFailure(ctx, cp.FailureObservation{Object: ref, Reason: reason, At: at}); err != nil {
			t.Fatalf("RecordFailure(%s): %v", reason, err)
		}
	}
	if rows := activeFailures(t, h.db); len(rows) != 2 {
		t.Fatalf("two reasons on one object produced %d rows, want 2: %+v", len(rows), rows)
	}

	// Only the OOM kill is still happening: the unschedulable row resolves and the other does not.
	later := at.Add(time.Minute)
	keep := []cp.FailureKey{{Object: ref, Reason: cp.ReasonOOMKilled}}
	if err := h.db.ResolveFailures(ctx, later, keep, nil); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}
	if got := findFailure(t, allFailures(t, h.db), cp.FailureApp, "web", cp.ReasonOOMKilled); !got.Active() {
		t.Errorf("the OOM row resolved though it was still observed")
	}
	unsched := findFailure(t, allFailures(t, h.db), cp.FailureApp, "web", cp.ReasonUnschedulable)
	if unsched.Active() || !unsched.ResolvedAt.Equal(later) {
		t.Errorf("the unschedulable row = %+v, want resolved at %s", unsched, later)
	}

	// A recurrence opens a NEW row rather than reviving the resolved one, so two episodes read as
	// two rather than as one long outage.
	if err := h.db.RecordFailure(ctx, cp.FailureObservation{Object: ref, Reason: cp.ReasonUnschedulable, At: later.Add(time.Hour)}); err != nil {
		t.Fatalf("RecordFailure after resolution: %v", err)
	}
	var episodes int
	for _, f := range allFailures(t, h.db) {
		if f.Reason == cp.ReasonUnschedulable {
			episodes++
		}
	}
	if episodes != 2 {
		t.Errorf("a failure that recurred after resolving reads as %d episodes, want 2", episodes)
	}
}

// TestObserverResolvesWhatRecovered: an app whose blocking condition clears has its row resolved by
// the next sweep — the answer to "did it recover on its own" (ADR-0074 §4).
func TestObserverResolvesWhatRecovered(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	seedDeployedApp(t, h.db, "web-1", "web")
	if err := h.k8s.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "ghcr.io/u/web:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: "0/3 nodes are available: insufficient cpu"})
	h.observer.ObserveOnceForTest(ctx)
	if rows := activeFailures(t, h.db); len(rows) != 1 {
		t.Fatalf("expected one active row after the first sweep, got %d", len(rows))
	}

	h.k8s.SetIssue("web", cp.IssueEvidence{})
	h.clock.Advance(time.Minute)
	recovered := h.clock.Now()
	h.observer.ObserveOnceForTest(ctx)

	if rows := activeFailures(t, h.db); len(rows) != 0 {
		t.Fatalf("the failure cleared but %d rows are still active: %+v", len(rows), rows)
	}
	f := findFailure(t, allFailures(t, h.db), cp.FailureApp, "web", cp.ReasonUnschedulable)
	if f.ResolvedAt == nil || !f.ResolvedAt.Equal(recovered) {
		t.Errorf("resolved_at = %v, want %s", f.ResolvedAt, recovered)
	}
}

// TestObserverRecordsIntentVersusCluster covers all four §6 discrepancies in one sweep: each is an
// ABSENCE, invisible from the cluster side, and each is recorded as a failure in its own right.
func TestObserverRecordsIntentVersusCluster(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})

	// A registered app whose Deployment is not there.
	seedDeployedApp(t, h.db, "ghost-1", "ghost")

	// An add-on registered as installed with nothing running. The fake reports readiness from its
	// add-on map, so a registry row with no cluster add-on is exactly this case.
	if err := h.db.SaveAddon(ctx, cp.AddonInfo{Name: "burrow-postgres", Type: cp.AddonPostgres, Mode: "installed", Environment: cp.DefaultEnvironment}); err != nil {
		t.Fatalf("SaveAddon: %v", err)
	}

	// A backup left pending by a burrowd that restarted mid-backup: the row is old enough that a
	// missing Job means gone, not "not created yet".
	if err := h.db.RecordBackup(ctx, cp.Backup{
		ID: "bk-1", App: "web", Environment: cp.DefaultEnvironment, Status: cp.BackupPending,
		CreatedAt: h.clock.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	// A recorded exposure whose Ingress is gone.
	if err := h.db.RecordExposure(ctx, cp.Exposure{App: "web", Environment: cp.DefaultEnvironment, Host: "web.example.com", Port: 8080, TLS: true}); err != nil {
		t.Fatalf("RecordExposure: %v", err)
	}

	h.observer.ObserveOnceForTest(ctx)

	rows := activeFailures(t, h.db)
	findFailure(t, rows, cp.FailureApp, "ghost", cp.ReasonWorkloadMissing)
	findFailure(t, rows, cp.FailureAddon, "burrow-postgres", cp.ReasonAddonNotRunning)
	findFailure(t, rows, cp.FailureBackup, "bk-1", cp.ReasonBackupJobMissing)
	findFailure(t, rows, cp.FailureExposure, "web", cp.ReasonIngressMissing)
	if len(rows) != 4 {
		t.Errorf("expected exactly the four §6 discrepancies, got %d: %+v", len(rows), rows)
	}
}

// TestObserverIgnoresARunningBackupJob: a pending backup whose Job is still there is a backup in
// progress, not an orphan, and a pending row inside the grace period is not judged at all.
func TestObserverIgnoresARunningBackupJob(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	old := cp.Backup{ID: "bk-old", App: "web", Environment: cp.DefaultEnvironment, Status: cp.BackupPending, CreatedAt: h.clock.Now().Add(-2 * time.Hour)}
	fresh := cp.Backup{ID: "bk-new", App: "web", Environment: cp.DefaultEnvironment, Status: cp.BackupPending, CreatedAt: h.clock.Now()}
	for _, b := range []cp.Backup{old, fresh} {
		if err := h.db.RecordBackup(ctx, b); err != nil {
			t.Fatalf("RecordBackup: %v", err)
		}
	}
	h.k8s.SetBackupJob("bk-old", true)

	h.observer.ObserveOnceForTest(ctx)

	if rows := activeFailures(t, h.db); len(rows) != 0 {
		t.Fatalf("a running backup and one inside the grace period produced %d rows: %+v", len(rows), rows)
	}
}

// TestObserverFlagsAnUnissuedCertificate: a TLS exposure whose Ingress is present but whose
// certificate never issued is its own reason, because the fix is a different one (ADR-0074 §6).
func TestObserverFlagsAnUnissuedCertificate(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	if err := h.k8s.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080, TLS: true, Issuer: "letsencrypt"}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if err := h.db.RecordExposure(ctx, cp.Exposure{App: "web", Environment: cp.DefaultEnvironment, Host: "web.example.com", Port: 8080, TLS: true}); err != nil {
		t.Fatalf("RecordExposure: %v", err)
	}

	h.observer.ObserveOnceForTest(ctx)
	findFailure(t, activeFailures(t, h.db), cp.FailureExposure, "web", cp.ReasonCertificateNotIssued)

	// Once cert-manager issues it, the row resolves like any other recovery.
	h.k8s.SetCertReady("web", true)
	h.clock.Advance(time.Minute)
	h.observer.ObserveOnceForTest(ctx)
	if rows := activeFailures(t, h.db); len(rows) != 0 {
		t.Errorf("the certificate issued but %d rows are still active: %+v", len(rows), rows)
	}
}

// TestObserverNeverMutatesTheCluster is the standing guard on ADR-0074 §9: the observer sweeps a
// cluster full of broken things and changes none of them. It asserts on the fake's recorded state
// rather than on a comment, because the tempting next commit is the one that restarts the crash
// looper.
func TestObserverNeverMutatesTheCluster(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	seedDeployedApp(t, h.db, "web-1", "web")
	if err := h.k8s.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "ghcr.io/u/web:1", Replicas: 3}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonOOMKilled, Container: "web", Detail: "256Mi"})
	h.k8s.SetReady("web", 0)
	before, ok := h.k8s.Spec("web")
	if !ok {
		t.Fatalf("no workload spec for web")
	}
	restartedBefore, rolledBefore := h.k8s.RestartedAt("web")

	for i := 0; i < 3; i++ {
		h.observer.ObserveOnceForTest(ctx)
		h.clock.Advance(time.Minute)
	}

	after, ok := h.k8s.Spec("web")
	if !ok {
		t.Fatalf("the observer removed the workload")
	}
	if after.Replicas != before.Replicas || after.Image != before.Image {
		t.Errorf("the observer changed the workload: %+v -> %+v", before, after)
	}
	if restartedAfter, rolledAfter := h.k8s.RestartedAt("web"); rolledAfter != rolledBefore || restartedAfter != restartedBefore {
		t.Errorf("the observer restarted the workload; noticing a failure is a read, restarting it is a mutation with nobody present")
	}
	if got := findFailure(t, activeFailures(t, h.db), cp.FailureApp, "web", cp.ReasonOOMKilled); got.Occurrences != 3 {
		t.Errorf("occurrences = %d after three sweeps, want 3", got.Occurrences)
	}
}

// TestObserverRecordsItsOwnCoverage: the sweeps of one process extend one window, and a restart
// begins a NEW one — so the gap while burrowd was down stays visible instead of reading as an hour
// in which nothing broke.
func TestObserverRecordsItsOwnCoverage(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	start := h.clock.Now()
	h.observer.ObserveOnceForTest(ctx)
	h.clock.Advance(time.Minute)
	h.observer.ObserveOnceForTest(ctx)

	windows, err := h.db.ObservationWindows(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ObservationWindows: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("two sweeps of one process produced %d windows, want 1: %+v", len(windows), windows)
	}
	if !windows[0].StartedAt.Equal(start) || windows[0].Sweeps != 2 || !windows[0].Complete() {
		t.Errorf("window = %+v, want two clean sweeps from %s", windows[0], start)
	}

	// burrowd is down for an hour, then comes back: a NEW observer, a new window, and an hour
	// between the first window's end and the second's start that is legible as a gap.
	h.clock.Advance(time.Hour)
	restarted := h.clock.Now()
	h.observer = newObserverOn(t, h)
	h.observer.ObserveOnceForTest(ctx)

	windows, err = h.db.ObservationWindows(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ObservationWindows: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("a restart produced %d windows, want 2: %+v", len(windows), windows)
	}
	if gap := windows[1].StartedAt.Sub(windows[0].Until); gap != time.Hour {
		t.Errorf("the gap between windows is %s, want the hour burrowd was down", gap)
	}
	if !windows[1].StartedAt.Equal(restarted) {
		t.Errorf("the new window starts at %s, want %s", windows[1].StartedAt, restarted)
	}
}

// TestObserverReportsPartialCoverage: a sweep that cannot read one object's cluster state counts as
// degraded and leaves that object's rows alone — a failure Burrow could not check is not a failure
// that recovered.
func TestObserverReportsPartialCoverage(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	if err := h.db.SaveAddon(ctx, cp.AddonInfo{Name: "burrow-postgres", Type: cp.AddonPostgres, Mode: "installed", Environment: cp.DefaultEnvironment}); err != nil {
		t.Fatalf("SaveAddon: %v", err)
	}
	h.observer.ObserveOnceForTest(ctx)
	findFailure(t, activeFailures(t, h.db), cp.FailureAddon, "burrow-postgres", cp.ReasonAddonNotRunning)

	// The next sweep cannot read the add-on at all.
	h.k8s.SetError(fake.OpAddonReady, errors.New("the API server is unreachable"))
	h.clock.Advance(time.Minute)
	h.observer.ObserveOnceForTest(ctx)

	if rows := activeFailures(t, h.db); len(rows) != 1 {
		t.Errorf("an unreadable object's row was closed: %d active rows, want 1", len(rows))
	}
	windows, err := h.db.ObservationWindows(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ObservationWindows: %v", err)
	}
	if len(windows) != 1 || windows[0].DegradedSweeps != 1 || windows[0].Detail == "" {
		t.Errorf("window = %+v, want one degraded sweep with a note", windows)
	}
}

// TestObserverDoesNotClaimCoverageItDoesNotHave: when the control plane's own database cannot be
// read, the sweep abandons the pass rather than resolving rows on the strength of an intent list it
// never saw — and records no coverage for it, because nothing was observed.
func TestObserverDoesNotClaimCoverageItDoesNotHave(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	seedDeployedApp(t, h.db, "web-1", "web")
	if err := h.k8s.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "ghcr.io/u/web:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: "insufficient cpu"})
	h.observer.ObserveOnceForTest(ctx)

	h.db.SetError(fake.OpManagedApps, errors.New("the database is unreachable"))
	h.clock.Advance(time.Minute)
	h.observer.ObserveOnceForTest(ctx)

	if rows := activeFailures(t, h.db); len(rows) != 1 {
		t.Errorf("a sweep that could not enumerate resolved rows anyway: %d active, want 1", len(rows))
	}
	windows, err := h.db.ObservationWindows(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ObservationWindows: %v", err)
	}
	if len(windows) != 1 || windows[0].Sweeps != 1 {
		t.Fatalf("window = %+v, want the one sweep that actually observed something", windows)
	}
	if !windows[0].Until.Equal(windows[0].StartedAt) {
		t.Errorf("coverage advanced to %s for a sweep that observed nothing", windows[0].Until)
	}
}

// TestObserverPrunesResolvedFailures: retention is bounded and configurable, it removes resolved
// history past the window, and it never removes a failure that is still active (ADR-0074 §4).
func TestObserverPrunesResolvedFailures(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{Retention: time.Hour, PruneInterval: time.Nanosecond})

	// A failure that broke and recovered two hours ago — history, past a one-hour window.
	healed := cp.ObjectRef{Kind: cp.FailureApp, Name: "web", Environment: cp.DefaultEnvironment}
	old := h.clock.Now().Add(-2 * time.Hour)
	if err := h.db.RecordFailure(ctx, cp.FailureObservation{Object: healed, Reason: cp.ReasonOOMKilled, At: old}); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if err := h.db.ResolveFailures(ctx, old.Add(time.Minute), nil, nil); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}

	// And an app the registry still owns whose workload is missing — still broken, so not history,
	// however long it goes on.
	seedDeployedApp(t, h.db, "ghost-1", "ghost")

	h.observer.ObserveOnceForTest(ctx)

	rows := allFailures(t, h.db)
	if len(rows) != 1 {
		t.Fatalf("after pruning, %d rows remain, want the one still-active failure: %+v", len(rows), rows)
	}
	if rows[0].Object.Name != "ghost" || !rows[0].Active() {
		t.Errorf("the surviving row is %+v, want the active WorkloadMissing on ghost", rows[0])
	}
}

// TestObserverRunStopsOnCancellation: Run sweeps before it waits, and returns promptly when the
// context is cancelled — the loop shape burrowd relies on for a clean shutdown.
func TestObserverRunStopsOnCancellation(t *testing.T) {
	h := newObserverHarness(t, cp.ObserverConfig{
		Interval: time.Second,
		// After never fires, so the loop blocks after the first sweep and cancellation is the only
		// way out — proving the sweep runs before any wait.
		After: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	})
	seedDeployedApp(t, h.db, "ghost-1", "ghost")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.observer.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for {
		if rows, err := h.db.Failures(context.Background(), cp.FailureFilter{}); err == nil && len(rows) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the first sweep did not record the missing workload")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// newObserverOn builds a second observer over the same fakes, modelling a burrowd restart: the new
// process shares the database and the cluster and knows nothing of the previous run's window.
func newObserverOn(t *testing.T, h *observerHarness) *cp.Observer {
	t.Helper()
	e, err := cp.New(cp.Deps{
		Kubernetes: h.k8s, Database: h.db, Clock: h.clock, IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e.NewObserver(cp.ObserverConfig{})
}
