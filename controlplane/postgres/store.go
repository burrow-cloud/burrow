// SPDX-License-Identifier: FSL-1.1-ALv2
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
// managed module can wire it; it is source-available under FSL-1.1-ALv2 (LICENSING.md,
// ADR-0001).
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
