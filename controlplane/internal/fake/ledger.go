// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The in-memory failure ledger (ADR-0074 §4–§6). It matches the Postgres store's semantics closely
// enough that an observer test asserts the same behavior production has: at most one ACTIVE row per
// (object, reason), a resolved row that never revives, and a resolution pass that leaves an
// unreadable object's rows exactly where they are.

// exposureKey joins an app and environment into the map key, mirroring the store's composite
// primary key.
func exposureKey(app, env string) string { return app + "\x00" + env }

// RecordFailure opens or extends the row for one observed failure. A sighting of an already-active
// (object, reason) advances last_seen and the count and leaves first_seen alone; a sighting of one
// that had been resolved opens a NEW row, so an episode that recurs reads as two rather than one.
func (d *Database) RecordFailure(ctx context.Context, obs controlplane.FailureObservation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpRecordFailure]; err != nil {
		return err
	}
	if !obs.Object.Kind.Valid() || obs.Object.Name == "" {
		return fmt.Errorf("database: record failure: invalid object %s", obs.Object)
	}
	if !controlplane.IsLedgerReason(obs.Reason) {
		return fmt.Errorf("database: record failure for %s: reason %q is not in the ledger vocabulary", obs.Object, obs.Reason)
	}
	detail := controlplane.LedgerDetail(obs.Detail)
	for i := range d.failures {
		f := &d.failures[i]
		if f.Active() && f.Object == obs.Object && f.Reason == obs.Reason {
			f.LastSeen = obs.At
			f.Detail = detail
			f.Occurrences++
			return nil
		}
	}
	d.failures = append(d.failures, controlplane.Failure{
		ID:          int64(len(d.failures) + 1),
		Object:      obs.Object,
		Reason:      obs.Reason,
		Detail:      detail,
		FirstSeen:   obs.At,
		LastSeen:    obs.At,
		Occurrences: 1,
	})
	return nil
}

// ResolveFailures closes every active row that is neither still observed (keep) nor belongs to an
// object the sweep could not read (skip).
func (d *Database) ResolveFailures(ctx context.Context, at time.Time, keep []controlplane.FailureKey, skip []controlplane.ObjectRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpResolveFailures]; err != nil {
		return err
	}
	active := make(map[controlplane.FailureKey]bool, len(keep))
	for _, k := range keep {
		active[k] = true
	}
	unread := make(map[controlplane.ObjectRef]bool, len(skip))
	for _, ref := range skip {
		unread[ref] = true
	}
	for i := range d.failures {
		f := &d.failures[i]
		if !f.Active() || unread[f.Object] || active[controlplane.FailureKey{Object: f.Object, Reason: f.Reason}] {
			continue
		}
		t := at
		f.ResolvedAt = &t
	}
	return nil
}

// ResolveFailure closes the one active row identified by key, at the instant the condition actually
// cleared (ADR-0079 §2). Resolving a row that is not active is a no-op, matching the store.
func (d *Database) ResolveFailure(ctx context.Context, at time.Time, key controlplane.FailureKey) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpResolveFailure]; err != nil {
		return err
	}
	for i := range d.failures {
		f := &d.failures[i]
		if !f.Active() || f.Object != key.Object || f.Reason != key.Reason {
			continue
		}
		t := at
		f.ResolvedAt = &t
		return nil
	}
	return nil
}

// Failures returns ledger rows matching filter, oldest first — the order ADR-0074 §5 asks for.
func (d *Database) Failures(ctx context.Context, filter controlplane.FailureFilter) ([]controlplane.Failure, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpFailures]; err != nil {
		return nil, err
	}
	out := make([]controlplane.Failure, 0, len(d.failures))
	for _, f := range d.failures {
		switch {
		case !filter.IncludeResolved && !f.Active():
		case filter.Kind != "" && f.Object.Kind != filter.Kind:
		case filter.Name != "" && f.Object.Name != filter.Name:
		case filter.Environment != "" && f.Object.Environment != filter.Environment:
		case filter.Reason != "" && f.Reason != filter.Reason:
		case !filter.Since.IsZero() && f.LastSeen.Before(filter.Since):
		default:
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].ID < out[j].ID
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// StartObservationWindow opens a window for one run of the observer and returns its id.
func (d *Database) StartObservationWindow(ctx context.Context, at time.Time) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpStartObservationWindow]; err != nil {
		return 0, err
	}
	w := controlplane.ObservationWindow{ID: int64(len(d.windows) + 1), StartedAt: at, Until: at}
	d.windows = append(d.windows, w)
	return w.ID, nil
}

// ExtendObservationWindow advances a window's coverage and counts the sweep, marking it degraded
// when the sweep could not read everything. An unknown id is ErrNotFound, which tells the observer
// to open a fresh window rather than quietly stop recording coverage.
func (d *Database) ExtendObservationWindow(ctx context.Context, id int64, until time.Time, degraded string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpExtendObservationWindow]; err != nil {
		return err
	}
	for i := range d.windows {
		w := &d.windows[i]
		if w.ID != id {
			continue
		}
		w.Until = until
		w.Sweeps++
		if degraded != "" {
			w.DegradedSweeps++
			w.Detail = controlplane.LedgerDetail(degraded)
		}
		return nil
	}
	return fmt.Errorf("database: observation window %d: %w", id, controlplane.ErrNotFound)
}

// ObservationWindows returns the windows whose coverage ends at or after since, oldest first.
func (d *Database) ObservationWindows(ctx context.Context, since time.Time, limit int) ([]controlplane.ObservationWindow, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpObservationWindows]; err != nil {
		return nil, err
	}
	out := make([]controlplane.ObservationWindow, 0, len(d.windows))
	for _, w := range d.windows {
		if !since.IsZero() && w.Until.Before(since) {
			continue
		}
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneLedger removes resolved failures and elapsed windows older than before. An ACTIVE failure
// survives however old it is: a thing that is still broken is not history (ADR-0074 §4).
func (d *Database) PruneLedger(ctx context.Context, before time.Time) (controlplane.LedgerPruneResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpPruneLedger]; err != nil {
		return controlplane.LedgerPruneResult{}, err
	}
	var res controlplane.LedgerPruneResult
	failures := d.failures[:0]
	for _, f := range d.failures {
		if !f.Active() && f.ResolvedAt.Before(before) {
			res.Failures++
			continue
		}
		failures = append(failures, f)
	}
	d.failures = failures
	windows := d.windows[:0]
	for _, w := range d.windows {
		if w.Until.Before(before) {
			res.Windows++
			continue
		}
		windows = append(windows, w)
	}
	d.windows = windows
	return res, nil
}

// ManagedApps returns the (app, environment) pairs whose release history shows a rollout that
// reached the cluster — deployed, or superseded by a later deploy that succeeded. An app whose only
// deploy failed is excluded: it has no workload to be missing (ADR-0074 §6).
func (d *Database) ManagedApps(ctx context.Context) ([]controlplane.AppEnvRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpManagedApps]; err != nil {
		return nil, err
	}
	seen := make(map[controlplane.AppEnvRef]bool)
	for _, r := range d.byID {
		if r.Status != controlplane.ReleaseDeployed && r.Status != controlplane.ReleaseSuperseded {
			continue
		}
		env := r.Environment
		if env == "" {
			env = controlplane.DefaultEnvironment
		}
		seen[controlplane.AppEnvRef{App: r.App, Env: env}] = true
	}
	out := make([]controlplane.AppEnvRef, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Env < out[j].Env
	})
	return out, nil
}

// PendingBackups returns the backups still pending that were recorded before the cutoff, oldest
// first.
func (d *Database) PendingBackups(ctx context.Context, before time.Time) ([]controlplane.Backup, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpPendingBackups]; err != nil {
		return nil, err
	}
	out := make([]controlplane.Backup, 0)
	for _, id := range d.backupSeq {
		b := d.backups[id]
		if b.Status == controlplane.BackupPending && b.CreatedAt.Before(before) {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// RecordExposure upserts the intent behind an expose, keyed by (app, environment).
func (d *Database) RecordExposure(ctx context.Context, ex controlplane.Exposure) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpRecordExposure]; err != nil {
		return err
	}
	if ex.App == "" {
		return fmt.Errorf("database: record exposure: empty app")
	}
	if ex.Environment == "" {
		ex.Environment = controlplane.DefaultEnvironment
	}
	d.exposures[exposureKey(ex.App, ex.Environment)] = ex
	return nil
}

// DeleteExposure removes the recorded exposure for app in env. Removing one that is not recorded is
// a no-op, matching the store.
func (d *Database) DeleteExposure(ctx context.Context, app, env string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDeleteExposure]; err != nil {
		return err
	}
	delete(d.exposures, exposureKey(app, env))
	return nil
}

// Exposures returns every recorded exposure, ordered by app then environment.
func (d *Database) Exposures(ctx context.Context) ([]controlplane.Exposure, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpExposures]; err != nil {
		return nil, err
	}
	out := make([]controlplane.Exposure, 0, len(d.exposures))
	for _, ex := range d.exposures {
		out = append(out, ex)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Environment < out[j].Environment
	})
	return out, nil
}
