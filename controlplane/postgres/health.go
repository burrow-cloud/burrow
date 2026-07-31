// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The declared health endpoint's store side (ADR-0076 §5): the path an app serves its readiness
// answer on, recorded once and rendered into every subsequent workload apply. It is intent, keyed by
// (app, environment) exactly as an exposure is.

// HealthEndpoint returns the endpoint declared for app in env, or the ZERO VALUE when none was
// declared. A missing row is deliberately not ErrNotFound: no endpoint is the state every app starts
// in (§5 is opt-in), and the deploy path reads this on every apply, so an error there would turn the
// ordinary case into something a caller has to special-case.
func (s *Store) HealthEndpoint(ctx context.Context, app, env string) (controlplane.HealthEndpoint, error) {
	const q = `SELECT app, environment, path, port, updated_at FROM app_health WHERE app = $1 AND environment = $2`
	var ep controlplane.HealthEndpoint
	err := s.db.QueryRowContext(ctx, q, app, env).Scan(&ep.App, &ep.Environment, &ep.Path, &ep.Port, &ep.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.HealthEndpoint{}, nil
	}
	if err != nil {
		return controlplane.HealthEndpoint{}, fmt.Errorf("postgres: health endpoint for %q in %q: %w", app, env, err)
	}
	return ep, nil
}

// SetHealthEndpoint upserts the declared endpoint for an app in one environment.
func (s *Store) SetHealthEndpoint(ctx context.Context, ep controlplane.HealthEndpoint) error {
	if ep.App == "" {
		return fmt.Errorf("postgres: set health endpoint: empty app")
	}
	const q = `
INSERT INTO app_health (app, environment, path, port, updated_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (app, environment) DO UPDATE SET
    path = EXCLUDED.path, port = EXCLUDED.port, updated_at = EXCLUDED.updated_at`
	env := ep.Environment
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	if _, err := s.db.ExecContext(ctx, q, ep.App, env, ep.Path, ep.Port, ep.UpdatedAt); err != nil {
		return fmt.Errorf("postgres: set health endpoint for %q in %q: %w", ep.App, env, err)
	}
	return nil
}

// UnsetHealthEndpoint removes the declared endpoint for app in env, returning it to the conservative
// default. Removing one that was never declared is a no-op: unset must be idempotent, since it is
// what a user reaches for when they are not sure what is set.
func (s *Store) UnsetHealthEndpoint(ctx context.Context, app, env string) error {
	const q = `DELETE FROM app_health WHERE app = $1 AND environment = $2`
	if _, err := s.db.ExecContext(ctx, q, app, env); err != nil {
		return fmt.Errorf("postgres: unset health endpoint for %q in %q: %w", app, env, err)
	}
	return nil
}

// DeleteHealthEndpoints removes every declared endpoint for app across all environments — the
// durable side of an app teardown.
func (s *Store) DeleteHealthEndpoints(ctx context.Context, app string) error {
	const q = `DELETE FROM app_health WHERE app = $1`
	if _, err := s.db.ExecContext(ctx, q, app); err != nil {
		return fmt.Errorf("postgres: delete health endpoints for %q: %w", app, err)
	}
	return nil
}

// Exposure returns the recorded exposure for app in env, or ErrNotFound when the app is not
// published there — the read behind ADR-0076 §3's conservative default, since an exposure's port is
// the only container port Burrow knows for an app.
func (s *Store) Exposure(ctx context.Context, app, env string) (controlplane.Exposure, error) {
	const q = `SELECT app, environment, host, port, tls, created_at FROM exposures WHERE app = $1 AND environment = $2`
	var ex controlplane.Exposure
	err := s.db.QueryRowContext(ctx, q, app, env).Scan(&ex.App, &ex.Environment, &ex.Host, &ex.Port, &ex.TLS, &ex.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.Exposure{}, fmt.Errorf("postgres: exposure for %q in %q: %w", app, env, controlplane.ErrNotFound)
	}
	if err != nil {
		return controlplane.Exposure{}, fmt.Errorf("postgres: exposure for %q in %q: %w", app, env, err)
	}
	return ex, nil
}
