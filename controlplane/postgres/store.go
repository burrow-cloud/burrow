// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package postgres is the production controlplane.Database adapter, backed by Postgres
// running in the cluster (ADR-0012). It implements the deploy-record seam (ADR-0007):
// the durable history of releases and the rollback handles, independent of cluster
// state. Schema changes are applied by embedded goose migrations on startup, gated to
// single-minor-step upgrades (ADR-0013).
//
// It uses the pgx driver through the standard database/sql interface (not pgxpool, not
// the maintenance-mode lib/pq), so one *sql.DB serves both the migrations and the app.
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the
// managed module can wire it; it is licensed Apache-2.0 (LICENSING.md,
// ADR-0033).
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Database = (*Store)(nil)

const columns = `id, app, image, digest, env, command, metrics_port, replicas, status, supersedes, created_at`

// Store is a Postgres-backed controlplane.Database.
type Store struct {
	db *sql.DB
}

// Open connects to the database at dsn and verifies the connection. The caller closes
// the Store with Close. It does not apply the schema; call Migrate for that.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: opening: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database connections.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) SaveRelease(ctx context.Context, r controlplane.Release) error {
	if r.ID == "" {
		return fmt.Errorf("postgres: save release: empty ID")
	}
	env := r.Env
	if env == nil {
		env = map[string]string{}
	}
	cmd := r.Command
	if cmd == nil {
		cmd = []string{}
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("postgres: save release %s: encoding env: %w", r.ID, err)
	}
	cmdJSON, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("postgres: save release %s: encoding command: %w", r.ID, err)
	}

	// env/command are cast to jsonb explicitly so the JSON is passed as text and
	// stored as jsonb regardless of how the driver encodes a parameter.
	const q = `
INSERT INTO releases (` + columns + `)
VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9, $10, $11)
ON CONFLICT (id) DO UPDATE SET
    app = EXCLUDED.app, image = EXCLUDED.image, digest = EXCLUDED.digest,
    env = EXCLUDED.env, command = EXCLUDED.command, metrics_port = EXCLUDED.metrics_port,
    replicas = EXCLUDED.replicas, status = EXCLUDED.status, supersedes = EXCLUDED.supersedes,
    created_at = EXCLUDED.created_at`

	_, err = s.db.ExecContext(ctx, q,
		r.ID, r.App, r.Image, r.Digest, string(envJSON), string(cmdJSON), r.MetricsPort, r.Replicas, string(r.Status), r.Supersedes, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: save release %s: %w", r.ID, err)
	}
	return nil
}

func (s *Store) Release(ctx context.Context, id string) (controlplane.Release, error) {
	const q = `SELECT ` + columns + ` FROM releases WHERE id = $1`
	r, err := scanRelease(s.db.QueryRowContext(ctx, q, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return controlplane.Release{}, fmt.Errorf("postgres: release %q: %w", id, controlplane.ErrNotFound)
		}
		return controlplane.Release{}, fmt.Errorf("postgres: release %q: %w", id, err)
	}
	return r, nil
}

func (s *Store) LatestRelease(ctx context.Context, app string) (controlplane.Release, error) {
	const q = `SELECT ` + columns + ` FROM releases WHERE app = $1 ORDER BY seq DESC LIMIT 1`
	r, err := scanRelease(s.db.QueryRowContext(ctx, q, app))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return controlplane.Release{}, fmt.Errorf("postgres: latest release for app %q: %w", app, controlplane.ErrNotFound)
		}
		return controlplane.Release{}, fmt.Errorf("postgres: latest release for app %q: %w", app, err)
	}
	return r, nil
}

func (s *Store) Releases(ctx context.Context, app string) ([]controlplane.Release, error) {
	const q = `SELECT ` + columns + ` FROM releases WHERE app = $1 ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q, app)
	if err != nil {
		return nil, fmt.Errorf("postgres: releases for app %q: %w", app, err)
	}
	defer rows.Close()

	out := make([]controlplane.Release, 0)
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: releases for app %q: %w", app, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: releases for app %q: %w", app, err)
	}
	return out, nil
}

// DeleteReleases removes every release record for app. Deleting the releases of an app that
// has none is a no-op (no RowsAffected check) — absence is fine.
func (s *Store) DeleteReleases(ctx context.Context, app string) error {
	const q = `DELETE FROM releases WHERE app = $1`
	if _, err := s.db.ExecContext(ctx, q, app); err != nil {
		return fmt.Errorf("postgres: delete releases for app %q: %w", app, err)
	}
	return nil
}

// AppEnv returns the non-secret environment store for app (ADR-0028). An app with no env
// yields an empty map and no error.
func (s *Store) AppEnv(ctx context.Context, app string) (map[string]string, error) {
	const q = `SELECT key, value FROM app_env WHERE app = $1`
	rows, err := s.db.QueryContext(ctx, q, app)
	if err != nil {
		return nil, fmt.Errorf("postgres: app env for %q: %w", app, err)
	}
	defer rows.Close()

	env := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("postgres: app env for %q: %w", app, err)
		}
		env[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: app env for %q: %w", app, err)
	}
	return env, nil
}

// SetAppEnv upserts one env key for app.
func (s *Store) SetAppEnv(ctx context.Context, app, key, value string) error {
	const q = `
INSERT INTO app_env (app, key, value) VALUES ($1, $2, $3)
ON CONFLICT (app, key) DO UPDATE SET value = EXCLUDED.value`
	if _, err := s.db.ExecContext(ctx, q, app, key, value); err != nil {
		return fmt.Errorf("postgres: set app env %q for %q: %w", key, app, err)
	}
	return nil
}

// UnsetAppEnv removes one env key for app. Removing a key that is not set is a no-op.
func (s *Store) UnsetAppEnv(ctx context.Context, app, key string) error {
	const q = `DELETE FROM app_env WHERE app = $1 AND key = $2`
	if _, err := s.db.ExecContext(ctx, q, app, key); err != nil {
		return fmt.Errorf("postgres: unset app env %q for %q: %w", key, app, err)
	}
	return nil
}

// Policy returns the current guardrail policy: the built-in defaults with any stored
// guardrail dispositions overlaid (ADR-0020). An empty table yields DefaultPolicy.
func (s *Store) Policy(ctx context.Context) (controlplane.Policy, error) {
	const q = `SELECT code, disposition FROM guardrail_policy`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return controlplane.Policy{}, fmt.Errorf("postgres: policy: %w", err)
	}
	defer rows.Close()

	p := controlplane.DefaultPolicy()
	for rows.Next() {
		var code, disp string
		if err := rows.Scan(&code, &disp); err != nil {
			return controlplane.Policy{}, fmt.Errorf("postgres: policy: %w", err)
		}
		p = p.With(controlplane.GuardrailCode(code), controlplane.Disposition(disp))
	}
	if err := rows.Err(); err != nil {
		return controlplane.Policy{}, fmt.Errorf("postgres: policy: %w", err)
	}
	return p, nil
}

// SetGuardrail upserts one guardrail's disposition.
func (s *Store) SetGuardrail(ctx context.Context, code controlplane.GuardrailCode, disp controlplane.Disposition) error {
	if !disp.Valid() {
		return fmt.Errorf("postgres: set guardrail %q: invalid disposition %q", code, disp)
	}
	const q = `
INSERT INTO guardrail_policy (code, disposition) VALUES ($1, $2)
ON CONFLICT (code) DO UPDATE SET disposition = EXCLUDED.disposition`
	if _, err := s.db.ExecContext(ctx, q, string(code), string(disp)); err != nil {
		return fmt.Errorf("postgres: set guardrail %q: %w", code, err)
	}
	return nil
}

// auditColumns is the audit_log projection in a stable order shared by the scanner.
const auditColumns = `id, ts, operation, target, args, guardrail_code, disposition, outcome, result, caller, principal, client_version`

// defaultAuditLimit caps an unbounded audit query so a huge log never returns in one response.
const defaultAuditLimit = 200

// AppendAudit appends one audit row (ADR-0027). The log is append-only: there is only this
// INSERT and the Audit SELECT — no update or delete path. The store assigns id.
func (s *Store) AppendAudit(ctx context.Context, e controlplane.AuditEntry) error {
	args := e.Args
	if args == nil {
		args = map[string]string{}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("postgres: append audit: encoding args: %w", err)
	}
	const q = `
INSERT INTO audit_log (ts, operation, target, args, guardrail_code, disposition, outcome, result, caller, principal, client_version)
VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9, $10, $11)`
	_, err = s.db.ExecContext(ctx, q,
		e.Timestamp, e.Operation, e.Target, string(argsJSON), e.GuardrailCode, e.Disposition, string(e.Outcome), e.Result, e.Caller, e.Principal, e.ClientVersion)
	if err != nil {
		return fmt.Errorf("postgres: append audit: %w", err)
	}
	return nil
}

// Audit returns audit rows matching filter, newest first, capped by filter.Limit (a default
// when unset). The filter clauses are optional and ANDed; an empty filter returns the latest
// rows up to the cap.
func (s *Store) Audit(ctx context.Context, filter controlplane.AuditFilter) ([]controlplane.AuditEntry, error) {
	q := `SELECT ` + auditColumns + ` FROM audit_log`
	var clauses []string
	var args []any
	add := func(col, val string) {
		args = append(args, val)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if filter.App != "" {
		add("target", filter.App)
	}
	if filter.Operation != "" {
		add("operation", filter.Operation)
	}
	if filter.Outcome != "" {
		add("outcome", string(filter.Outcome))
	}
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.AuditEntry, 0)
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: audit: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: audit: %w", err)
	}
	return out, nil
}

func scanAudit(sc scanner) (controlplane.AuditEntry, error) {
	var (
		e        controlplane.AuditEntry
		ts       time.Time
		argsJSON []byte
		outcome  string
	)
	if err := sc.Scan(&e.ID, &ts, &e.Operation, &e.Target, &argsJSON, &e.GuardrailCode, &e.Disposition, &outcome, &e.Result, &e.Caller, &e.Principal, &e.ClientVersion); err != nil {
		return controlplane.AuditEntry{}, err
	}
	if err := json.Unmarshal(argsJSON, &e.Args); err != nil {
		return controlplane.AuditEntry{}, fmt.Errorf("decoding args: %w", err)
	}
	e.Timestamp = ts
	e.Outcome = controlplane.AuditOutcome(outcome)
	return e, nil
}

// CreateEnvironment registers a named environment mapping name to namespace (ADR-0035 phase 2).
// The name is the primary key, so a duplicate is rejected: the INSERT ... ON CONFLICT DO NOTHING
// affects no rows, which is reported as an ErrInvalid-wrapped duplicate error.
func (s *Store) CreateEnvironment(ctx context.Context, name, namespace string) error {
	const q = `INSERT INTO environments (name, namespace) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`
	res, err := s.db.ExecContext(ctx, q, name, namespace)
	if err != nil {
		return fmt.Errorf("postgres: create environment %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: create environment %q: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("postgres: environment %q already exists: %w", name, controlplane.ErrInvalid)
	}
	return nil
}

// ListEnvironments returns the registered environments ordered by name (ADR-0035 phase 2). The
// synthesized `default` environment is not stored here; the engine prepends it.
func (s *Store) ListEnvironments(ctx context.Context) ([]controlplane.Environment, error) {
	const q = `SELECT name, namespace, created_at FROM environments ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres: list environments: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.Environment, 0)
	for rows.Next() {
		var e controlplane.Environment
		if err := rows.Scan(&e.Name, &e.Namespace, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: list environments: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list environments: %w", err)
	}
	return out, nil
}

// GetEnvironment returns the registered environment with the given name, or ErrNotFound.
func (s *Store) GetEnvironment(ctx context.Context, name string) (controlplane.Environment, error) {
	const q = `SELECT name, namespace, created_at FROM environments WHERE name = $1`
	var e controlplane.Environment
	err := s.db.QueryRowContext(ctx, q, name).Scan(&e.Name, &e.Namespace, &e.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return controlplane.Environment{}, fmt.Errorf("postgres: environment %q: %w", name, controlplane.ErrNotFound)
		}
		return controlplane.Environment{}, fmt.Errorf("postgres: environment %q: %w", name, err)
	}
	return e, nil
}

// scanner is the read side common to *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanRelease(sc scanner) (controlplane.Release, error) {
	var (
		r       controlplane.Release
		envJSON []byte
		cmdJSON []byte
		status  string
		created time.Time
	)
	if err := sc.Scan(&r.ID, &r.App, &r.Image, &r.Digest, &envJSON, &cmdJSON, &r.MetricsPort, &r.Replicas, &status, &r.Supersedes, &created); err != nil {
		return controlplane.Release{}, err
	}
	if err := json.Unmarshal(envJSON, &r.Env); err != nil {
		return controlplane.Release{}, fmt.Errorf("decoding env: %w", err)
	}
	if err := json.Unmarshal(cmdJSON, &r.Command); err != nil {
		return controlplane.Release{}, fmt.Errorf("decoding command: %w", err)
	}
	r.Status = controlplane.ReleaseStatus(status)
	r.CreatedAt = created
	return r, nil
}
