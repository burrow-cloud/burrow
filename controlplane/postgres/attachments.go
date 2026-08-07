// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The store side of an attachment's one stored fact (ADR-0031, issue #462): the NAME of the
// environment variable the connection string was written under.
//
// Nothing here stores whether an app is attached. That is derived from the instance's own database
// listing on every read, so there is no copy of the attachment to drift; this row says only what the
// variable is called if there is one.

// AddonEnvKey returns the environment variable name app's attachment to addon in env was written
// under.
//
// A MISSING ROW IS DATABASE_URL. Every attachment made before the name was a choice was written
// there, so the ordinary case has no row, and returning ErrNotFound would make every caller —
// detach, the dependency derivation, the restore cutover — special-case the state most apps are in.
func (s *Store) AddonEnvKey(ctx context.Context, addon, app, env string) (string, error) {
	const q = `SELECT env_key FROM addon_attachments WHERE addon = $1 AND app = $2 AND environment = $3`
	var key string
	err := s.db.QueryRowContext(ctx, q, addon, app, env).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.AppDatabaseURLKey, nil
	}
	if err != nil {
		return "", fmt.Errorf("postgres: attachment key for %q in %q: %w", app, env, err)
	}
	if key == "" {
		return controlplane.AppDatabaseURLKey, nil
	}
	return key, nil
}

// SetAddonEnvKey records the variable name app's attachment to addon in env is written under. It is
// written by the attach that wrote the value, after the write succeeded, so the recorded name is
// only ever a name the Secret actually holds.
func (s *Store) SetAddonEnvKey(ctx context.Context, addon, app, env, key string, at time.Time) error {
	if app == "" {
		return fmt.Errorf("postgres: set attachment key: empty app")
	}
	if key == "" {
		return fmt.Errorf("postgres: set attachment key for %q: empty key", app)
	}
	const q = `
INSERT INTO addon_attachments (addon, app, environment, env_key, updated_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (addon, app, environment) DO UPDATE SET
    env_key = EXCLUDED.env_key, updated_at = EXCLUDED.updated_at`
	if _, err := s.db.ExecContext(ctx, q, addon, app, env, key, at); err != nil {
		return fmt.Errorf("postgres: set attachment key for %q in %q: %w", app, env, err)
	}
	return nil
}

// DeleteAddonEnvKey forgets the recorded name for app's attachment to addon in env — what a detach
// does once it has removed the variable. Deleting a row that was never written is a no-op, which is
// the ordinary case for an attachment that used the default.
func (s *Store) DeleteAddonEnvKey(ctx context.Context, addon, app, env string) error {
	const q = `DELETE FROM addon_attachments WHERE addon = $1 AND app = $2 AND environment = $3`
	if _, err := s.db.ExecContext(ctx, q, addon, app, env); err != nil {
		return fmt.Errorf("postgres: delete attachment key for %q in %q: %w", app, env, err)
	}
	return nil
}

// DeleteAppAttachments forgets every recorded name for app across add-ons and environments — the
// durable side of an app teardown, alongside DeleteHealthEndpoints.
func (s *Store) DeleteAppAttachments(ctx context.Context, app string) error {
	const q = `DELETE FROM addon_attachments WHERE app = $1`
	if _, err := s.db.ExecContext(ctx, q, app); err != nil {
		return fmt.Errorf("postgres: delete attachment keys for %q: %w", app, err)
	}
	return nil
}
