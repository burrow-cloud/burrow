// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveLoadRoundTrip confirms a saved config reloads with the same handles and selection,
// and that the migratable apiVersion/kind header is stamped on disk.
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv(envConfigPath, path)

	cfg := &Config{
		Current: "nonprod",
		Environments: []Environment{
			{Name: "dev", Context: "do-nyc1-dev", AppNamespace: "burrow-apps"},
			{Name: "nonprod", Context: "do-nyc1-nonprod", ControlPlaneNamespace: "burrow", AppNamespace: "team-x"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "apiVersion: "+APIVersion) || !strings.Contains(string(data), "kind: "+Kind) {
		t.Errorf("saved file missing apiVersion/kind header:\n%s", data)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.APIVersion != APIVersion || got.Kind != Kind {
		t.Errorf("header = %q/%q, want %q/%q", got.APIVersion, got.Kind, APIVersion, Kind)
	}
	if got.Current != "nonprod" {
		t.Errorf("current = %q, want nonprod", got.Current)
	}
	if len(got.Environments) != 2 {
		t.Fatalf("environments = %d, want 2", len(got.Environments))
	}
	np, ok := got.Lookup("nonprod")
	if !ok || np.Context != "do-nyc1-nonprod" || np.AppNamespace != "team-x" {
		t.Errorf("nonprod handle = %+v (found=%v), want context do-nyc1-nonprod / appNamespace team-x", np, ok)
	}
}

// TestLoadMissingFile confirms an absent file is first-run, not an error: an empty config and
// Exists() == false.
func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv(envConfigPath, path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load of missing file should not error: %v", err)
	}
	if cfg == nil || len(cfg.Environments) != 0 || cfg.Current != "" {
		t.Errorf("missing file should load an empty config, got %+v", cfg)
	}
	exists, err := Exists()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Errorf("Exists() = true for a missing file, want false")
	}
}

// TestLoadEmptyFile confirms an empty (or whitespace-only) file is tolerated as first-run.
func TestLoadEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envConfigPath, path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load of empty file should not error: %v", err)
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("empty file should load no environments, got %+v", cfg)
	}
	exists, err := Exists()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Errorf("Exists() = false for a present (empty) file, want true")
	}
}

// TestPathHonorsEnvOverride confirms $BURROW_CONFIG wins, and the default ends at
// ~/.burrow/config.
func TestPathHonorsEnvOverride(t *testing.T) {
	t.Setenv(envConfigPath, "/tmp/custom-burrow-config")
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p != "/tmp/custom-burrow-config" {
		t.Errorf("Path() = %q, want the $BURROW_CONFIG override", p)
	}

	t.Setenv(envConfigPath, "")
	p, err = Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if !strings.HasSuffix(p, filepath.Join(".burrow", "config")) {
		t.Errorf("default Path() = %q, want it to end at .burrow/config", p)
	}
}

// TestValidateHeader confirms a present apiVersion/kind is validated, and an absent header is
// tolerated.
func TestValidateHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv(envConfigPath, path)

	if err := os.WriteFile(path, []byte("apiVersion: burrow.dev/v999\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Errorf("Load should reject an unsupported apiVersion")
	}

	if err := os.WriteFile(path, []byte("apiVersion: burrow.dev/v1\nkind: NotAConfig\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Errorf("Load should reject an unsupported kind")
	}

	// A headerless but otherwise valid file is tolerated (hand-started / pre-header).
	if err := os.WriteFile(path, []byte("environments:\n  - name: dev\n    context: do-nyc1-dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("headerless file should load: %v", err)
	}
	if _, ok := cfg.Lookup("dev"); !ok {
		t.Errorf("headerless file did not parse its environment")
	}
}

// TestAddRemoveRename exercises the handle-mutation helpers later slices reuse.
func TestAddRemoveRename(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Add(Environment{Name: "dev", Context: "do-nyc1-dev"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := cfg.Add(Environment{Name: "dev", Context: "other"}); err == nil {
		t.Errorf("Add of a duplicate name should error")
	}
	if err := cfg.Add(Environment{Context: "no-name"}); err == nil {
		t.Errorf("Add with an empty name should error")
	}

	cfg.Current = "dev"
	if err := cfg.Rename("dev", "development"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if cfg.Current != "development" {
		t.Errorf("Rename should carry the pin, current = %q", cfg.Current)
	}
	if _, ok := cfg.Lookup("development"); !ok {
		t.Errorf("renamed handle not found")
	}
	if err := cfg.Rename("missing", "x"); err == nil {
		t.Errorf("Rename of a missing handle should error")
	}

	if err := cfg.Remove("development"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if cfg.Current != "" {
		t.Errorf("removing the pinned handle should revert to follow, current = %q", cfg.Current)
	}
	if err := cfg.Remove("development"); err == nil {
		t.Errorf("Remove of a missing handle should error")
	}
}
