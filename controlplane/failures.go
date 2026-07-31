// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"time"
)

// The READ side of the failure ledger (ADR-0074 §8): the cluster-wide answer to "what is broken",
// so the second step of a diagnosis stops being `kubectl describe`. The write side is observer.go;
// nothing here mutates anything.
//
// IT RETURNS ROWS, NEVER GROUPS. ADR-0074 §5 puts the grouping in the presentation layer on purpose:
// grouping by shared reason is a HEURISTIC that will sometimes place two unrelated crash loops side
// by side, and an agent reading this surface must be able to correlate on its own terms rather than
// inherit a human-facing hint it cannot see the shape of. The `burrow failures` listing groups; this
// does not, and neither does the JSON either CLI prints.
//
// AND IT ALWAYS REPORTS ITS OWN COVERAGE. An empty list is two very different facts — "nothing broke"
// and "nobody was watching" — and a reliability surface that cannot tell them apart is the specific
// joke ADR-0074's consequences refuse to make. So every answer carries the observation windows behind
// it and the literal gaps between them, and no caller has to ask a second question to find out
// whether the first answer meant anything.

// DefaultFailureLookback is the window the coverage record is read over when a query names none. It
// bounds only the COVERAGE half of the answer: the failures themselves are the active ones, which
// have no age. A day is the span over which "was Burrow watching?" is a question with a useful
// answer — long enough to cover a night nobody was present for, short enough that the gap list is
// about the current incident rather than the month.
const DefaultFailureLookback = 24 * time.Hour

// FailureQuery is the read side's request. It differs from FailureFilter in one way that matters:
// Since is a DURATION rather than an instant, resolved against the control plane's own injected
// clock. The ledger's timestamps were written by that clock, so a client resolving "the last hour"
// against its own would query a window skewed by however wrong its clock is — and the caller most
// likely to be wrong about the time is an agent on a laptop that just woke from sleep.
type FailureQuery struct {
	// Kind restricts to one class of managed object; empty matches any.
	Kind FailureKind
	// Name restricts to one object name; empty matches any.
	Name string
	// Environment restricts to one environment; empty matches any.
	Environment string
	// Reason restricts to one reason from the closed ledger vocabulary; empty matches any. It is
	// ADR-0074 §5's grouping asked from the query side: "what else hit this in the same window".
	Reason string
	// Since bounds the answer to failures last seen within this much of now, and widens it from the
	// active failures to the history — a window over the past is not a question about the present.
	// Zero leaves the query on the active rows and reads coverage over DefaultFailureLookback.
	Since time.Duration
	// IncludeResolved widens the answer to failures that have since recovered, with no time bound
	// beyond the ledger's retention.
	IncludeResolved bool
	// Limit caps the rows returned. Zero or negative applies the store's default cap.
	Limit int
}

// filter renders the query as the store-level filter, resolving Since against now.
func (q FailureQuery) filter(now time.Time) FailureFilter {
	f := FailureFilter{
		Kind:            q.Kind,
		Name:            q.Name,
		Environment:     q.Environment,
		Reason:          q.Reason,
		IncludeResolved: q.IncludeResolved,
		Limit:           q.Limit,
	}
	if q.Since > 0 {
		f.Since = now.Add(-q.Since)
		// A window over the past that showed only what is still broken would answer a question
		// nobody asked: "did this happen last night" is exactly a question about failures that have
		// since recovered.
		f.IncludeResolved = true
	}
	return f
}

// coverageSince is the instant the coverage half of the answer is read from.
func (q FailureQuery) coverageSince(now time.Time) time.Time {
	if q.Since > 0 {
		return now.Add(-q.Since)
	}
	return now.Add(-DefaultFailureLookback)
}

// CoverageGap is a stretch of time no observer was recording. It is a LITERAL gap, not an inference:
// each observation window is one continuous run of the observer, so the time between two windows is
// time in which burrowd was down, restarting, or wedged — and the failures that began and ended
// inside it were not seen and are not in the ledger.
type CoverageGap struct {
	// From is when coverage stopped.
	From time.Time `json:"from"`
	// To is when it resumed — or the moment the answer was assembled, for a gap that is still open.
	To time.Time `json:"to"`
}

// Duration is how long the gap lasted.
func (g CoverageGap) Duration() time.Duration { return g.To.Sub(g.From) }

// Coverage is what the observer was doing over the period an answer describes, reported ALONGSIDE
// every answer rather than on request.
//
// ADR-0074's consequences name the failure this exists to prevent: "if the observer was down from
// 02:00 to 03:00, an empty ledger for that hour reads as 'nothing broke'". A caller that has to ask
// a second question to learn the first answer was hollow will not ask it at 3am, so the answer
// carries its own caveat.
type Coverage struct {
	// Since and Until bound the period this describes.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// Windows are the observer runs overlapping that period, oldest first.
	Windows []ObservationWindow `json:"windows"`
	// Gaps are the stretches inside it that no window covers, oldest first. Non-empty means the
	// ledger is INCOMPLETE for this period and an empty failure list says nothing about it.
	Gaps []CoverageGap `json:"gaps"`
	// DegradedSweeps is how many sweeps across those windows could not read every object they set
	// out to. It is partial coverage rather than none: rows for the objects a degraded sweep could
	// not read may be missing or stale, and their absence is not evidence of health either.
	DegradedSweeps int64 `json:"degraded_sweeps,omitempty"`
	// Detail is the most recent degradation note across those windows, empty when none degraded.
	Detail string `json:"detail,omitempty"`
}

// Complete reports whether the ledger can be read at face value for this period: continuous
// coverage, and every sweep in it read everything it set out to.
func (c Coverage) Complete() bool { return len(c.Gaps) == 0 && c.DegradedSweeps == 0 }

// Observed reports whether anything was watching at all. False is the starkest case — no window
// overlaps the period, so an empty failure list is not a claim about the cluster.
func (c Coverage) Observed() bool { return len(c.Windows) > 0 }

// Failures answers the cluster-wide question ADR-0074 §8 asks: what, across everything Burrow
// manages, is broken — with the coverage behind that answer attached.
//
// It reads only the ledger and the coverage record. It does NOT consult the cluster: current state
// is a live read that belongs to Status, and a listing that re-derived thirty objects' state from
// the API server during the incident that made them all fail is the wrong thing to be doing at that
// moment. The rows are what the observer saw at the time, which is the fact the reader is after.
func (e *Engine) Failures(ctx context.Context, q FailureQuery) (FailureReport, error) {
	if q.Kind != "" && !q.Kind.Valid() {
		return FailureReport{}, fmt.Errorf("failures: unknown kind %q: %w", q.Kind, ErrInvalid)
	}
	if q.Reason != "" && !IsLedgerReason(q.Reason) {
		return FailureReport{}, fmt.Errorf("failures: %q is not a reason the ledger records: %w", q.Reason, ErrInvalid)
	}
	now := e.clock.Now()
	rows, err := e.db.Failures(ctx, q.filter(now))
	if err != nil {
		return FailureReport{}, fmt.Errorf("failures: %w", err)
	}
	cov, err := e.coverage(ctx, q.coverageSince(now), now)
	if err != nil {
		return FailureReport{}, fmt.Errorf("failures: %w", err)
	}
	return FailureReport{Failures: rows, Coverage: cov}, nil
}

// FailureReport is one answer from the ledger: the rows, and what the observer was doing while they
// were (or were not) being recorded.
type FailureReport struct {
	// Failures are the ledger rows, oldest first — the order ADR-0074 §5 asks for, because the
	// earliest first_seen in a cascade is the likeliest thing to actually fix. They are rows and not
	// groups on purpose: see the file comment.
	Failures []Failure `json:"failures"`
	// Coverage is what was watching over the period described. It is never omitted.
	Coverage Coverage `json:"coverage"`
}

// coverage reads the observation windows overlapping [since, until] and turns the space between them
// into the explicit gap list.
//
// A read failure here is returned rather than swallowed. Degrading to "rows without coverage" would
// produce exactly the answer this surface exists to prevent — a list that looks authoritative and is
// not — so the honest failure is no answer at all.
func (e *Engine) coverage(ctx context.Context, since, until time.Time) (Coverage, error) {
	windows, err := e.db.ObservationWindows(ctx, since, 0)
	if err != nil {
		return Coverage{}, fmt.Errorf("reading observation coverage: %w", err)
	}
	return coverageOver(since, until, windows), nil
}

// coverageOver is the pure computation behind Coverage, kept separate from the read so the gap
// arithmetic is testable without a store. windows arrive oldest first.
func coverageOver(since, until time.Time, windows []ObservationWindow) Coverage {
	cov := Coverage{Since: since, Until: until, Windows: windows, Gaps: []CoverageGap{}}
	if cov.Windows == nil {
		cov.Windows = []ObservationWindow{}
	}
	for _, w := range windows {
		cov.DegradedSweeps += w.DegradedSweeps
		if w.Detail != "" {
			cov.Detail = w.Detail
		}
	}

	// The tolerance is the observer's own sampling interval, inferred from the most recent window
	// rather than assumed: coverage always trails `now` by up to one sweep, and reporting that lag
	// as an outage every single time would train a reader to ignore the gap list — which is the one
	// part of this answer that must never be ignored. Two missed sweeps is the threshold, so a
	// single slow sweep is quiet and a stopped observer is not.
	tolerance := 2 * sweepCadence(windows)

	cursor := since
	for _, w := range windows {
		start := w.StartedAt
		if start.Before(since) {
			start = since
		}
		if start.Sub(cursor) > tolerance {
			cov.Gaps = append(cov.Gaps, CoverageGap{From: cursor, To: start})
		}
		if w.Until.After(cursor) {
			cursor = w.Until
		}
	}
	// The trailing gap is the important one: it is an observer that stopped and has not come back,
	// which is the state in which every other reliability answer Burrow gives is stale.
	if until.Sub(cursor) > tolerance {
		cov.Gaps = append(cov.Gaps, CoverageGap{From: cursor, To: until})
	}
	return cov
}

// sweepCadence infers how often the observer is sweeping from the most recent window that ran long
// enough to say: a window of n sweeps spans n-1 intervals. It is derived rather than read from
// configuration because the configuration belongs to burrowd's process and this answer may be
// assembled long after a sweep with a different interval wrote the rows — and a tolerance that
// disagreed with the data would either hide a real gap or invent one. DefaultObserveInterval is the
// fallback when no window has swept twice.
func sweepCadence(windows []ObservationWindow) time.Duration {
	for i := len(windows) - 1; i >= 0; i-- {
		w := windows[i]
		if w.Sweeps > 1 && w.Until.After(w.StartedAt) {
			return w.Until.Sub(w.StartedAt) / time.Duration(w.Sweeps-1)
		}
	}
	return DefaultObserveInterval
}

// StatusFailureLookback is how far back an app's status reaches for its recent-failure history
// (ADR-0074 §8: "a short recent-failure history for that app"). It is the same span as the listing's
// default coverage window, so the two surfaces answer about the same period and a reader moving
// between them is not comparing a day against a month.
const StatusFailureLookback = DefaultFailureLookback

// statusFailureRows bounds that history. Status is a live read someone runs to find out what is
// wrong right now; the full episode list belongs to the listing, which has the filters for it.
const statusFailureRows = 10

// appFailures reads one app's recent failure history and the coverage over the same window, for the
// status surface. It is the per-app view of the same ledger the cluster-wide listing reads.
//
// Coverage travels with it for the reason it travels with everything else here: an app whose history
// is empty because burrowd was down all night must not read as an app that had a quiet night.
func (e *Engine) appFailures(ctx context.Context, app, env string) ([]Failure, Coverage, error) {
	now := e.clock.Now()
	since := now.Add(-StatusFailureLookback)
	rows, err := e.db.Failures(ctx, FailureFilter{
		Kind:            FailureApp,
		Name:            app,
		Environment:     env,
		Since:           since,
		IncludeResolved: true,
	})
	if err != nil {
		return nil, Coverage{}, fmt.Errorf("reading failure history: %w", err)
	}
	// The store orders oldest first, so a truncation applied there would drop the NEWEST rows —
	// the opposite of a recent history. Bound the window in the query and keep the tail here.
	if len(rows) > statusFailureRows {
		rows = rows[len(rows)-statusFailureRows:]
	}
	cov, err := e.coverage(ctx, since, now)
	if err != nil {
		return nil, Coverage{}, err
	}
	return rows, cov, nil
}
