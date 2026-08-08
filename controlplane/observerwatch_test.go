// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The watch half of the observer (ADR-0079). Every test here drives the same path production does:
// the fake cluster delivers an event because something changed in it, the observer latches the
// transition, and the injected clock decides when — if ever — a row appears. Nothing waits on wall
// time, which is the cost ADR-0079's consequences say must not be given up.

// watchedApp seeds a deployed app, starts the observer's watches over the namespace it lives in, and
// returns the moment the cluster is quiet and healthy. The first pass is what establishes the watch,
// exactly as burrowd's first pass does.
func watchedApp(t *testing.T, h *observerHarness, app string) {
	t.Helper()
	ctx := context.Background()
	seedDeployedApp(t, h.db, app+"-1", app)
	if err := h.k8s.ApplyWorkload(ctx, cp.WorkloadSpec{App: app, Image: "ghcr.io/u/" + app + ":1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	h.observer.ObserveOnceForTest(ctx)
}

// TestObserverRecordsOnAWatchEventNotOnATheTimer: a crash loop that starts and is recorded between
// two periodic passes. The sweep this replaces could not see it at all until its next pass, which is
// the resolution ADR-0079 exists to recover.
func TestObserverRecordsOnAWatchEventNotOnATheTimer(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	watchedApp(t, h, "web")
	if rows := activeFailures(t, h.db); len(rows) != 0 {
		t.Fatalf("a healthy app produced rows: %+v", rows)
	}

	// Five seconds after the pass — a twelfth of the cadence, and nowhere near the next one.
	h.clock.Advance(5 * time.Second)
	broke := h.clock.Now()
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonCrashLoopBackOff, Container: "web", ExitCode: 137})
	h.observer.DrainForTest(ctx)

	f := findFailure(t, activeFailures(t, h.db), cp.FailureApp, "web", cp.ReasonCrashLoopBackOff)
	if !f.FirstSeen.Equal(broke) {
		t.Errorf("first_seen = %s, want %s: the row is written when the cluster reported it, not at the next pass", f.FirstSeen, broke)
	}
	if f.Occurrences != 1 {
		t.Errorf("occurrences = %d, want 1", f.Occurrences)
	}
}

// TestObserverRecordsAZeroDwellReasonImmediately: the reasons ADR-0079 §3 gives no dwell are the
// ones already produced by waiting, and they are recorded with no added latency at all. Asserting it
// per reason is the guard on the table itself — a uniform dwell would fail here on every row.
func TestObserverRecordsAZeroDwellReasonImmediately(t *testing.T) {
	for _, tc := range []struct {
		reason string
		why    string
	}{
		{cp.ReasonOOMKilled, "the kernel already killed the process"},
		{cp.ReasonCrashLoopBackOff, "the backoff is itself the dwell"},
		{cp.ReasonProgressDeadlineExceeded, "a deadline is a dwell that already elapsed"},
		{cp.ReasonCreateContainerConfigError, "a key that is missing now will not appear by waiting"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			ctx := context.Background()
			h := newObserverHarness(t, cp.ObserverConfig{})
			watchedApp(t, h, "web")

			at := h.clock.Now()
			h.k8s.SetIssue("web", cp.IssueEvidence{Reason: tc.reason, Container: "web"})
			h.observer.DrainForTest(ctx)

			f := findFailure(t, activeFailures(t, h.db), cp.FailureApp, "web", tc.reason)
			if !f.FirstSeen.Equal(at) {
				t.Errorf("%s was delayed to %s, want %s — %s", tc.reason, f.FirstSeen, at, tc.why)
			}
		})
	}
}

// TestObserverSwallowsAFlapInsideItsDwell: an app that goes unschedulable and is placed again inside
// the grace produces NO rows, however many times it does it. This is the noise ADR-0079 §2 exists to
// remove, and the reason a watch without a latch would be a faster way to be wrong.
func TestObserverSwallowsAFlapInsideItsDwell(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	watchedApp(t, h, "web")
	grace := defaultDwell(t, cp.LimitUnschedulableGrace)

	for i := 0; i < 20; i++ {
		h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: "0/3 nodes are available"})
		h.observer.DrainForTest(ctx)
		h.clock.Advance(grace / 4)
		h.k8s.SetIssue("web", cp.IssueEvidence{})
		h.observer.DrainForTest(ctx)
		h.clock.Advance(grace / 4)
	}

	if rows := allFailures(t, h.db); len(rows) != 0 {
		t.Fatalf("a flap inside its dwell produced %d ledger rows, want none: %+v", len(rows), rows)
	}
	// And it left nothing behind either: a transition that never opened is deleted when it clears,
	// so twenty flaps are not twenty entries waiting for a dwell that will never elapse.
	if n := h.observer.LatchedForTest(); n != 0 {
		t.Errorf("the latch is holding %d transitions after twenty flaps, want none", n)
	}
}

// TestObserverDoesNotCloseAndReopenInsideTheClosingDwell: latching only the opening edge would let a
// flapping object open and close one row repeatedly, which is the same noise through a different
// door — and it would make ADR-0074 §4's occurrence count count flaps rather than failures.
func TestObserverDoesNotCloseAndReopenInsideTheClosingDwell(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	watchedApp(t, h, "web")
	grace := defaultDwell(t, cp.LimitUnschedulableGrace)

	began := h.clock.Now()
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: "0/3 nodes are available"})
	h.observer.DrainForTest(ctx)
	h.clock.Advance(grace)
	h.observer.SettleForTest(ctx)
	if rows := activeFailures(t, h.db); len(rows) != 1 {
		t.Fatalf("expected the one open row to latch against, got %+v", rows)
	}

	// It clears and comes back, five times, never staying clear for the dwell.
	for i := 0; i < 5; i++ {
		h.k8s.SetIssue("web", cp.IssueEvidence{})
		h.observer.DrainForTest(ctx)
		h.clock.Advance(grace / 2)
		h.observer.SettleForTest(ctx)
		h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: "0/3 nodes are available"})
		h.observer.DrainForTest(ctx)
		h.clock.Advance(grace / 2)
		h.observer.SettleForTest(ctx)
	}

	rows := allFailures(t, h.db)
	if len(rows) != 1 {
		t.Fatalf("a flapping condition produced %d rows, want the one episode it is: %+v", len(rows), rows)
	}
	if !rows[0].Active() {
		t.Errorf("the row resolved during a clear shorter than its dwell: %+v", rows[0])
	}
	if !rows[0].FirstSeen.Equal(began) {
		t.Errorf("first_seen moved to %s; \"when did it start\" must survive the flapping", rows[0].FirstSeen)
	}
}

// TestObserverWakesForTheDwellRatherThanForThePass: the run loop's sleep has to shrink to the
// earliest dwell deadline. Waking only on the cadence would round every dwell up to it, which is
// exactly the imprecision ADR-0079 was written about — a thirty-second grace under a sixty-second
// pass would be a sixty-second grace.
func TestObserverWakesForTheDwellRatherThanForThePass(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{Interval: time.Minute})
	watchedApp(t, h, "web")
	grace := defaultDwell(t, cp.LimitUnschedulableGrace)
	if grace >= time.Minute {
		t.Fatalf("the default grace (%s) is not shorter than the pass; this test asserts nothing", grace)
	}

	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: "0/3 nodes are available"})
	h.observer.DrainForTest(ctx)

	if got := h.observer.NextWakeForTest(); got != grace {
		t.Errorf("the loop would sleep %s with a %s dwell pending, want %s", got, grace, grace)
	}
}

// TestObserverStopsCoveringWhenAWatchDrops: a dropped watch ends coverage where it dropped, a
// completed re-list resumes it, and the space between the two is a literal gap in what `burrow
// failures` reports (ADR-0079 §4).
//
// A re-list reports current state, not what happened while the watch was away, so the gap has to
// show rather than the observer pretending continuity across it.
func TestObserverStopsCoveringWhenAWatchDrops(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	watchedApp(t, h, "web")
	h.clock.Advance(time.Minute)
	h.observer.ObserveOnceForTest(ctx)

	h.clock.Advance(10 * time.Second)
	dropped := h.clock.Now()
	h.k8s.DropWorkloadWatch("the connection to the API server was reset")
	h.observer.DrainForTest(ctx)

	// Passes keep running; they just stop claiming to have seen anything.
	for i := 0; i < 3; i++ {
		h.clock.Advance(time.Minute)
		h.observer.ObserveOnceForTest(ctx)
	}

	h.clock.Advance(time.Minute)
	resumed := h.clock.Now()
	h.k8s.SyncWorkloadWatch()
	h.observer.DrainForTest(ctx)
	h.observer.ObserveOnceForTest(ctx)

	report, err := h.engine.Failures(ctx, cp.FailureQuery{})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	// The leading gap is the time before burrowd started, which every answer carries; the one this
	// test is about is the last.
	if len(report.Coverage.Gaps) == 0 {
		t.Fatalf("a dropped watch produced no coverage gap: %+v", report.Coverage)
	}
	gap := report.Coverage.Gaps[len(report.Coverage.Gaps)-1]
	if !gap.From.Equal(dropped) {
		t.Errorf("the gap starts at %s, want the instant the watch dropped (%s)", gap.From, dropped)
	}
	if !gap.To.Equal(resumed) {
		t.Errorf("the gap ends at %s, want the instant the re-list completed (%s)", gap.To, resumed)
	}
	if report.Coverage.Complete() {
		t.Errorf("coverage reports itself complete across a stretch nothing was watching: %+v", report.Coverage)
	}
}

// TestObserverKeepsRowsOpenWhileItsWatchIsDown: a failure Burrow could not check is not a failure
// that recovered. The rows stay exactly as they were, their last_seen stops advancing because
// nothing is looking, and the coverage record is what tells the reader why.
func TestObserverKeepsRowsOpenWhileItsWatchIsDown(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	watchedApp(t, h, "web")
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonOOMKilled, Container: "web", Detail: "128Mi"})
	h.observer.DrainForTest(ctx)
	opened := findFailure(t, activeFailures(t, h.db), cp.FailureApp, "web", cp.ReasonOOMKilled)

	h.k8s.DropWorkloadWatch("the connection was reset")
	h.observer.DrainForTest(ctx)
	for i := 0; i < 3; i++ {
		h.clock.Advance(time.Minute)
		h.observer.ObserveOnceForTest(ctx)
	}

	f := findFailure(t, activeFailures(t, h.db), cp.FailureApp, "web", cp.ReasonOOMKilled)
	if !f.Active() {
		t.Errorf("a row was resolved while nothing was watching the object it is about")
	}
	if !f.LastSeen.Equal(opened.LastSeen) || f.Occurrences != opened.Occurrences {
		t.Errorf("row = %+v, want it frozen at %+v: an unwatched failure is not an observed one", f, opened)
	}
}

// TestObserverReaffirmsAStandingFailure: a condition nothing reports again — a pod the scheduler has
// refused once and will not mention until something changes — must still read as ongoing. The
// periodic pass is what advances its last_seen, and without it "is it still happening" would answer
// no for a failure that has never stopped.
func TestObserverReaffirmsAStandingFailure(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	watchedApp(t, h, "web")
	began := h.clock.Now()
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonCrashLoopBackOff, Container: "web", ExitCode: 1})
	h.observer.DrainForTest(ctx)

	// Two more passes and not one further event from the cluster.
	for i := 0; i < 2; i++ {
		h.clock.Advance(time.Minute)
		h.observer.ObserveOnceForTest(ctx)
	}
	last := h.clock.Now()

	f := findFailure(t, activeFailures(t, h.db), cp.FailureApp, "web", cp.ReasonCrashLoopBackOff)
	if !f.FirstSeen.Equal(began) {
		t.Errorf("first_seen = %s, want %s", f.FirstSeen, began)
	}
	if !f.LastSeen.Equal(last) {
		t.Errorf("last_seen = %s, want %s: a standing failure that emits no events must still read as ongoing", f.LastSeen, last)
	}
	if f.Occurrences != 3 {
		t.Errorf("occurrences = %d, want 3 — the sighting that opened it and one per pass since", f.Occurrences)
	}
}

// TestObserverKeepsATransitionAWriteLost: a ledger write that fails does not lose the transition.
// It is held and written when the database comes back, with the instant the failure actually
// started — and until then it is PACED, because a deadline that stays in the past would turn the run
// loop into a tight retry against the thing that is already down.
func TestObserverKeepsATransitionAWriteLost(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	watchedApp(t, h, "web")

	h.db.SetError(fake.OpRecordFailure, errors.New("the database is unreachable"))
	broke := h.clock.Now()
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonOOMKilled, Container: "web", Detail: "128Mi"})
	h.observer.DrainForTest(ctx)
	if rows := allFailures(t, h.db); len(rows) != 0 {
		t.Fatalf("a row appeared despite the write failing: %+v", rows)
	}
	if wake := h.observer.NextWakeForTest(); wake <= 0 {
		t.Errorf("the loop would not sleep after a failed write (%s); that is a tight retry against an unavailable database", wake)
	}

	h.db.SetError(fake.OpRecordFailure, nil)
	h.clock.Advance(time.Minute)
	h.observer.SettleForTest(ctx)

	f := findFailure(t, activeFailures(t, h.db), cp.FailureApp, "web", cp.ReasonOOMKilled)
	if !f.FirstSeen.Equal(broke) {
		t.Errorf("first_seen = %s, want %s: the transition is written with when it started, not when the write finally succeeded", f.FirstSeen, broke)
	}
}

// TestObserverStateIsBoundedByTheManagedSet: the latch is bounded by what Burrow manages, not by how
// much has ever gone wrong. An app that leaves the registry takes its transitions with it, and its
// row is resolved rather than held open by state nobody will ever clear.
func TestObserverStateIsBoundedByTheManagedSet(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	watchedApp(t, h, "web")
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonOOMKilled, Container: "web", Detail: "128Mi"})
	h.observer.DrainForTest(ctx)
	if n := h.observer.LatchedForTest(); n != 1 {
		t.Fatalf("the latch holds %d transitions, want the one open failure", n)
	}

	// The app is deleted: no release history, no workload, nothing for the registry to record.
	if err := h.db.DeleteReleases(ctx, "web"); err != nil {
		t.Fatalf("DeleteReleases: %v", err)
	}
	if err := h.k8s.DeleteWorkload(ctx, "web"); err != nil {
		t.Fatalf("DeleteWorkload: %v", err)
	}
	h.clock.Advance(time.Minute)
	h.observer.ObserveOnceForTest(ctx)

	if n := h.observer.LatchedForTest(); n != 0 {
		t.Errorf("the latch still holds %d transitions for an app the registry no longer records", n)
	}
	if rows := activeFailures(t, h.db); len(rows) != 0 {
		t.Errorf("an unmanaged object's rows are still active: %+v", rows)
	}
}

// TestObserverSurvivesAWatchItCannotEstablish: a namespace that cannot be watched degrades the pass
// and stops coverage rather than being quietly skipped, and the next pass retries it. A namespace
// nobody is watching that is reported as covered is the one failure this surface may not have.
func TestObserverSurvivesAWatchItCannotEstablish(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	seedDeployedApp(t, h.db, "web-1", "web")
	if err := h.k8s.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "ghcr.io/u/web:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	h.k8s.SetError(fake.OpWatchWorkloads, errors.New("the API server is unreachable"))
	h.observer.ObserveOnceForTest(ctx)

	windows, err := h.db.ObservationWindows(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ObservationWindows: %v", err)
	}
	if len(windows) != 0 {
		t.Fatalf("coverage was claimed for a namespace nobody is watching: %+v", windows)
	}

	h.k8s.SetError(fake.OpWatchWorkloads, nil)
	h.clock.Advance(time.Minute)
	resumed := h.clock.Now()
	h.observer.ObserveOnceForTest(ctx)

	windows, err = h.db.ObservationWindows(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ObservationWindows: %v", err)
	}
	if len(windows) != 1 || !windows[0].StartedAt.Equal(resumed) {
		t.Fatalf("windows = %+v, want one opening at %s once the watch was established", windows, resumed)
	}
}

// TestObserverDoesNotRecordPodConditionsFromThePeriodicPass: the periodic pass keeps ADR-0074 §6's
// absences and nothing else. If it also recorded what a pod reports, a flap the latch had
// deliberately swallowed would get a row anyway from the other mechanism — two writers on one table
// under different rules, which is worse than either.
func TestObserverDoesNotRecordPodConditionsFromThePeriodicPass(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	seedDeployedApp(t, h.db, "web-1", "web")
	if err := h.k8s.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "ghcr.io/u/web:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	// Unschedulable before anything is watching, so the pass sees it in its listing and the watch
	// reports it on the same tick — and the dwell still applies.
	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: "0/3 nodes are available"})

	h.observer.ObserveOnceForTest(ctx)
	if rows := allFailures(t, h.db); len(rows) != 0 {
		t.Fatalf("the periodic pass wrote a pod condition straight to the ledger, bypassing the dwell: %+v", rows)
	}

	h.clock.Advance(defaultDwell(t, cp.LimitUnschedulableGrace))
	h.observer.SettleForTest(ctx)
	if rows := activeFailures(t, h.db); len(rows) != 1 {
		t.Fatalf("the row never opened once the dwell elapsed: %+v", rows)
	}
}

// TestObserverHonoursAnOperatorsDwell: the dwell is `status.` configuration (ADR-0079 §3), so an
// operator whose autoscaler takes ninety seconds to provision a node says so once and the ledger
// honours it — which is the whole reason these are limits rather than constants.
func TestObserverHonoursAnOperatorsDwell(t *testing.T) {
	ctx := context.Background()
	h := newObserverHarness(t, cp.ObserverConfig{})
	if err := h.engine.SetLimit(ctx, "", cp.LimitUnschedulableGrace, "90s"); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	watchedApp(t, h, "web")

	h.k8s.SetIssue("web", cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: "0/3 nodes are available"})
	h.observer.DrainForTest(ctx)

	h.clock.Advance(defaultDwell(t, cp.LimitUnschedulableGrace))
	h.observer.SettleForTest(ctx)
	if rows := activeFailures(t, h.db); len(rows) != 0 {
		t.Fatalf("the built-in grace opened a row the operator asked to wait longer for: %+v", rows)
	}

	h.clock.Advance(90*time.Second - defaultDwell(t, cp.LimitUnschedulableGrace))
	h.observer.SettleForTest(ctx)
	if rows := activeFailures(t, h.db); len(rows) != 1 {
		t.Fatalf("the row did not open at the operator's ninety seconds: %+v", rows)
	}
}
