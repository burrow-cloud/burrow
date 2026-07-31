// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/client"
)

// The presentation half of ADR-0074 §8. What is asserted here is what the record is emphatic about:
// a cascade reads as ONE event rather than a wall of red, the earliest row leads, the output never
// claims a cause, and a hole in the coverage is impossible to miss.

var testNow = time.Date(2026, 7, 31, 15, 0, 0, 0, time.UTC)

// at is a timestamp d before the fixed test now.
func at(d time.Duration) time.Time { return testNow.Add(-d) }

// row builds a ledger row for the formatter.
func row(id int64, kind, name, reason string, firstSeen time.Duration, occurrences int64) client.Failure {
	return client.Failure{
		ID:          id,
		Object:      client.ObjectRef{Kind: kind, Name: name, Environment: "prod"},
		Reason:      reason,
		Detail:      reason + " detail for " + name,
		FirstSeen:   at(firstSeen),
		LastSeen:    at(time.Minute),
		Occurrences: occurrences,
	}
}

// continuous is coverage with no holes in it, so a test about the rows is not also a test about the
// coverage warning.
func continuous() client.Coverage {
	return client.Coverage{
		Since:   at(24 * time.Hour),
		Until:   testNow,
		Windows: []client.ObservationWindow{{ID: 1, StartedAt: at(25 * time.Hour), Until: testNow, Sweeps: 1500}},
		Gaps:    []client.CoverageGap{},
	}
}

// TestFailuresGroupsByReasonOldestFirst: one taint takes out three objects and one unrelated app
// crash-loops. The listing must read as two causes, with the older one first, rather than as four
// separate problems (ADR-0074 §5).
func TestFailuresGroupsByReasonOldestFirst(t *testing.T) {
	report := client.FailureReport{
		Coverage: continuous(),
		Failures: []client.Failure{
			row(1, client.FailureApp, "api", "Unschedulable", 30*time.Minute, 30),
			row(2, client.FailureAddon, "postgres", "Unschedulable", 29*time.Minute, 29),
			row(3, client.FailureApp, "web", "Unschedulable", 28*time.Minute, 28),
			row(4, client.FailureApp, "worker", "CrashLoopBackOff", 90*time.Minute, 90),
		},
	}
	out := formatFailures(io.Discard, report, testNow)

	groups := groupFailures(report.Failures)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 — four rows, two shared reasons", len(groups))
	}
	if groups[0].Reason != "CrashLoopBackOff" {
		t.Errorf("first group = %q, want the older cause to lead (ADR-0074 §5)", groups[0].Reason)
	}
	if len(groups[1].Rows) != 3 {
		t.Errorf("the Unschedulable group holds %d rows, want the three objects one taint took out", len(groups[1].Rows))
	}
	// The cascade reads as one heading with the objects under it.
	if !strings.Contains(out, "Unschedulable — 3 objects") {
		t.Errorf("output does not present the cascade as one event:\n%s", out)
	}
	if strings.Index(out, "CrashLoopBackOff — 1 object") > strings.Index(out, "Unschedulable — 3 objects") {
		t.Errorf("groups are not oldest-first:\n%s", out)
	}
	// The rows underneath stay individually addressable.
	for _, want := range []string{"app/api (prod)", "addon/postgres (prod)", "app/web (prod)", "app/worker (prod)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not name %q — grouping is presentation, the rows are the record:\n%s", want, out)
		}
	}
}

// TestFailuresNeverClaimsACause: the grouping is a heuristic that will sometimes place two unrelated
// failures side by side, and the output has to say so itself rather than leaving the reader to infer
// how much it is claiming (ADR-0074 §5).
func TestFailuresNeverClaimsACause(t *testing.T) {
	out := formatFailures(io.Discard, client.FailureReport{
		Coverage: continuous(),
		Failures: []client.Failure{row(1, client.FailureApp, "api", "Unschedulable", time.Hour, 60)},
	}, testNow)

	if !strings.Contains(out, "correlation") || !strings.Contains(out, "not a cause") {
		t.Errorf("output does not present the grouping as correlation rather than diagnosis:\n%s", out)
	}
	for _, forbidden := range []string{"root cause", "caused by", "because of"} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Errorf("output claims causation with %q:\n%s", forbidden, out)
		}
	}
}

// TestFailuresEmptyWithAGapDoesNotReadAsHealth: the specific joke ADR-0074's consequences refuse to
// make. An empty list over an hour nobody was watching must not be printed as "nothing is broken".
func TestFailuresEmptyWithAGapDoesNotReadAsHealth(t *testing.T) {
	out := formatFailures(io.Discard, client.FailureReport{
		Failures: []client.Failure{},
		Coverage: client.Coverage{
			Since:   at(24 * time.Hour),
			Until:   testNow,
			Windows: []client.ObservationWindow{{ID: 1, StartedAt: at(25 * time.Hour), Until: at(time.Hour), Sweeps: 1441}},
			Gaps:    []client.CoverageGap{{From: at(time.Hour), To: testNow}},
		},
	}, testNow)

	if !strings.Contains(out, "coverage is incomplete") {
		t.Errorf("output does not lead with the coverage caveat:\n%s", out)
	}
	if !strings.Contains(out, "no observations from") {
		t.Errorf("output does not name the gap:\n%s", out)
	}
	if !strings.Contains(out, "not the same as nothing having broken") {
		t.Errorf("an empty list over a gap reads as health:\n%s", out)
	}
	// The caveat leads: it qualifies everything below it, and a note after the rows is a note
	// nobody reads at 3am.
	if strings.Index(out, "coverage is incomplete") > strings.Index(out, "No failures are recorded") {
		t.Errorf("the coverage caveat trails the list instead of leading it:\n%s", out)
	}
}

// TestFailuresWithNoObserverSaysSo: nothing has ever watched, so the listing says the answer is not a
// claim about the cluster at all — the loudest of the three coverage cases, not the quietest.
func TestFailuresWithNoObserverSaysSo(t *testing.T) {
	out := formatFailures(io.Discard, client.FailureReport{
		Failures: []client.Failure{},
		Coverage: client.Coverage{
			Since:   at(24 * time.Hour),
			Until:   testNow,
			Windows: []client.ObservationWindow{},
			Gaps:    []client.CoverageGap{{From: at(24 * time.Hour), To: testNow}},
		},
	}, testNow)

	if !strings.Contains(out, "No observation coverage is recorded") {
		t.Errorf("output does not say that nothing was watching:\n%s", out)
	}
	if !strings.Contains(out, "not evidence that nothing") {
		t.Errorf("output does not warn that the empty list means nothing:\n%s", out)
	}
}

// TestFailuresCleanCoverageIsStated: the reassuring case is stated rather than left as an absence, so
// "no warning" is never something the reader has to notice.
func TestFailuresCleanCoverageIsStated(t *testing.T) {
	out := formatFailures(io.Discard, client.FailureReport{Failures: []client.Failure{}, Coverage: continuous()}, testNow)
	if !strings.Contains(out, "Observed continuously") {
		t.Errorf("output does not state that coverage was continuous:\n%s", out)
	}
	if !strings.Contains(out, "No failures are recorded") {
		t.Errorf("output does not report the empty list:\n%s", out)
	}
}

// TestFailuresDegradedSweepsAreReported: partial coverage is neither clean coverage nor a gap, and it
// gets its own line, because rows for the objects a degraded sweep could not read may be missing.
func TestFailuresDegradedSweepsAreReported(t *testing.T) {
	cov := continuous()
	cov.DegradedSweeps = 3
	cov.Detail = "the workloads in namespace \"staging\" could not be listed"
	out := formatFailures(io.Discard, client.FailureReport{Failures: []client.Failure{}, Coverage: cov}, testNow)

	if !strings.Contains(out, "3 sweeps could not read every object") {
		t.Errorf("output does not report the degraded sweeps:\n%s", out)
	}
	if !strings.Contains(out, "staging") {
		t.Errorf("output does not carry the degradation note:\n%s", out)
	}
}

// TestFailuresResolvedRowsReadAsHistory: a `--since` listing mixes failures that ended with ones that
// have not, and reading a resolved row as current is the mistake the listing must not invite.
func TestFailuresResolvedRowsReadAsHistory(t *testing.T) {
	resolved := row(1, client.FailureApp, "web", "CrashLoopBackOff", 3*time.Hour, 60)
	end := at(2 * time.Hour)
	resolved.ResolvedAt = &end
	out := formatFailures(io.Discard, client.FailureReport{Coverage: continuous(), Failures: []client.Failure{resolved}}, testNow)

	if !strings.Contains(out, "resolved") {
		t.Errorf("output does not mark the row as recovered:\n%s", out)
	}
	// The timestamp is rendered in the reader's own zone, so the assertion is on the rendering and
	// not on a fixed offset.
	if !strings.Contains(out, "resolved "+end.Local().Format(timestampLayout)+" after 1h") {
		t.Errorf("output does not say when it ended and how long it ran:\n%s", out)
	}
}

// TestFailuresCarriesTheDetail: the detail is the actionable half of a row, so it is printed in full
// rather than truncated into a column.
func TestFailuresCarriesTheDetail(t *testing.T) {
	f := row(1, client.FailureApp, "api", "Unschedulable", time.Hour, 60)
	f.Detail = "0/3 nodes are available: 3 node(s) had untolerated taint {maintenance: true}"
	out := formatFailures(io.Discard, client.FailureReport{Coverage: continuous(), Failures: []client.Failure{f}}, testNow)
	if !strings.Contains(out, f.Detail) {
		t.Errorf("output does not carry the row's detail in full:\n%s", out)
	}
}

// TestGroupFailuresIsIndependentOfInputOrder: the ordering rule is a property of the grouping, not an
// accident of the order the server happened to return.
func TestGroupFailuresIsIndependentOfInputOrder(t *testing.T) {
	rows := []client.Failure{
		row(3, client.FailureApp, "web", "Unschedulable", 10*time.Minute, 10),
		row(1, client.FailureApp, "api", "Unschedulable", 30*time.Minute, 30),
		row(2, client.FailureApp, "worker", "OOMKilled", 20*time.Minute, 20),
	}
	groups := groupFailures(rows)
	if len(groups) != 2 || groups[0].Reason != "Unschedulable" || groups[1].Reason != "OOMKilled" {
		t.Fatalf("groups = %+v, want Unschedulable (oldest at 30m) then OOMKilled", groups)
	}
	if groups[0].Rows[0].Object.Name != "api" {
		t.Errorf("rows within a group are not oldest-first: %+v", groups[0].Rows)
	}
}

// TestFailuresPlurals: no line in an incident surface should read "1 objects".
func TestFailuresPlurals(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{1, "1 object"}, {2, "2 objects"}} {
		if got := plural(tc.n, "object", "objects"); got != tc.want {
			t.Errorf("plural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
