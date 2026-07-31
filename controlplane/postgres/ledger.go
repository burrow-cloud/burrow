// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The failure ledger's store side (ADR-0074 §4–§7). It is a SEPARATE table from audit_log and shares
// nothing with it but the clock and the object names — merging them would subject an append-only
// security record to this file's retention pruning (§7).

// ledgerColumns is the failure_ledger projection in a stable order shared by the scanner and every
// SELECT.
const ledgerColumns = `id, kind, object, environment, reason, detail, first_seen, last_seen, resolved_at, occurrences`

// defaultLedgerLimit caps an unbounded ledger query so a long incident's history never returns in
// one response.
const defaultLedgerLimit = 500

// defaultWindowLimit caps an unbounded coverage query for the same reason.
const defaultWindowLimit = 200

// keySep joins an object reference's parts into the single text key the resolution pass compares
// against. It is the ASCII unit separator: no Kubernetes name, environment name or reason from the
// closed vocabulary can contain it, so two different keys can never collide into one string.
const keySep = "\x1f"

// RecordFailure opens or extends the row for one observed failure (ADR-0074 §4). The upsert targets
// the PARTIAL unique index on active rows, which is what makes the whole shape work: a sighting of a
// failure that is already active advances last_seen and the count and leaves first_seen — the answer
// to "when did it start" — untouched, while a sighting of one that had been RESOLVED conflicts with
// nothing and opens a new row, so a thing that broke, recovered and broke again reads as two
// episodes rather than one long outage.
func (s *Store) RecordFailure(ctx context.Context, obs controlplane.FailureObservation) error {
	if !obs.Object.Kind.Valid() {
		return fmt.Errorf("postgres: record failure: invalid object kind %q", obs.Object.Kind)
	}
	if obs.Object.Name == "" {
		return fmt.Errorf("postgres: record failure: empty object name")
	}
	if !controlplane.IsLedgerReason(obs.Reason) {
		return fmt.Errorf("postgres: record failure for %s: reason %q is not in the ledger vocabulary", obs.Object, obs.Reason)
	}
	const q = `
INSERT INTO failure_ledger (kind, object, environment, reason, detail, first_seen, last_seen, occurrences)
VALUES ($1, $2, $3, $4, $5, $6, $6, 1)
ON CONFLICT (kind, object, environment, reason) WHERE resolved_at IS NULL
DO UPDATE SET last_seen = EXCLUDED.last_seen,
              detail = EXCLUDED.detail,
              occurrences = failure_ledger.occurrences + 1`
	_, err := s.db.ExecContext(ctx, q,
		string(obs.Object.Kind), obs.Object.Name, obs.Object.Environment, obs.Reason,
		controlplane.LedgerDetail(obs.Detail), obs.At)
	if err != nil {
		return fmt.Errorf("postgres: record failure %s on %s: %w", obs.Reason, obs.Object, err)
	}
	return nil
}

// ResolveFailures closes every active row that is neither still observed (keep) nor unreadable
// (skip). Both lists arrive as text keys because Postgres compares an array of composites far less
// readably than an array of joined strings, and the separator cannot occur in any part.
//
// An empty keep list resolves everything not skipped, which is correct and is the normal case once
// a cluster is healthy: nothing observed active means nothing is active.
func (s *Store) ResolveFailures(ctx context.Context, at time.Time, keep []controlplane.FailureKey, skip []controlplane.ObjectRef) error {
	keepKeys := make([]string, 0, len(keep))
	for _, k := range keep {
		keepKeys = append(keepKeys, failureKeyText(k.Object, k.Reason))
	}
	skipKeys := make([]string, 0, len(skip))
	for _, ref := range skip {
		skipKeys = append(skipKeys, objectKeyText(ref))
	}
	const q = `
UPDATE failure_ledger SET resolved_at = $1
WHERE resolved_at IS NULL
  AND NOT (kind || $2 || object || $2 || environment || $2 || reason = ANY($3::text[]))
  AND NOT (kind || $2 || object || $2 || environment = ANY($4::text[]))`
	if _, err := s.db.ExecContext(ctx, q, at, keySep, textArray(keepKeys), textArray(skipKeys)); err != nil {
		return fmt.Errorf("postgres: resolving failures: %w", err)
	}
	return nil
}

// Failures returns ledger rows matching filter, oldest first — the order ADR-0074 §5 asks for,
// because the earliest first_seen in a cascade is the likeliest thing to actually fix. The filter
// clauses are optional and ANDed; the zero filter returns the active rows up to the cap.
func (s *Store) Failures(ctx context.Context, filter controlplane.FailureFilter) ([]controlplane.Failure, error) {
	q := `SELECT ` + ledgerColumns + ` FROM failure_ledger`
	var (
		clauses []string
		args    []any
	)
	add := func(clause string, val any) {
		args = append(args, val)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if !filter.IncludeResolved {
		clauses = append(clauses, "resolved_at IS NULL")
	}
	if filter.Kind != "" {
		add("kind = $%d", string(filter.Kind))
	}
	if filter.Name != "" {
		add("object = $%d", filter.Name)
	}
	if filter.Environment != "" {
		add("environment = $%d", filter.Environment)
	}
	if filter.Reason != "" {
		add("reason = $%d", filter.Reason)
	}
	if !filter.Since.IsZero() {
		add("last_seen >= $%d", filter.Since)
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultLedgerLimit
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY first_seen ASC, id ASC LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: failures: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.Failure, 0)
	for rows.Next() {
		f, err := scanFailure(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: failures: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: failures: %w", err)
	}
	return out, nil
}

func scanFailure(sc scanner) (controlplane.Failure, error) {
	var (
		f        controlplane.Failure
		kind     string
		resolved sql.NullTime
	)
	if err := sc.Scan(&f.ID, &kind, &f.Object.Name, &f.Object.Environment, &f.Reason, &f.Detail,
		&f.FirstSeen, &f.LastSeen, &resolved, &f.Occurrences); err != nil {
		return controlplane.Failure{}, err
	}
	f.Object.Kind = controlplane.FailureKind(kind)
	if resolved.Valid {
		t := resolved.Time
		f.ResolvedAt = &t
	}
	return f, nil
}

// StartObservationWindow opens a window for one run of the observer and returns its id.
func (s *Store) StartObservationWindow(ctx context.Context, at time.Time) (int64, error) {
	const q = `INSERT INTO observation_windows (started_at, until) VALUES ($1, $1) RETURNING id`
	var id int64
	if err := s.db.QueryRowContext(ctx, q, at).Scan(&id); err != nil {
		return 0, fmt.Errorf("postgres: starting observation window: %w", err)
	}
	return id, nil
}

// ExtendObservationWindow advances a window's coverage and counts the sweep. A degraded note counts
// the sweep as incomplete and replaces the window's most recent note; a clean sweep leaves any
// earlier note in place, because the window is still one that had a degraded stretch in it.
//
// Extending a window that no longer exists is ErrNotFound rather than a silent no-op: the caller's
// correct response is to open a fresh window, and a no-op here would have it record no coverage at
// all while believing it was.
func (s *Store) ExtendObservationWindow(ctx context.Context, id int64, until time.Time, degraded string) error {
	const q = `
UPDATE observation_windows
   SET until = $2,
       sweeps = sweeps + 1,
       degraded_sweeps = degraded_sweeps + CASE WHEN $3 = '' THEN 0 ELSE 1 END,
       detail = CASE WHEN $3 = '' THEN detail ELSE $3 END
 WHERE id = $1`
	res, err := s.db.ExecContext(ctx, q, id, until, controlplane.LedgerDetail(degraded))
	if err != nil {
		return fmt.Errorf("postgres: extending observation window %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: extending observation window %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("postgres: observation window %d: %w", id, controlplane.ErrNotFound)
	}
	return nil
}

// ObservationWindows returns the windows whose coverage ends at or after since, oldest first. The
// gaps between them are what the caller is actually reading.
func (s *Store) ObservationWindows(ctx context.Context, since time.Time, limit int) ([]controlplane.ObservationWindow, error) {
	q := `SELECT id, started_at, until, sweeps, degraded_sweeps, detail FROM observation_windows`
	var args []any
	if !since.IsZero() {
		args = append(args, since)
		q += " WHERE until >= $1"
	}
	if limit <= 0 {
		limit = defaultWindowLimit
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY started_at ASC, id ASC LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: observation windows: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.ObservationWindow, 0)
	for rows.Next() {
		var w controlplane.ObservationWindow
		if err := rows.Scan(&w.ID, &w.StartedAt, &w.Until, &w.Sweeps, &w.DegradedSweeps, &w.Detail); err != nil {
			return nil, fmt.Errorf("postgres: observation windows: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: observation windows: %w", err)
	}
	return out, nil
}

// PruneLedger enforces the retention bound (ADR-0074 §4). It removes RESOLVED failures and elapsed
// coverage windows; an active failure survives however old it is, because a thing that is still
// broken is not history. Both deletes run in one transaction so a partial prune cannot leave the
// ledger and its coverage record describing different periods.
func (s *Store) PruneLedger(ctx context.Context, before time.Time) (controlplane.LedgerPruneResult, error) {
	var res controlplane.LedgerPruneResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("postgres: pruning the ledger: %w", err)
	}
	// A rollback after a successful commit is a no-op, so this is the plain cleanup path.
	defer func() { _ = tx.Rollback() }()

	failures, err := tx.ExecContext(ctx, `DELETE FROM failure_ledger WHERE resolved_at IS NOT NULL AND resolved_at < $1`, before)
	if err != nil {
		return res, fmt.Errorf("postgres: pruning resolved failures: %w", err)
	}
	windows, err := tx.ExecContext(ctx, `DELETE FROM observation_windows WHERE until < $1`, before)
	if err != nil {
		return res, fmt.Errorf("postgres: pruning observation windows: %w", err)
	}
	if res.Failures, err = failures.RowsAffected(); err != nil {
		return controlplane.LedgerPruneResult{}, fmt.Errorf("postgres: pruning the ledger: %w", err)
	}
	if res.Windows, err = windows.RowsAffected(); err != nil {
		return controlplane.LedgerPruneResult{}, fmt.Errorf("postgres: pruning the ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return controlplane.LedgerPruneResult{}, fmt.Errorf("postgres: pruning the ledger: %w", err)
	}
	return res, nil
}

// ManagedApps returns the (app, environment) pairs whose release history shows a rollout that
// reached the cluster — a release that was deployed, or one superseded by a later deploy, which only
// happens after the later one succeeded. An app whose only deploy failed before any workload was
// applied is deliberately excluded: it has no Deployment to be missing, and reporting one would be a
// false positive on the day someone is already debugging a failed deploy (ADR-0074 §6).
func (s *Store) ManagedApps(ctx context.Context) ([]controlplane.AppEnvRef, error) {
	const q = `
SELECT DISTINCT app, environment FROM releases
 WHERE status IN ($1, $2)
 ORDER BY app, environment`
	rows, err := s.db.QueryContext(ctx, q, string(controlplane.ReleaseDeployed), string(controlplane.ReleaseSuperseded))
	if err != nil {
		return nil, fmt.Errorf("postgres: managed apps: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.AppEnvRef, 0)
	for rows.Next() {
		var ref controlplane.AppEnvRef
		if err := rows.Scan(&ref.App, &ref.Env); err != nil {
			return nil, fmt.Errorf("postgres: managed apps: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: managed apps: %w", err)
	}
	return out, nil
}

// PendingBackups returns the backups still `pending` that were recorded before the cutoff, oldest
// first — the candidates for a Job that is gone (ADR-0074 §6). The cutoff is the caller's: it is
// what separates a backup still running from one nothing is going to finish.
func (s *Store) PendingBackups(ctx context.Context, before time.Time) ([]controlplane.Backup, error) {
	const q = `SELECT ` + backupColumns + ` FROM postgres_backups
 WHERE status = $1 AND created_at < $2
 ORDER BY created_at ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q, string(controlplane.BackupPending), before)
	if err != nil {
		return nil, fmt.Errorf("postgres: pending backups: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.Backup, 0)
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: pending backups: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: pending backups: %w", err)
	}
	return out, nil
}

// RecordExposure upserts the intent behind an expose, keyed by (app, environment) — what was asked
// for, never whether it currently works (ADR-0074 §6).
func (s *Store) RecordExposure(ctx context.Context, ex controlplane.Exposure) error {
	if ex.App == "" {
		return fmt.Errorf("postgres: record exposure: empty app")
	}
	const q = `
INSERT INTO exposures (app, environment, host, port, tls, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (app, environment) DO UPDATE SET
    host = EXCLUDED.host, port = EXCLUDED.port, tls = EXCLUDED.tls, created_at = EXCLUDED.created_at`
	env := ex.Environment
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	if _, err := s.db.ExecContext(ctx, q, ex.App, env, ex.Host, ex.Port, ex.TLS, ex.CreatedAt); err != nil {
		return fmt.Errorf("postgres: record exposure for %q in %q: %w", ex.App, env, err)
	}
	return nil
}

// DeleteExposure removes the recorded exposure for app in env. Removing one that is not recorded is
// a no-op: an unexpose must succeed whether or not the intent row survived.
func (s *Store) DeleteExposure(ctx context.Context, app, env string) error {
	const q = `DELETE FROM exposures WHERE app = $1 AND environment = $2`
	if _, err := s.db.ExecContext(ctx, q, app, env); err != nil {
		return fmt.Errorf("postgres: delete exposure for %q in %q: %w", app, env, err)
	}
	return nil
}

// Exposures returns every recorded exposure, ordered by app then environment.
func (s *Store) Exposures(ctx context.Context) ([]controlplane.Exposure, error) {
	const q = `SELECT app, environment, host, port, tls, created_at FROM exposures ORDER BY app, environment`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres: exposures: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.Exposure, 0)
	for rows.Next() {
		var ex controlplane.Exposure
		if err := rows.Scan(&ex.App, &ex.Environment, &ex.Host, &ex.Port, &ex.TLS, &ex.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: exposures: %w", err)
		}
		out = append(out, ex)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: exposures: %w", err)
	}
	return out, nil
}

// failureKeyText renders one ledger row's identity as the text key ResolveFailures compares against.
func failureKeyText(ref controlplane.ObjectRef, reason string) string {
	return objectKeyText(ref) + keySep + reason
}

// objectKeyText renders one object's identity as a text key.
func objectKeyText(ref controlplane.ObjectRef) string {
	return string(ref.Kind) + keySep + ref.Name + keySep + ref.Environment
}

// textArray renders a Go string slice as a Postgres text[] literal, so it can be passed as one
// parameter to = ANY(...). The driver has no generic array encoder through database/sql, and the
// alternative — expanding N placeholders into the statement — would produce a different query plan
// for every sweep. Backslashes and quotes are escaped; a nil or empty slice yields an empty array,
// which ANY correctly matches nothing against.
func textArray(vals []string) string {
	if len(vals) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
