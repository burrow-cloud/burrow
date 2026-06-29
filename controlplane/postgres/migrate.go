// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate brings the database schema up to date and records appVersion as the version
// that last migrated it. It first enforces the single-minor-step upgrade gate
// (ADR-0013): if the database was last migrated by a different version, the upgrade
// must be exactly one minor step forward within the same major, or Migrate refuses
// before changing anything. Migrations are applied under a Postgres advisory lock, so
// concurrent control-plane replicas do not race. Migrate is safe to call on every
// startup; with no pending migrations it only re-stamps the version.
func (s *Store) Migrate(ctx context.Context, appVersion string) error {
	bMaj, bMin, err := parseMajorMinor(appVersion)
	if err != nil {
		return fmt.Errorf("postgres: migrate: invalid binary version %q: %w", appVersion, err)
	}

	// Upgrade gate: enforce single-minor-step upgrades against the recorded version.
	stored, ok, err := s.storedVersion(ctx)
	if err != nil {
		return fmt.Errorf("postgres: migrate: reading recorded version: %w", err)
	}
	if ok {
		dMaj, dMin, err := parseMajorMinor(stored)
		if err != nil {
			return fmt.Errorf("postgres: migrate: recorded version %q is invalid: %w", stored, err)
		}
		if err := checkUpgrade(dMaj, dMin, bMaj, bMin); err != nil {
			return err
		}
	}

	if err := runMigrations(ctx, s.db); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO burrow_meta (id, version) VALUES (TRUE, $1)
		 ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version`, appVersion); err != nil {
		return fmt.Errorf("postgres: migrate: recording version: %w", err)
	}
	return nil
}

// storedVersion returns the Burrow version recorded in burrow_meta, and whether one was
// found. A database without the burrow_meta table (a fresh install) reports not-found
// with no error, so the gate is skipped on first migration.
func (s *Store) storedVersion(ctx context.Context) (string, bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT to_regclass('burrow_meta') IS NOT NULL`).Scan(&exists); err != nil {
		return "", false, err
	}
	if !exists {
		return "", false, nil
	}
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT version FROM burrow_meta WHERE id = TRUE`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("postgres: migrate: migrations fs: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("postgres: migrate: advisory locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("postgres: migrate: provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("postgres: migrate: applying migrations: %w", err)
	}
	return nil
}
