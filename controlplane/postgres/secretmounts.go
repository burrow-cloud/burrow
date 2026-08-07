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

// The store side of a secret projected as a file (ADR-0089 §5). A mount is app configuration, not a
// release property, so it is recorded here and re-read by every path that authors a workload —
// exactly as the declared health endpoint above it is.
//
// NOTHING IN THIS FILE HANDLES A VALUE. Both queries select and write a key name, a filename and a
// directory; the value stays in the per-app Kubernetes Secret, where ADR-0029 put it.

// SecretMounts returns app's file projection in env: the directory override, empty when the app uses
// the default, and the mounts sorted by key so every render of a pod template from them is
// byte-identical. An app that mounts nothing yields the zero value and no error — no mount is the
// state every app starts in, and the deploy path reads this on every apply.
func (s *Store) SecretMounts(ctx context.Context, app, env string) (controlplane.SecretMounts, error) {
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	var out controlplane.SecretMounts

	const dirQ = `SELECT directory FROM app_secret_dirs WHERE app = $1 AND environment = $2`
	err := s.db.QueryRowContext(ctx, dirQ, app, env).Scan(&out.Dir)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return controlplane.SecretMounts{}, fmt.Errorf("postgres: secrets directory for %q in %q: %w", app, env, err)
	}

	const q = `
SELECT app, environment, key, filename, updated_at
FROM app_secret_mounts
WHERE app = $1 AND environment = $2
ORDER BY key`
	rows, err := s.db.QueryContext(ctx, q, app, env)
	if err != nil {
		return controlplane.SecretMounts{}, fmt.Errorf("postgres: secret mounts for %q in %q: %w", app, env, err)
	}
	defer rows.Close()
	for rows.Next() {
		var m controlplane.SecretMount
		if err := rows.Scan(&m.App, &m.Environment, &m.Key, &m.Filename, &m.UpdatedAt); err != nil {
			return controlplane.SecretMounts{}, fmt.Errorf("postgres: secret mounts for %q in %q: %w", app, env, err)
		}
		out.Mounts = append(out.Mounts, m)
	}
	if err := rows.Err(); err != nil {
		return controlplane.SecretMounts{}, fmt.Errorf("postgres: secret mounts for %q in %q: %w", app, env, err)
	}
	return out, nil
}

// SetSecretMount upserts one key's projection for an app in one environment. Re-mounting a key under
// a new filename replaces the old projection rather than adding a second file.
func (s *Store) SetSecretMount(ctx context.Context, m controlplane.SecretMount) error {
	if m.App == "" {
		return fmt.Errorf("postgres: set secret mount: empty app")
	}
	if m.Key == "" {
		return fmt.Errorf("postgres: set secret mount for %q: empty key", m.App)
	}
	const q = `
INSERT INTO app_secret_mounts (app, environment, key, filename, updated_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (app, environment, key) DO UPDATE SET
    filename = EXCLUDED.filename, updated_at = EXCLUDED.updated_at`
	env := m.Environment
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	if _, err := s.db.ExecContext(ctx, q, m.App, env, m.Key, m.Filename, m.UpdatedAt); err != nil {
		// The key NAME is in the message and the value is not, because there is no value here to be
		// in it (ADR-0029).
		return fmt.Errorf("postgres: set secret mount %q for %q in %q: %w", m.Key, m.App, env, err)
	}
	return nil
}

// UnsetSecretMount stops projecting one key as a file. Removing a mount that was never made is a
// no-op: unset must be idempotent, since it is what a caller reaches for when they are not sure what
// is mounted.
func (s *Store) UnsetSecretMount(ctx context.Context, app, env, key string) error {
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	const q = `DELETE FROM app_secret_mounts WHERE app = $1 AND environment = $2 AND key = $3`
	if _, err := s.db.ExecContext(ctx, q, app, env, key); err != nil {
		return fmt.Errorf("postgres: unset secret mount %q for %q in %q: %w", key, app, env, err)
	}
	return nil
}

// SetSecretsDir records the directory an app's mounted keys land in, overriding the default. It is
// one directory for the whole app in that environment (ADR-0089 §2), which is what the primary key
// on this table says.
func (s *Store) SetSecretsDir(ctx context.Context, app, env, dir string, at time.Time) error {
	if app == "" {
		return fmt.Errorf("postgres: set secrets directory: empty app")
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	const q = `
INSERT INTO app_secret_dirs (app, environment, directory, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (app, environment) DO UPDATE SET
    directory = EXCLUDED.directory, updated_at = EXCLUDED.updated_at`
	if _, err := s.db.ExecContext(ctx, q, app, env, dir, at); err != nil {
		return fmt.Errorf("postgres: set secrets directory for %q in %q: %w", app, env, err)
	}
	return nil
}

// DeleteSecretMounts removes every mount and directory override for app across all environments —
// the durable side of an app teardown. An app created later under the same name starts with nothing
// projected rather than inheriting a previous occupant's file layout.
func (s *Store) DeleteSecretMounts(ctx context.Context, app string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM app_secret_mounts WHERE app = $1`, app); err != nil {
		return fmt.Errorf("postgres: delete secret mounts for %q: %w", app, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM app_secret_dirs WHERE app = $1`, app); err != nil {
		return fmt.Errorf("postgres: delete secrets directory for %q: %w", app, err)
	}
	return nil
}
