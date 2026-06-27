// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/postgres"
)

// openStore connects to the test database named by BURROW_TEST_DATABASE_URL and applies
// the schema, skipping the test when the variable is unset. Tests isolate themselves by
// prefixing app and release IDs with the test name, so they are safe to run against a
// shared database and idempotent across re-runs.
func openStore(t *testing.T) *postgres.Store {
	t.Helper()
	dsn := os.Getenv("BURROW_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BURROW_TEST_DATABASE_URL to run the Postgres integration tests")
	}
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx, "0.1.0"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// TestStoreUpgradeGate exercises the single-minor-step gate against a real database.
// It only ever stamps v0.1.0 (the refused jump never stamps), so it does not disturb
// the shared burrow_meta other tests rely on.
func TestStoreUpgradeGate(t *testing.T) {
	ctx := context.Background()
	s := openStore(t) // migrates and stamps v0.1.0

	// A jump of more than one minor is refused.
	if err := s.Migrate(ctx, "0.5.0"); err == nil {
		t.Fatalf("Migrate v0.1.0 -> v0.5.0 should be refused")
	}
	// Re-running the recorded version is fine.
	if err := s.Migrate(ctx, "0.1.0"); err != nil {
		t.Fatalf("re-migrate same version: %v", err)
	}
	// The store still works after a refused upgrade.
	if err := s.SaveRelease(ctx, cp.Release{ID: t.Name() + "-r1", App: t.Name() + "-web", Image: "img:1"}); err != nil {
		t.Fatalf("SaveRelease after refused upgrade: %v", err)
	}
}

func TestStoreSaveAndQuery(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"
	other := t.Name() + "-api"

	r1 := cp.Release{ID: t.Name() + "-r1", App: app, Image: "img:1", Replicas: 1, Status: cp.ReleaseDeployed}
	r2 := cp.Release{ID: t.Name() + "-r2", App: app, Image: "img:2", Replicas: 2, Supersedes: r1.ID, Status: cp.ReleaseDeployed}
	o1 := cp.Release{ID: t.Name() + "-o1", App: other, Image: "api:1", Replicas: 1}
	for _, r := range []cp.Release{r1, r2, o1} {
		if err := s.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease(%s): %v", r.ID, err)
		}
	}

	got, err := s.Release(ctx, r1.ID)
	if err != nil || got.Image != "img:1" || got.Status != cp.ReleaseDeployed {
		t.Fatalf("Release(r1) = %+v, err=%v", got, err)
	}

	latest, err := s.LatestRelease(ctx, app)
	if err != nil || latest.ID != r2.ID {
		t.Fatalf("LatestRelease = %+v, err=%v, want %s", latest, err, r2.ID)
	}

	all, err := s.Releases(ctx, app)
	if err != nil || len(all) != 2 || all[0].ID != r1.ID || all[1].ID != r2.ID {
		t.Fatalf("Releases = %+v, err=%v, want [%s %s] oldest first", all, err, r1.ID, r2.ID)
	}
	if all[1].Supersedes != r1.ID {
		t.Errorf("r2.Supersedes = %q, want %q", all[1].Supersedes, r1.ID)
	}

	// Not-found cases map to controlplane.ErrNotFound.
	if _, err := s.Release(ctx, t.Name()+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Release(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestRelease(ctx, t.Name()+"-nobody"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("LatestRelease(nobody) err = %v, want ErrNotFound", err)
	}
	if none, err := s.Releases(ctx, t.Name()+"-nobody"); err != nil || len(none) != 0 {
		t.Errorf("Releases(nobody) = %+v, err=%v, want empty", none, err)
	}
}

func TestStoreOverwriteKeepsOrder(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"
	id1, id2 := t.Name()+"-r1", t.Name()+"-r2"

	if err := s.SaveRelease(ctx, cp.Release{ID: id1, App: app, Image: "img:1", Status: cp.ReleasePending}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRelease(ctx, cp.Release{ID: id2, App: app, Image: "img:2", Status: cp.ReleaseDeployed}); err != nil {
		t.Fatal(err)
	}
	// Re-save id1 with a new status: order must not change and it must not duplicate.
	if err := s.SaveRelease(ctx, cp.Release{ID: id1, App: app, Image: "img:1", Status: cp.ReleaseSuperseded}); err != nil {
		t.Fatal(err)
	}

	all, err := s.Releases(ctx, app)
	if err != nil || len(all) != 2 || all[0].ID != id1 || all[1].ID != id2 {
		t.Fatalf("Releases after overwrite = %+v (err=%v), want [%s %s]", all, err, id1, id2)
	}
	if all[0].Status != cp.ReleaseSuperseded {
		t.Errorf("id1 status = %q, want superseded (overwrite should apply)", all[0].Status)
	}
}

func TestStoreRoundTripsEnvCommandAndTime(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	when := time.Date(2026, 6, 23, 15, 4, 5, 0, time.UTC)
	in := cp.Release{
		ID:          t.Name() + "-r1",
		App:         t.Name() + "-web",
		Image:       "registry.example.com/web@sha256:abc",
		Digest:      "sha256:abc",
		Env:         map[string]string{"A": "1", "B": "two"},
		Command:     []string{"server", "--port", "8080"},
		MetricsPort: 8080,
		Replicas:    3,
		Status:      cp.ReleaseDeployed,
		CreatedAt:   when,
	}
	if err := s.SaveRelease(ctx, in); err != nil {
		t.Fatalf("SaveRelease: %v", err)
	}

	got, err := s.Release(ctx, in.ID)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got.Env["A"] != "1" || got.Env["B"] != "two" || len(got.Env) != 2 {
		t.Errorf("Env = %v, want {A:1 B:two}", got.Env)
	}
	if len(got.Command) != 3 || got.Command[0] != "server" || got.Command[2] != "8080" {
		t.Errorf("Command = %v, want [server --port 8080]", got.Command)
	}
	if got.Digest != "sha256:abc" {
		t.Errorf("Digest = %q, want sha256:abc", got.Digest)
	}
	if got.MetricsPort != 8080 {
		t.Errorf("MetricsPort = %d, want 8080", got.MetricsPort)
	}
	if !got.CreatedAt.Equal(when) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, when)
	}
}
