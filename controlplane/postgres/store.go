// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package postgres is the production controlplane.Database adapter, backed by Postgres
// running in the cluster (ADR-0012) via pgx. It implements the deploy-record seam
// (ADR-0007): the durable history
// of releases and the rollback handles, independent of cluster state. It lives under
// controlplane/ (not controlplane/internal) so cmd/burrowd and the managed module can
// wire it; it is source-available under FSL-1.1-ALv2 (LICENSING.md, ADR-0001).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Database = (*Store)(nil)

// schema is applied by Migrate. Releases carry a monotonic seq for deterministic
// ordering (the fake's "save order"); id is unique so SaveRelease can upsert status
// transitions without changing a release's position.
const schema = `
CREATE TABLE IF NOT EXISTS releases (
    seq        BIGSERIAL   PRIMARY KEY,
    id         TEXT        NOT NULL UNIQUE,
    app        TEXT        NOT NULL,
    image      TEXT        NOT NULL,
    digest     TEXT        NOT NULL DEFAULT '',
    env        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    command    JSONB       NOT NULL DEFAULT '[]'::jsonb,
    replicas   INTEGER     NOT NULL DEFAULT 0,
    status     TEXT        NOT NULL DEFAULT '',
    supersedes TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS releases_app_seq_idx ON releases (app, seq);
`

const columns = `id, app, image, digest, env, command, replicas, status, supersedes, created_at`

// Store is a Postgres-backed controlplane.Database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database at dsn and verifies the connection. The caller closes
// the Store with Close. It does not apply the schema; call Migrate for that.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connecting: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Migrate applies the schema. It is idempotent (CREATE TABLE IF NOT EXISTS) and safe to
// call on every startup.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}
	return nil
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

	const q = `
INSERT INTO releases (` + columns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
    app = EXCLUDED.app, image = EXCLUDED.image, digest = EXCLUDED.digest,
    env = EXCLUDED.env, command = EXCLUDED.command, replicas = EXCLUDED.replicas,
    status = EXCLUDED.status, supersedes = EXCLUDED.supersedes, created_at = EXCLUDED.created_at`

	_, err = s.pool.Exec(ctx, q,
		r.ID, r.App, r.Image, r.Digest, envJSON, cmdJSON, r.Replicas, string(r.Status), r.Supersedes, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: save release %s: %w", r.ID, err)
	}
	return nil
}

func (s *Store) Release(ctx context.Context, id string) (controlplane.Release, error) {
	const q = `SELECT ` + columns + ` FROM releases WHERE id = $1`
	r, err := scanRelease(s.pool.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return controlplane.Release{}, fmt.Errorf("postgres: release %q: %w", id, controlplane.ErrNotFound)
		}
		return controlplane.Release{}, fmt.Errorf("postgres: release %q: %w", id, err)
	}
	return r, nil
}

func (s *Store) LatestRelease(ctx context.Context, app string) (controlplane.Release, error) {
	const q = `SELECT ` + columns + ` FROM releases WHERE app = $1 ORDER BY seq DESC LIMIT 1`
	r, err := scanRelease(s.pool.QueryRow(ctx, q, app))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return controlplane.Release{}, fmt.Errorf("postgres: latest release for app %q: %w", app, controlplane.ErrNotFound)
		}
		return controlplane.Release{}, fmt.Errorf("postgres: latest release for app %q: %w", app, err)
	}
	return r, nil
}

func (s *Store) Releases(ctx context.Context, app string) ([]controlplane.Release, error) {
	const q = `SELECT ` + columns + ` FROM releases WHERE app = $1 ORDER BY seq ASC`
	rows, err := s.pool.Query(ctx, q, app)
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

// row is the read side of both QueryRow and Rows.
type row interface {
	Scan(dest ...any) error
}

func scanRelease(rw row) (controlplane.Release, error) {
	var (
		r       controlplane.Release
		envJSON []byte
		cmdJSON []byte
		status  string
		created time.Time
	)
	if err := rw.Scan(&r.ID, &r.App, &r.Image, &r.Digest, &envJSON, &cmdJSON, &r.Replicas, &status, &r.Supersedes, &created); err != nil {
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
