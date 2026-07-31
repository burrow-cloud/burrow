// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The store side of the deploy-time dependency check's one stored fact (ADR-0076 §4): whether the
// Burrow-supplied default runs for an app in one environment.
//
// Nothing here stores what is CHECKED. That is derived from what Burrow provisioned on every read,
// so there is no copy of the registry to drift.

// DependencyChecksEnabled reports whether the deploy-time dependency check runs for app in env.
//
// A MISSING ROW IS TRUE. The check is Burrow's default, so a row exists only where somebody decided
// otherwise; this is read on every deploy, and returning ErrNotFound for the ordinary case would make
// the caller special-case the state every app is in.
func (s *Store) DependencyChecksEnabled(ctx context.Context, app, env string) (bool, error) {
	const q = `SELECT enabled FROM app_dependency_checks WHERE app = $1 AND environment = $2`
	var enabled bool
	err := s.db.QueryRowContext(ctx, q, app, env).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: dependency check setting for %q in %q: %w", app, env, err)
	}
	return enabled, nil
}

// SetDependencyChecks records whether the deploy-time dependency check runs for app in env. Enabling
// writes a row rather than deleting one, so "somebody looked at this and left it on" and "nobody has
// ever thought about it" stay distinguishable.
func (s *Store) SetDependencyChecks(ctx context.Context, app, env string, enabled bool, at time.Time) error {
	if app == "" {
		return fmt.Errorf("postgres: set dependency checks: empty app")
	}
	const q = `
INSERT INTO app_dependency_checks (app, environment, enabled, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (app, environment) DO UPDATE SET
    enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at`
	if _, err := s.db.ExecContext(ctx, q, app, env, enabled, at); err != nil {
		return fmt.Errorf("postgres: set dependency checks for %q in %q: %w", app, env, err)
	}
	return nil
}

// DeleteDependencyCheckSettings removes app's recorded setting across all environments — the durable
// side of an app teardown.
func (s *Store) DeleteDependencyCheckSettings(ctx context.Context, app string) error {
	const q = `DELETE FROM app_dependency_checks WHERE app = $1`
	if _, err := s.db.ExecContext(ctx, q, app); err != nil {
		return fmt.Errorf("postgres: delete dependency check settings for %q: %w", app, err)
	}
	return nil
}
