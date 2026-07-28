// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Migration 00018 is the half of ADR-0067 §2–§3 that an EXISTING install meets: the canonical name of
// the first environment changes from `default` to `prod`, and every control-plane row keyed by it has
// to move with it, because reading both names in parallel would mean two names for one environment —
// the ambiguity the rename exists to remove.
//
// These tests run each case in its own schema, migrated from nothing to 00017 and then across the
// boundary, which is the only way to observe a migration rather than its already-applied result. They
// need a real Postgres (BURROW_TEST_DATABASE_URL) and skip without one.

const (
	// beforeRename is the last migration that still knew the first environment as `default`.
	beforeRename int64 = 17
	// theRename is 00018_first_environment_is_prod.
	theRename int64 = 18
)

// scratchSchema opens a connection whose search_path is a schema created for this test alone, and a
// goose provider over the embedded migrations pointed at it. Migrations are observed from an empty
// schema forward, so the shared test database other store tests use is never touched.
func scratchSchema(t *testing.T) (*sql.DB, *goose.Provider) {
	t.Helper()
	dsn := os.Getenv("BURROW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BURROW_TEST_DATABASE_URL to run the Postgres integration tests")
	}
	ctx := context.Background()

	schema := "burrow_mig_" + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToLower(t.Name()))

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema)); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("pgx", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema))
	})

	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("migrations fs: %v", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		t.Fatalf("locker: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return db, provider
}

// TestFirstEnvironmentRenameMovesEveryStoredRow is the existing-install case in full: an install that
// recorded everything under `default` comes out the other side recorded under `prod`, with the same
// apps, the same add-on instance NAME, and the same backup ids. The instance name is the assertion
// that matters most — `burrow-postgres` is a live Deployment, PVC and Secret in the cluster, and the
// rename must not have implied a rename there (ADR-0067 §3).
func TestFirstEnvironmentRenameMovesEveryStoredRow(t *testing.T) {
	ctx := context.Background()
	db, provider := scratchSchema(t)

	if _, err := provider.UpTo(ctx, beforeRename); err != nil {
		t.Fatalf("migrate to %d: %v", beforeRename, err)
	}

	// The state a pre-ADR-0067 install is in: every row on the implicit environment, whose stored
	// name was `default`, and one Postgres add-on instance under its unqualified cluster name.
	seed := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO releases (id, app, image, env, command, replicas, status, supersedes, created_at, environment)
		  VALUES ('r1', 'web', 'img:1', '{}'::jsonb, '[]'::jsonb, 1, 'deployed', '', now(), 'default')`, nil},
		{`INSERT INTO addons (name, type, mode, backend, image, endpoint, capabilities, secret_key, created_at, environment)
		  VALUES ('burrow-postgres', 'postgres', 'installed', 'postgres', 'postgres:17-alpine', 'burrow-postgres:5432', '["database"]'::jsonb, '', now(), 'default')`, nil},
		{`INSERT INTO postgres_backups (id, app, created_at, path, size_bytes, status, environment)
		  VALUES ('b1', 'web', now(), '/backups/web/b1.dump', 42, 'completed', 'default')`, nil},
		{`INSERT INTO app_autodeploy (app, environment, level) VALUES ('web', 'default', 'patch')`, nil},
	}
	for _, s := range seed {
		if _, err := db.ExecContext(ctx, s.q, s.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, s.q)
		}
	}

	if _, err := provider.UpTo(ctx, theRename); err != nil {
		t.Fatalf("migrate across the rename: %v", err)
	}

	for _, c := range []struct{ table, id, idCol string }{
		{"releases", "r1", "id"},
		{"addons", "burrow-postgres", "name"},
		{"postgres_backups", "b1", "id"},
	} {
		var env string
		q := fmt.Sprintf(`SELECT environment FROM %s WHERE %s = $1`, c.table, c.idCol)
		if err := db.QueryRowContext(ctx, q, c.id).Scan(&env); err != nil {
			t.Fatalf("read %s: %v", c.table, err)
		}
		if env != "prod" {
			t.Errorf("%s row %q environment = %q, want prod", c.table, c.id, env)
		}
	}
	var level string
	if err := db.QueryRowContext(ctx, `SELECT level FROM app_autodeploy WHERE app = 'web' AND environment = 'prod'`).Scan(&level); err != nil {
		t.Fatalf("auto-deploy level did not move to prod: %v", err)
	}
	if level != "patch" {
		t.Errorf("auto-deploy level = %q, want patch (the row moved, its value did not)", level)
	}

	// Nothing is left behind under the retired name: a row readable under BOTH names would be the
	// two-names-for-one-environment state the rename exists to remove.
	for _, table := range []string{"releases", "addons", "postgres_backups", "app_autodeploy"} {
		var n int
		if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE environment = 'default'`, table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d row(s) under the retired name `default`", table, n)
		}
	}

	// NOTHING IN THE CLUSTER MOVED. The add-on row is the proxy for a live Deployment, PVC and
	// superuser Secret: its name is what burrowd looks the instance up by, and it is unchanged.
	var name, endpoint string
	if err := db.QueryRowContext(ctx, `SELECT name, endpoint FROM addons WHERE type = 'postgres'`).Scan(&name, &endpoint); err != nil {
		t.Fatalf("read addon: %v", err)
	}
	if name != "burrow-postgres" || endpoint != "burrow-postgres:5432" {
		t.Errorf("add-on instance = (%q, %q), want it untouched at (burrow-postgres, burrow-postgres:5432) — the environment was renamed, the instance was not", name, endpoint)
	}
}

// TestFirstEnvironmentRenameRefusesAnExistingProd covers the one install the rename cannot serve: one
// that already ran `burrow env add prod` for a namespace of its own. Folding the old `default` rows
// into that environment would join two environments' deploy histories and cross their databases — the
// exact failure ADR-0067 exists to prevent — so the migration stops and asks for a human decision
// instead of picking one.
func TestFirstEnvironmentRenameRefusesAnExistingProd(t *testing.T) {
	ctx := context.Background()
	db, provider := scratchSchema(t)

	if _, err := provider.UpTo(ctx, beforeRename); err != nil {
		t.Fatalf("migrate to %d: %v", beforeRename, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO environments (name, namespace) VALUES ('prod', 'burrow-apps-prod')`); err != nil {
		t.Fatalf("seed environment: %v", err)
	}

	_, err := provider.UpTo(ctx, theRename)
	if err == nil {
		t.Fatal("migration succeeded with a pre-existing `prod` environment; it must refuse rather than merge two environments")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("refusal = %v, want it to say a prod environment already exists", err)
	}
}
