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

// The read surface over the failure ledger (ADR-0074 §8). Two properties carry the record's weight
// and are asserted here rather than left to the CLI: the answer is ROWS and never groups, and it
// always carries its own observation coverage so a gap cannot be mistaken for health.

// seedFailure records one observed failure in the fake ledger.
func seedFailure(t *testing.T, d *fake.Database, ref cp.ObjectRef, reason string, at time.Time) {
	t.Helper()
	if err := d.RecordFailure(context.Background(), cp.FailureObservation{
		Object: ref, Reason: reason, Detail: "seeded", At: at,
	}); err != nil {
		t.Fatalf("RecordFailure(%s on %s): %v", reason, ref, err)
	}
}

// seedWindow records one run of the observer covering [start, until] in sweeps sweeps, which is how
// a real observer's coverage reaches the store.
func seedWindow(t *testing.T, d *fake.Database, start, until time.Time, sweeps int, degraded string) {
	t.Helper()
	ctx := context.Background()
	id, err := d.StartObservationWindow(ctx, start)
	if err != nil {
		t.Fatalf("StartObservationWindow: %v", err)
	}
	step := until.Sub(start) / time.Duration(max(sweeps-1, 1))
	for i := range sweeps {
		at := start.Add(step * time.Duration(i))
		if i == sweeps-1 {
			at = until
		}
		note := ""
		if degraded != "" && i == sweeps-1 {
			note = degraded
		}
		if err := d.ExtendObservationWindow(ctx, id, at, note); err != nil {
			t.Fatalf("ExtendObservationWindow: %v", err)
		}
	}
}

// TestFailuresReturnsRowsNeverGroups: one cause produces many rows and the API hands back every one
// of them, individually addressable and oldest first. ADR-0074 §5 keeps the grouping in the
// presentation layer precisely so the agent correlates on its own terms, and a listing that
// collapsed a cascade here would take that choice away from it.
func TestFailuresReturnsRowsNeverGroups(t *testing.T) {
	ctx := context.Background()
	e, _, d, c := newEngine(t, permissive())
	now := c.Now()

	// One taint, four unschedulable objects, in the order they were first seen.
	seedFailure(t, d, cp.ObjectRef{Kind: cp.FailureApp, Name: "api", Environment: "prod"}, cp.ReasonUnschedulable, now.Add(-30*time.Minute))
	seedFailure(t, d, cp.ObjectRef{Kind: cp.FailureAddon, Name: "postgres", Environment: "prod"}, cp.ReasonUnschedulable, now.Add(-29*time.Minute))
	seedFailure(t, d, cp.ObjectRef{Kind: cp.FailureApp, Name: "web", Environment: "prod"}, cp.ReasonUnschedulable, now.Add(-28*time.Minute))
	// And one unrelated failure that started earlier still.
	seedFailure(t, d, cp.ObjectRef{Kind: cp.FailureApp, Name: "worker", Environment: "prod"}, cp.ReasonCrashLoopBackOff, now.Add(-90*time.Minute))
	seedWindow(t, d, now.Add(-2*time.Hour), now, 121, "")

	report, err := e.Failures(ctx, cp.FailureQuery{})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(report.Failures) != 4 {
		t.Fatalf("got %d rows, want 4 — the cascade must stay four addressable rows: %+v", len(report.Failures), report.Failures)
	}
	for i := 1; i < len(report.Failures); i++ {
		if report.Failures[i].FirstSeen.Before(report.Failures[i-1].FirstSeen) {
			t.Fatalf("rows are not oldest-first: %+v", report.Failures)
		}
	}
	if report.Failures[0].Object.Name != "worker" {
		t.Errorf("oldest row is %q, want the crash loop that started first — the earliest first_seen leads (ADR-0074 §5)",
			report.Failures[0].Object.Name)
	}
}

// TestFailuresReportsACoverageGap: the observer stopped an hour ago, so the answer says so. This is
// the failure mode ADR-0074's consequences name outright — an empty ledger for an hour nobody was
// watching reads as "nothing broke" — and the gap is what makes the two tellable apart.
func TestFailuresReportsACoverageGap(t *testing.T) {
	ctx := context.Background()
	e, _, d, c := newEngine(t, permissive())
	now := c.Now()

	// burrowd observed continuously for a day, then stopped an hour ago and has not come back.
	seedWindow(t, d, now.Add(-25*time.Hour), now.Add(-time.Hour), 1441, "")

	report, err := e.Failures(ctx, cp.FailureQuery{})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("got %d rows, want none seeded", len(report.Failures))
	}
	if report.Coverage.Complete() {
		t.Fatal("coverage reports complete over an hour with no observer — an empty list would read as health")
	}
	if len(report.Coverage.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want exactly the hour since the observer stopped", report.Coverage.Gaps)
	}
	gap := report.Coverage.Gaps[0]
	if !gap.From.Equal(now.Add(-time.Hour)) || !gap.To.Equal(now) {
		t.Errorf("gap = %s → %s, want %s → %s", gap.From, gap.To, now.Add(-time.Hour), now)
	}
	if !report.Coverage.Observed() {
		t.Error("coverage reports nothing observed, but a window covers most of the period")
	}
}

// TestFailuresWithNoObserverAtAll: the starkest case. Nothing has ever observed, so the empty list
// is not a claim about the cluster, and the coverage says exactly that rather than staying silent.
func TestFailuresWithNoObserverAtAll(t *testing.T) {
	ctx := context.Background()
	e, _, _, c := newEngine(t, permissive())

	report, err := e.Failures(ctx, cp.FailureQuery{})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if report.Coverage.Observed() {
		t.Fatal("coverage claims something was observing when no window exists")
	}
	if len(report.Coverage.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want the whole period reported as one gap", report.Coverage.Gaps)
	}
	if !report.Coverage.Gaps[0].From.Equal(c.Now().Add(-cp.DefaultFailureLookback)) {
		t.Errorf("gap starts at %s, want the whole default lookback %s", report.Coverage.Gaps[0].From, cp.DefaultFailureLookback)
	}
}

// TestFailuresContinuousCoverageIsQuiet: a healthy observer's normal lag behind `now` — coverage
// advances only on a completed sweep — must not be reported as a gap. A gap list that cried wolf
// every minute would train its reader to skip the one part of this answer that must never be
// skipped.
func TestFailuresContinuousCoverageIsQuiet(t *testing.T) {
	ctx := context.Background()
	e, _, d, c := newEngine(t, permissive())
	now := c.Now()

	// Sweeping every minute for a day; the last completed sweep was 40 seconds ago, which is
	// coverage trailing by less than one interval rather than an observer that stopped.
	seedWindow(t, d, now.Add(-25*time.Hour), now.Add(-40*time.Second), 1500, "")

	report, err := e.Failures(ctx, cp.FailureQuery{})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if !report.Coverage.Complete() {
		t.Fatalf("coverage = %+v, want complete: a sub-interval lag is the sampling cadence, not an outage", report.Coverage)
	}
}

// TestFailuresReportsDegradedSweeps: partial coverage is its own answer. A sweep that ran but could
// not read every object leaves rows that may be missing or stale, and their absence is not evidence
// of health either.
func TestFailuresReportsDegradedSweeps(t *testing.T) {
	ctx := context.Background()
	e, _, d, c := newEngine(t, permissive())
	now := c.Now()
	seedWindow(t, d, now.Add(-25*time.Hour), now, 1501, "the workloads in namespace \"staging\" could not be listed")

	report, err := e.Failures(ctx, cp.FailureQuery{})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(report.Coverage.Gaps) != 0 {
		t.Fatalf("gaps = %+v, want none: the observer never stopped", report.Coverage.Gaps)
	}
	if report.Coverage.Complete() {
		t.Fatal("coverage reports complete despite a sweep that could not read everything")
	}
	if report.Coverage.DegradedSweeps != 1 || report.Coverage.Detail == "" {
		t.Errorf("coverage = %+v, want one degraded sweep and its note", report.Coverage)
	}
}

// TestFailuresSinceWidensToHistory: the default answers "what is broken"; --since answers "what
// broke last night", which is a question about failures that have since recovered. A window over the
// past that showed only what is still active would answer neither.
func TestFailuresSinceWidensToHistory(t *testing.T) {
	ctx := context.Background()
	e, _, d, c := newEngine(t, permissive())
	now := c.Now()
	ref := cp.ObjectRef{Kind: cp.FailureApp, Name: "web", Environment: "prod"}

	seedFailure(t, d, ref, cp.ReasonCrashLoopBackOff, now.Add(-3*time.Hour))
	if err := d.ResolveFailures(ctx, now.Add(-2*time.Hour), nil, nil); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}
	seedWindow(t, d, now.Add(-6*time.Hour), now, 361, "")

	active, err := e.Failures(ctx, cp.FailureQuery{})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(active.Failures) != 0 {
		t.Fatalf("default listing returned %d rows, want only what is still broken: %+v", len(active.Failures), active.Failures)
	}

	history, err := e.Failures(ctx, cp.FailureQuery{Since: 4 * time.Hour})
	if err != nil {
		t.Fatalf("Failures(--since): %v", err)
	}
	if len(history.Failures) != 1 || history.Failures[0].Active() {
		t.Fatalf("--since returned %+v, want the one resolved episode", history.Failures)
	}
	if !history.Coverage.Since.Equal(now.Add(-4 * time.Hour)) {
		t.Errorf("coverage window starts at %s, want the queried window %s", history.Coverage.Since, now.Add(-4*time.Hour))
	}
}

// TestFailuresRejectsAnUnknownFilter: the kind and reason vocabularies are closed, so a typo is an
// error rather than an empty list. An empty list is the one answer this surface must never give by
// accident.
func TestFailuresRejectsAnUnknownFilter(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	for _, q := range []cp.FailureQuery{{Kind: "deployment"}, {Reason: "ContainerCreating"}} {
		if _, err := e.Failures(ctx, q); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("Failures(%+v) error = %v, want ErrInvalid", q, err)
		}
	}
}

// TestFailuresFailsRatherThanHideCoverage: if the coverage record cannot be read, the answer is no
// answer. Returning rows with an empty coverage block would produce exactly the listing this surface
// exists to prevent — one that looks authoritative and is not.
func TestFailuresFailsRatherThanHideCoverage(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	d.SetError(fake.OpObservationWindows, errors.New("database unavailable"))

	if _, err := e.Failures(ctx, cp.FailureQuery{}); err == nil {
		t.Fatal("Failures returned an answer while the coverage record was unreadable")
	}
}

// TestStatusCarriesRecentFailureHistory: `burrow status <app>` keeps its live read and gains the
// short history ADR-0074 §8 asks for — the half no live read can produce, because the cluster does
// not keep the evidence. The coverage travels with it for the same reason it travels with the
// listing.
func TestStatusCarriesRecentFailureHistory(t *testing.T) {
	ctx := context.Background()
	e, _, d, c := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	now := c.Now()
	ref := cp.ObjectRef{Kind: cp.FailureApp, Name: "web", Environment: cp.DefaultEnvironment}
	seedFailure(t, d, ref, cp.ReasonCrashLoopBackOff, now.Add(-4*time.Hour))
	// Another app's failure, which must not appear on this app's status.
	seedFailure(t, d, cp.ObjectRef{Kind: cp.FailureApp, Name: "api", Environment: cp.DefaultEnvironment}, cp.ReasonOOMKilled, now.Add(-3*time.Hour))
	// And one outside the lookback window.
	seedFailure(t, d, ref, cp.ReasonUnschedulable, now.Add(-40*time.Hour))
	seedWindow(t, d, now.Add(-25*time.Hour), now, 1501, "")

	st, err := e.Status(ctx, "web", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Failures) != 1 {
		t.Fatalf("status failures = %+v, want only this app's recent one", st.Failures)
	}
	if st.Failures[0].Reason != cp.ReasonCrashLoopBackOff {
		t.Errorf("status failure reason = %q, want %q", st.Failures[0].Reason, cp.ReasonCrashLoopBackOff)
	}
	if !st.Coverage.Complete() {
		t.Errorf("coverage = %+v, want complete over a continuously observed window", st.Coverage)
	}
	// The live read is untouched: status is still derived, never served from the ledger.
	if !st.Running || !st.Workload.Available {
		t.Errorf("workload = %+v, want the live read unchanged", st.Workload)
	}
}

// TestStatusHistoryShowsItsOwnGap: an app with no rows because nothing was watching must not read as
// an app that had a quiet night.
func TestStatusHistoryShowsItsOwnGap(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	st, err := e.Status(ctx, "web", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Failures) != 0 {
		t.Fatalf("status failures = %+v, want none", st.Failures)
	}
	if st.Coverage.Observed() || st.Coverage.Complete() {
		t.Errorf("coverage = %+v, want it to report that nothing was watching", st.Coverage)
	}
}
