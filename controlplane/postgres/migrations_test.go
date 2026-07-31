// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The migration set is versioned by the number in each filename (ADR-0013), and goose refuses to
// build a provider over a set that carries the same version twice. Nothing in git enforces that:
// two branches that each add "the next migration" pick the same number independently, the filenames
// differ so the merge is clean, and the duplicate only surfaces as a goose error when burrowd next
// starts against a database — an upgrade-time crash, on a commit that every check passed. A gap has
// the same shape: a renumbering that lands one file short leaves a hole no reviewer counts.
//
// These tests read the embedded set directly, so the guard costs nothing and runs without Postgres.
// checkMigrationVersions is separated from the FS read so the failure it exists to catch can itself
// be tested, rather than asserted only against a set that is currently correct.

// migrationVersion extracts the leading version number from a goose migration filename.
func migrationVersion(name string) (int64, error) {
	digits := name
	if i := strings.IndexFunc(name, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = name[:i]
	}
	if digits == "" {
		return 0, fmt.Errorf("migration %q does not begin with a version number", name)
	}
	return strconv.ParseInt(digits, 10, 64)
}

// checkMigrationVersions reports every way the version sequence of names is malformed: a version
// claimed by two files, or a number missing from an otherwise contiguous run starting at 1.
func checkMigrationVersions(names []string) []error {
	var errs []error
	byVersion := make(map[int64][]string, len(names))
	versions := make([]int64, 0, len(names))
	for _, name := range names {
		v, err := migrationVersion(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, seen := byVersion[v]; !seen {
			versions = append(versions, v)
		}
		byVersion[v] = append(byVersion[v], name)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })

	for _, v := range versions {
		if files := byVersion[v]; len(files) > 1 {
			sort.Strings(files)
			errs = append(errs, fmt.Errorf("migration version %d is claimed by %d files: %s — "+
				"two branches took the same next number; renumber the later one to the end of the set",
				v, len(files), strings.Join(files, ", ")))
		}
	}
	if len(versions) == 0 {
		return errs
	}
	if versions[0] != 1 {
		errs = append(errs, fmt.Errorf("migration versions start at %d, want 1", versions[0]))
	}
	for i := 1; i < len(versions); i++ {
		if prev, cur := versions[i-1], versions[i]; cur != prev+1 {
			errs = append(errs, fmt.Errorf("migration versions jump from %d to %d: %d..%d missing",
				prev, cur, prev+1, cur-1))
		}
	}
	return errs
}

// migrationNames returns the base filenames of the embedded migration set.
func migrationNames(t *testing.T) []string {
	t.Helper()
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("migrations fs: %v", err)
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no migrations embedded")
	}
	return names
}

// TestEmbeddedMigrationVersionsAreUniqueAndContiguous is the guard on the set that actually ships.
func TestEmbeddedMigrationVersionsAreUniqueAndContiguous(t *testing.T) {
	for _, err := range checkMigrationVersions(migrationNames(t)) {
		t.Error(err)
	}
}

func TestCheckMigrationVersions(t *testing.T) {
	cases := []struct {
		name    string
		names   []string
		wantErr bool
	}{
		{
			name:  "contiguous from one",
			names: []string{"00001_init.sql", "00002_providers.sql", "00003_addons.sql"},
		},
		{
			// The collision this file exists to prevent: two branches each took 00021.
			name:    "duplicate version",
			names:   []string{"00020_backup.sql", "00021_exposures.sql", "00021_operational_config.sql"},
			wantErr: true,
		},
		{
			name:    "gap in the sequence",
			names:   []string{"00001_init.sql", "00002_providers.sql", "00004_addons.sql"},
			wantErr: true,
		},
		{
			name:    "does not start at one",
			names:   []string{"00002_providers.sql", "00003_addons.sql"},
			wantErr: true,
		},
		{
			name:    "unversioned filename",
			names:   []string{"00001_init.sql", "providers.sql"},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := checkMigrationVersions(c.names)
			if c.wantErr && len(errs) == 0 {
				t.Fatalf("checkMigrationVersions(%v) = no errors, want an error", c.names)
			}
			if !c.wantErr && len(errs) != 0 {
				t.Fatalf("checkMigrationVersions(%v) = %v, want no errors", c.names, errs)
			}
		})
	}
}
