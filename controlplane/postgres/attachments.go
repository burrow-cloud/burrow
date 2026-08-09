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
//
// AN ATTACHMENT IS AGAINST ONE INSTANCE (ADR-0091 §3). An app may hold several in one environment,
// so the instance is part of the key — without it the second attach would rename the first one's
// variable instead of adding its own.

// AddonEnvKey returns the environment variable name app's attachment to addon's instance in env was
// written under, and whether a row was found at all.
//
// THE SECOND RETURN IS LOAD-BEARING, and it is why this does not simply default. A missing row means
// DATABASE_URL for an attachment made before the name was a choice (migration 00029) — but only for
// the environment's DEFAULT instance, which is the only instance those attachments could have been
// against. For any other instance a missing row means there is no attachment, and defaulting there
// would tell an attach it already owns `DATABASE_URL` and let it overwrite another instance's
// connection string. The engine holds that distinction, because it is the layer that knows which
// instance is the environment's default.
func (s *Store) AddonEnvKey(ctx context.Context, addon, app, env, instance string) (string, bool, error) {
	const q = `SELECT env_key FROM addon_attachments WHERE addon = $1 AND app = $2 AND environment = $3 AND instance = $4`
	var key string
	err := s.db.QueryRowContext(ctx, q, addon, app, env, instance).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("postgres: attachment key for %q on %q in %q: %w", app, instance, env, err)
	}
	if key == "" {
		return "", false, nil
	}
	return key, true, nil
}

// SetAddonEnvKey records the variable name app's attachment to addon's instance in env is written
// under. It is written by the attach that wrote the value, after the write succeeded, so the recorded
// name is only ever a name the Secret actually holds.
func (s *Store) SetAddonEnvKey(ctx context.Context, addon, app, env, instance, key string, at time.Time) error {
	if app == "" {
		return fmt.Errorf("postgres: set attachment key: empty app")
	}
	if instance == "" {
		return fmt.Errorf("postgres: set attachment key for %q: empty instance", app)
	}
	if key == "" {
		return fmt.Errorf("postgres: set attachment key for %q: empty key", app)
	}
	const q = `
INSERT INTO addon_attachments (addon, app, environment, instance, env_key, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (addon, app, environment, instance) DO UPDATE SET
    env_key = EXCLUDED.env_key, updated_at = EXCLUDED.updated_at`
	if _, err := s.db.ExecContext(ctx, q, addon, app, env, instance, key, at); err != nil {
		return fmt.Errorf("postgres: set attachment key for %q on %q in %q: %w", app, instance, env, err)
	}
	return nil
}

// DeleteAddonEnvKey forgets the recorded name for app's attachment to addon's instance in env — what
// a detach does once it has removed the variable. Deleting a row that was never written is a no-op,
// which is the ordinary case for an attachment that used the default.
func (s *Store) DeleteAddonEnvKey(ctx context.Context, addon, app, env, instance string) error {
	const q = `DELETE FROM addon_attachments WHERE addon = $1 AND app = $2 AND environment = $3 AND instance = $4`
	if _, err := s.db.ExecContext(ctx, q, addon, app, env, instance); err != nil {
		return fmt.Errorf("postgres: delete attachment key for %q on %q in %q: %w", app, instance, env, err)
	}
	return nil
}

// AppAttachments returns every recorded attachment app holds to addon in env, instance order — the
// listing an operation that has to act on ALL of an app's databases reads (a teardown, or a report of
// what an app is wired to). An app with none, or with only attachments that predate the record, yields
// an empty slice and no error.
func (s *Store) AppAttachments(ctx context.Context, addon, app, env string) ([]controlplane.AddonAttachment, error) {
	const q = `SELECT instance, env_key FROM addon_attachments WHERE addon = $1 AND app = $2 AND environment = $3 ORDER BY instance`
	rows, err := s.db.QueryContext(ctx, q, addon, app, env)
	if err != nil {
		return nil, fmt.Errorf("postgres: attachments for %q in %q: %w", app, env, err)
	}
	defer rows.Close()

	out := []controlplane.AddonAttachment{}
	for rows.Next() {
		a := controlplane.AddonAttachment{Addon: controlplane.AddonType(addon), App: app, Environment: env}
		if err := rows.Scan(&a.Instance, &a.SecretKey); err != nil {
			return nil, fmt.Errorf("postgres: attachments for %q in %q: %w", app, env, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: attachments for %q in %q: %w", app, env, err)
	}
	return out, nil
}

// DeleteAppAttachments forgets every recorded name for app across add-ons, environments and
// instances — the durable side of an app teardown, alongside DeleteHealthEndpoints.
func (s *Store) DeleteAppAttachments(ctx context.Context, app string) error {
	const q = `DELETE FROM addon_attachments WHERE app = $1`
	if _, err := s.db.ExecContext(ctx, q, app); err != nil {
		return fmt.Errorf("postgres: delete attachment keys for %q: %w", app, err)
	}
	return nil
}
