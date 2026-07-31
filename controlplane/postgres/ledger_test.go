// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/postgres"
)

// The ledger's store tests run against a real Postgres, because the load-bearing behaviour is the
// PARTIAL unique index on active rows — at most one active row per (object, reason), resolved rows
// exempt — and no in-memory fake can prove that the database enforces it.
//
// They isolate themselves the way the other store tests do: every object name is prefixed with the
// test's name, and every assertion filters by it, so they are safe against a shared database. The
// two calls that cannot be scoped by name — ResolveFailures and PruneLedger — act on rows other
// tests have already finished asserting on, which is why nothing here asserts on a global count.

// ledgerRef builds an object reference scoped to this test.
func ledgerRef(t *testing.T, kind cp.FailureKind, name string) cp.ObjectRef {
	t.Helper()
	return cp.ObjectRef{Kind: kind, Name: strings.ToLower(t.Name()) + "-" + name, Environment: "prod"}
}

// TestStoreLedgerTransition: a failure observed twice is ONE row that counts, first_seen never
// moves, resolution closes it, and a recurrence opens a second episode rather than reviving the
// first (ADR-0074 §4).
func TestStoreLedgerTransition(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ref := ledgerRef(t, cp.FailureApp, "web")
	start := time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC)

	for i, at := range []time.Time{start, start.Add(time.Minute), start.Add(2 * time.Minute)} {
		if err := s.RecordFailure(ctx, cp.FailureObservation{
			Object: ref, Reason: cp.ReasonCrashLoopBackOff, Detail: "exited with code 1", At: at,
		}); err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
	}
	rows, err := s.Failures(ctx, cp.FailureFilter{Name: ref.Name, IncludeResolved: true})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("three sightings produced %d rows, want one that counts three: %+v", len(rows), rows)
	}
	f := rows[0]
	if !f.FirstSeen.Equal(start) {
		t.Errorf("first_seen = %s, want %s — it is the answer to \"when did it start\" and must not move", f.FirstSeen, start)
	}
	if !f.LastSeen.Equal(start.Add(2*time.Minute)) || f.Occurrences != 3 || !f.Active() {
		t.Errorf("row = %+v, want last_seen advanced, three occurrences, still active", f)
	}

	// It recovers: the row closes.
	resolvedAt := start.Add(3 * time.Minute)
	if err := s.ResolveFailures(ctx, resolvedAt, nil, nil); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}
	if active, err := s.Failures(ctx, cp.FailureFilter{Name: ref.Name}); err != nil || len(active) != 0 {
		t.Fatalf("after resolution: %d active rows (err %v), want none", len(active), err)
	}

	// It breaks again: a NEW row, because two episodes are not one long outage.
	again := start.Add(time.Hour)
	if err := s.RecordFailure(ctx, cp.FailureObservation{Object: ref, Reason: cp.ReasonCrashLoopBackOff, At: again}); err != nil {
		t.Fatalf("RecordFailure after resolution: %v", err)
	}
	rows, err = s.Failures(ctx, cp.FailureFilter{Name: ref.Name, IncludeResolved: true})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("a recurrence produced %d rows, want two episodes: %+v", len(rows), rows)
	}
	if !rows[0].FirstSeen.Before(rows[1].FirstSeen) {
		t.Errorf("rows are not oldest-first: %+v", rows)
	}
	if rows[1].Occurrences != 1 || !rows[1].Active() {
		t.Errorf("the second episode = %+v, want a fresh active row", rows[1])
	}
}

// TestStoreLedgerConcurrentReasons: two reasons on one object are two rows with independent
// lifetimes, and resolving one leaves the other alone (ADR-0074 §5). This is the property the
// partial unique index exists for — a single status column per object would drop the second.
func TestStoreLedgerConcurrentReasons(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ref := ledgerRef(t, cp.FailureApp, "web")
	at := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)

	for _, reason := range []string{cp.ReasonOOMKilled, cp.ReasonUnschedulable} {
		if err := s.RecordFailure(ctx, cp.FailureObservation{Object: ref, Reason: reason, At: at}); err != nil {
			t.Fatalf("RecordFailure(%s): %v", reason, err)
		}
	}
	rows, err := s.Failures(ctx, cp.FailureFilter{Name: ref.Name})
	if err != nil || len(rows) != 2 {
		t.Fatalf("two concurrent reasons produced %d rows (err %v), want 2", len(rows), err)
	}

	// The OOM kill is still happening; the scheduling failure is not.
	keep := []cp.FailureKey{{Object: ref, Reason: cp.ReasonOOMKilled}}
	if err := s.ResolveFailures(ctx, at.Add(time.Minute), keep, nil); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}
	active, err := s.Failures(ctx, cp.FailureFilter{Name: ref.Name})
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if len(active) != 1 || active[0].Reason != cp.ReasonOOMKilled {
		t.Fatalf("active rows = %+v, want only the OOM kill", active)
	}
}

// TestStoreLedgerLeavesUnreadObjectsAlone: an object the sweep could not read keeps its rows. A
// failure Burrow could not check is not a failure that recovered.
func TestStoreLedgerLeavesUnreadObjectsAlone(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	unread := ledgerRef(t, cp.FailureApp, "unread")
	healed := ledgerRef(t, cp.FailureApp, "healed")
	at := time.Date(2026, 7, 31, 4, 0, 0, 0, time.UTC)
	for _, ref := range []cp.ObjectRef{unread, healed} {
		if err := s.RecordFailure(ctx, cp.FailureObservation{Object: ref, Reason: cp.ReasonUnschedulable, At: at}); err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
	}

	if err := s.ResolveFailures(ctx, at.Add(time.Minute), nil, []cp.ObjectRef{unread}); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}
	if rows, err := s.Failures(ctx, cp.FailureFilter{Name: unread.Name}); err != nil || len(rows) != 1 {
		t.Errorf("the unread object's row = %d active (err %v), want 1 untouched", len(rows), err)
	}
	if rows, err := s.Failures(ctx, cp.FailureFilter{Name: healed.Name}); err != nil || len(rows) != 0 {
		t.Errorf("the healthy object's row = %d active (err %v), want 0", len(rows), err)
	}
}

// TestStoreLedgerRejectsAnUnvettedReason: the vocabulary is closed at the write, so a reason nobody
// decided to record cannot reach a row by accident (ADR-0074 §5 — the consumer branches on it).
func TestStoreLedgerRejectsAnUnvettedReason(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	err := s.RecordFailure(ctx, cp.FailureObservation{
		Object: ledgerRef(t, cp.FailureApp, "web"), Reason: "SomethingWentWrong", At: time.Now(),
	})
	if err == nil {
		t.Fatal("RecordFailure accepted a reason outside the ledger vocabulary")
	}
}

// TestStoreLedgerPrune: retention removes resolved rows past the window and never removes an active
// one (ADR-0074 §4).
func TestStoreLedgerPrune(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	resolved := ledgerRef(t, cp.FailureApp, "resolved")
	active := ledgerRef(t, cp.FailureApp, "active")
	// Well in the past, so this test's prune cutoff cannot reach rows another test is still using.
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, ref := range []cp.ObjectRef{resolved, active} {
		if err := s.RecordFailure(ctx, cp.FailureObservation{Object: ref, Reason: cp.ReasonWorkloadMissing, At: at}); err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
	}
	if err := s.ResolveFailures(ctx, at.Add(time.Minute), []cp.FailureKey{{Object: active, Reason: cp.ReasonWorkloadMissing}}, nil); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}

	res, err := s.PruneLedger(ctx, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneLedger: %v", err)
	}
	if res.Failures < 1 {
		t.Errorf("PruneLedger removed %d failures, want at least the resolved one", res.Failures)
	}
	if rows, err := s.Failures(ctx, cp.FailureFilter{Name: resolved.Name, IncludeResolved: true}); err != nil || len(rows) != 0 {
		t.Errorf("the resolved row survived retention: %d rows (err %v)", len(rows), err)
	}
	rows, err := s.Failures(ctx, cp.FailureFilter{Name: active.Name, IncludeResolved: true})
	if err != nil || len(rows) != 1 || !rows[0].Active() {
		t.Errorf("the active row = %+v (err %v), want it kept: a thing that is still broken is not history", rows, err)
	}
}

// TestStoreObservationWindows: a window is opened per run of the observer and extended by its
// sweeps, and the gap between two windows is the time nobody was watching.
func TestStoreObservationWindows(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	// A base far enough out that only this test's windows fall in the queried range.
	base := time.Date(2031, 3, 4, 1, 0, 0, 0, time.UTC)

	first, err := s.StartObservationWindow(ctx, base)
	if err != nil {
		t.Fatalf("StartObservationWindow: %v", err)
	}
	if err := s.ExtendObservationWindow(ctx, first, base.Add(time.Minute), ""); err != nil {
		t.Fatalf("ExtendObservationWindow: %v", err)
	}
	if err := s.ExtendObservationWindow(ctx, first, base.Add(2*time.Minute), "the add-on could not be read"); err != nil {
		t.Fatalf("ExtendObservationWindow: %v", err)
	}

	// burrowd restarts an hour later: a second window, not an extension of the first.
	second, err := s.StartObservationWindow(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("StartObservationWindow: %v", err)
	}
	if err := s.ExtendObservationWindow(ctx, second, base.Add(time.Hour+time.Minute), ""); err != nil {
		t.Fatalf("ExtendObservationWindow: %v", err)
	}

	windows, err := s.ObservationWindows(ctx, base, 0)
	if err != nil {
		t.Fatalf("ObservationWindows: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("got %d windows, want 2: %+v", len(windows), windows)
	}
	if windows[0].Sweeps != 2 || windows[0].DegradedSweeps != 1 || windows[0].Detail == "" {
		t.Errorf("first window = %+v, want two sweeps of which one degraded, with a note", windows[0])
	}
	if windows[0].Complete() {
		t.Errorf("a window with a degraded sweep reports itself complete")
	}
	if gap := windows[1].StartedAt.Sub(windows[0].Until); gap != 58*time.Minute {
		t.Errorf("gap between windows = %s, want the stretch nobody was watching", gap)
	}

	// Extending a window that is gone says so, so the caller opens a fresh one rather than quietly
	// recording no coverage at all.
	if _, err := s.PruneLedger(ctx, base.Add(3*time.Minute)); err != nil {
		t.Fatalf("PruneLedger: %v", err)
	}
	if err := s.ExtendObservationWindow(ctx, first, base.Add(4*time.Minute), ""); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("extending a pruned window returned %v, want ErrNotFound", err)
	}
}

// TestStoreManagedApps: the intent side of §6 is the apps whose release history shows a rollout that
// reached the cluster — not every app with a release row, because an app whose only deploy failed
// has no workload to be missing.
func TestStoreManagedApps(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	prefix := strings.ToLower(t.Name())
	env := "prod"
	for _, r := range []cp.Release{
		{ID: prefix + "-r1", App: prefix + "-live", Image: "img:1", Environment: env, Status: cp.ReleaseDeployed},
		{ID: prefix + "-r2", App: prefix + "-rolled", Image: "img:1", Environment: env, Status: cp.ReleaseSuperseded},
		{ID: prefix + "-r3", App: prefix + "-neverran", Image: "img:1", Environment: env, Status: cp.ReleaseFailed},
	} {
		if err := s.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease: %v", err)
		}
	}

	apps, err := s.ManagedApps(ctx)
	if err != nil {
		t.Fatalf("ManagedApps: %v", err)
	}
	got := make(map[string]bool, len(apps))
	for _, ref := range apps {
		got[ref.App] = true
	}
	for _, want := range []string{prefix + "-live", prefix + "-rolled"} {
		if !got[want] {
			t.Errorf("ManagedApps omits %q, which rolled out at some point", want)
		}
	}
	if got[prefix+"-neverran"] {
		t.Errorf("ManagedApps includes an app whose only deploy failed; it has no workload to be missing")
	}
}

// TestStorePendingBackups: a pending row older than the caller's cutoff is a candidate for a Job
// that is gone; a fresh one, or one that finished, is not.
func TestStorePendingBackups(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	prefix := strings.ToLower(t.Name())
	base := time.Date(2029, 5, 6, 7, 0, 0, 0, time.UTC)
	for _, b := range []cp.Backup{
		{ID: prefix + "-stale", App: prefix + "-web", Environment: "prod", Status: cp.BackupPending, CreatedAt: base},
		{ID: prefix + "-fresh", App: prefix + "-web", Environment: "prod", Status: cp.BackupPending, CreatedAt: base.Add(time.Hour)},
		{ID: prefix + "-done", App: prefix + "-web", Environment: "prod", Status: cp.BackupCompleted, CreatedAt: base},
	} {
		if err := s.RecordBackup(ctx, b); err != nil {
			t.Fatalf("RecordBackup: %v", err)
		}
	}

	pending, err := s.PendingBackups(ctx, base.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("PendingBackups: %v", err)
	}
	var ids []string
	for _, b := range pending {
		if strings.HasPrefix(b.ID, prefix) {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) != 1 || ids[0] != prefix+"-stale" {
		t.Errorf("PendingBackups returned %v, want only the stale pending row", ids)
	}
}

// TestStoreExposures: the intent behind an expose round-trips, upserts by (app, environment), and is
// removable — the registry side of "an exposure whose Ingress is gone".
func TestStoreExposures(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := strings.ToLower(t.Name()) + "-web"
	at := time.Date(2026, 7, 31, 6, 0, 0, 0, time.UTC)

	if err := s.RecordExposure(ctx, cp.Exposure{App: app, Environment: "prod", Host: "a.example.com", Port: 8080, CreatedAt: at}); err != nil {
		t.Fatalf("RecordExposure: %v", err)
	}
	// Exposing the same app again at a new host replaces the row rather than adding a second.
	if err := s.RecordExposure(ctx, cp.Exposure{App: app, Environment: "prod", Host: "b.example.com", Port: 9000, TLS: true, CreatedAt: at.Add(time.Hour)}); err != nil {
		t.Fatalf("RecordExposure (re-expose): %v", err)
	}

	found := exposureFor(t, s, app)
	if found.Host != "b.example.com" || found.Port != 9000 || !found.TLS {
		t.Errorf("exposure = %+v, want the re-exposed host, port and TLS", found)
	}

	if err := s.DeleteExposure(ctx, app, "prod"); err != nil {
		t.Fatalf("DeleteExposure: %v", err)
	}
	if all, err := s.Exposures(ctx); err != nil {
		t.Fatalf("Exposures: %v", err)
	} else {
		for _, ex := range all {
			if ex.App == app {
				t.Errorf("the exposure survived its deletion: %+v", ex)
			}
		}
	}
	// Removing one that is not recorded is a no-op: an unexpose must succeed either way.
	if err := s.DeleteExposure(ctx, app, "prod"); err != nil {
		t.Errorf("DeleteExposure on an absent row: %v", err)
	}
}

// exposureFor returns the recorded exposure for app, or fails the test.
func exposureFor(t *testing.T, s *postgres.Store, app string) cp.Exposure {
	t.Helper()
	all, err := s.Exposures(context.Background())
	if err != nil {
		t.Fatalf("Exposures: %v", err)
	}
	var found []cp.Exposure
	for _, ex := range all {
		if ex.App == app {
			found = append(found, ex)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d exposures for %q, want exactly 1: %+v", len(found), app, found)
	}
	return found[0]
}

// TestStoreLedgerReadFilters: the filters the cluster-wide listing is built on (ADR-0074 §8) applied
// against a real Postgres. Ordering, the kind/reason/environment narrowing, the last-seen window and
// the row cap all live in one SQL builder, and a wrong clause there produces the one answer this
// surface must never give by accident — a short list that looks complete.
func TestStoreLedgerReadFilters(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	prefix := strings.ToLower(t.Name())
	base := time.Date(2031, 5, 6, 1, 0, 0, 0, time.UTC)

	// One taint's worth of cascade across two kinds, plus an unrelated crash loop that started
	// earlier and a stale row outside the window.
	seed := []struct {
		kind   cp.FailureKind
		name   string
		env    string
		reason string
		at     time.Time
	}{
		{cp.FailureApp, "api", "prod", cp.ReasonUnschedulable, base.Add(30 * time.Minute)},
		{cp.FailureAddon, "postgres", "prod", cp.ReasonUnschedulable, base.Add(31 * time.Minute)},
		{cp.FailureApp, "web", "staging", cp.ReasonUnschedulable, base.Add(32 * time.Minute)},
		{cp.FailureApp, "worker", "prod", cp.ReasonCrashLoopBackOff, base},
		{cp.FailureApp, "ancient", "prod", cp.ReasonOOMKilled, base.Add(-48 * time.Hour)},
	}
	for _, sd := range seed {
		if err := s.RecordFailure(ctx, cp.FailureObservation{
			Object: cp.ObjectRef{Kind: sd.kind, Name: prefix + "-" + sd.name, Environment: sd.env},
			Reason: sd.reason, At: sd.at,
		}); err != nil {
			t.Fatalf("RecordFailure(%s): %v", sd.name, err)
		}
	}

	// Oldest first, because the earliest first_seen in a cascade is the likeliest thing to fix.
	rows := readFailures(t, s, cp.FailureFilter{Since: base.Add(-time.Hour)})
	rows = onlyPrefixed(rows, prefix)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want the four inside the window: %+v", len(rows), rows)
	}
	if rows[0].Object.Name != prefix+"-worker" {
		t.Errorf("first row is %q, want the crash loop that started first", rows[0].Object.Name)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].FirstSeen.Before(rows[i-1].FirstSeen) {
			t.Fatalf("rows are not oldest-first: %+v", rows)
		}
	}

	// The narrowing filters.
	for _, tc := range []struct {
		what   string
		filter cp.FailureFilter
		want   int
	}{
		{"kind", cp.FailureFilter{Kind: cp.FailureAddon, Since: base.Add(-time.Hour)}, 1},
		{"reason", cp.FailureFilter{Reason: cp.ReasonUnschedulable, Since: base.Add(-time.Hour)}, 3},
		{"environment", cp.FailureFilter{Environment: "staging", Since: base.Add(-time.Hour)}, 1},
		{"name", cp.FailureFilter{Name: prefix + "-api"}, 1},
		{"window excludes the stale row", cp.FailureFilter{Reason: cp.ReasonOOMKilled, Since: base.Add(-time.Hour)}, 0},
		{"window includes it when widened", cp.FailureFilter{Reason: cp.ReasonOOMKilled, Since: base.Add(-72 * time.Hour)}, 1},
	} {
		got := onlyPrefixed(readFailures(t, s, tc.filter), prefix)
		if len(got) != tc.want {
			t.Errorf("%s filter returned %d rows, want %d: %+v", tc.what, len(got), tc.want, got)
		}
	}

	// The cap is asserted unfiltered: it bounds the RESPONSE, so it is the one property a
	// prefix-scoped count could not see.
	if capped := readFailures(t, s, cp.FailureFilter{Since: base.Add(-72 * time.Hour), Limit: 2}); len(capped) != 2 {
		t.Errorf("limit 2 returned %d rows, want the response capped at 2", len(capped))
	}

	// A resolved episode leaves the default listing and stays in the history.
	if err := s.ResolveFailures(ctx, base.Add(time.Hour), []cp.FailureKey{
		{Object: cp.ObjectRef{Kind: cp.FailureApp, Name: prefix + "-api", Environment: "prod"}, Reason: cp.ReasonUnschedulable},
	}, nil); err != nil {
		t.Fatalf("ResolveFailures: %v", err)
	}
	active := onlyPrefixed(readFailures(t, s, cp.FailureFilter{Name: prefix + "-worker"}), prefix)
	if len(active) != 0 {
		t.Errorf("the resolved crash loop is still in the default listing: %+v", active)
	}
	history := onlyPrefixed(readFailures(t, s, cp.FailureFilter{Name: prefix + "-worker", IncludeResolved: true}), prefix)
	if len(history) != 1 || history[0].Active() {
		t.Errorf("history = %+v, want the one resolved episode", history)
	}
}

// readFailures runs one ledger query or fails the test.
func readFailures(t *testing.T, s *postgres.Store, filter cp.FailureFilter) []cp.Failure {
	t.Helper()
	rows, err := s.Failures(context.Background(), filter)
	if err != nil {
		t.Fatalf("Failures(%+v): %v", filter, err)
	}
	return rows
}

// onlyPrefixed keeps the rows this test seeded, so it is safe against a shared database.
func onlyPrefixed(rows []cp.Failure, prefix string) []cp.Failure {
	out := make([]cp.Failure, 0, len(rows))
	for _, f := range rows {
		if strings.HasPrefix(f.Object.Name, prefix) {
			out = append(out, f)
		}
	}
	return out
}
