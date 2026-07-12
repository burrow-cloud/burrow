// SPDX-License-Identifier: Apache-2.0
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

	latest, err := s.LatestRelease(ctx, app, "default")
	if err != nil || latest.ID != r2.ID {
		t.Fatalf("LatestRelease = %+v, err=%v, want %s", latest, err, r2.ID)
	}

	all, err := s.Releases(ctx, app, "default")
	if err != nil || len(all) != 2 || all[0].ID != r1.ID || all[1].ID != r2.ID {
		t.Fatalf("Releases = %+v, err=%v, want [%s %s] oldest first", all, err, r1.ID, r2.ID)
	}
	if all[1].Supersedes != r1.ID {
		t.Errorf("r2.Supersedes = %q, want %q", all[1].Supersedes, r1.ID)
	}
	// A save with no Environment reads back under the canonical default environment (the migration
	// backfills existing rows and SaveRelease defaults an empty Environment to "default").
	if latest.Environment != "default" {
		t.Errorf("Environment = %q, want default (backfilled)", latest.Environment)
	}

	// Not-found cases map to controlplane.ErrNotFound.
	if _, err := s.Release(ctx, t.Name()+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Release(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestRelease(ctx, t.Name()+"-nobody", "default"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("LatestRelease(nobody) err = %v, want ErrNotFound", err)
	}
	if none, err := s.Releases(ctx, t.Name()+"-nobody", "default"); err != nil || len(none) != 0 {
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

	all, err := s.Releases(ctx, app, "default")
	if err != nil || len(all) != 2 || all[0].ID != id1 || all[1].ID != id2 {
		t.Fatalf("Releases after overwrite = %+v (err=%v), want [%s %s]", all, err, id1, id2)
	}
	if all[0].Status != cp.ReleaseSuperseded {
		t.Errorf("id1 status = %q, want superseded (overwrite should apply)", all[0].Status)
	}
}

// TestStoreListReleases reads the deploy timeline: every release for an app, newest first,
// isolated per app, and empty for an app with none. It is the read behind `app history`.
func TestStoreListReleases(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"
	other := t.Name() + "-api"

	r1 := cp.Release{ID: t.Name() + "-r1", App: app, Image: "img:1", Status: cp.ReleaseSuperseded}
	r2 := cp.Release{ID: t.Name() + "-r2", App: app, Image: "img:2", Supersedes: r1.ID, Status: cp.ReleaseSuperseded}
	r3 := cp.Release{ID: t.Name() + "-r3", App: app, Image: "img:3", Supersedes: r2.ID, Status: cp.ReleaseDeployed}
	o1 := cp.Release{ID: t.Name() + "-o1", App: other, Image: "api:1", Status: cp.ReleaseDeployed}
	for _, r := range []cp.Release{r1, r2, r3, o1} {
		if err := s.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease(%s): %v", r.ID, err)
		}
	}

	// Newest first: r3, r2, r1 — the reverse of Releases' oldest-first order.
	got, err := s.ListReleases(ctx, app, "default")
	if err != nil || len(got) != 3 || got[0].ID != r3.ID || got[1].ID != r2.ID || got[2].ID != r1.ID {
		t.Fatalf("ListReleases = %+v, err=%v, want [%s %s %s] newest first", got, err, r3.ID, r2.ID, r1.ID)
	}
	if got[0].Status != cp.ReleaseDeployed || got[0].Image != "img:3" {
		t.Errorf("newest release = %+v, want the deployed img:3", got[0])
	}

	// Per-app isolation: the other app's timeline holds only its own release.
	if oth, err := s.ListReleases(ctx, other, "default"); err != nil || len(oth) != 1 || oth[0].ID != o1.ID {
		t.Errorf("ListReleases(other) = %+v, err=%v, want just %s", oth, err, o1.ID)
	}
	// An app with no releases yields an empty slice and no error.
	if none, err := s.ListReleases(ctx, t.Name()+"-nobody", "default"); err != nil || len(none) != 0 {
		t.Errorf("ListReleases(nobody) = %+v, err=%v, want empty", none, err)
	}
}

// TestStoreDeleteReleases removes every release for an app and leaves no rows behind; deleting
// an app that has no releases is a no-op, not an error.
func TestStoreDeleteReleases(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"

	for _, id := range []string{t.Name() + "-r1", t.Name() + "-r2"} {
		if err := s.SaveRelease(ctx, cp.Release{ID: id, App: app, Image: "img:1", Status: cp.ReleaseDeployed}); err != nil {
			t.Fatalf("SaveRelease(%s): %v", id, err)
		}
	}

	if err := s.DeleteReleases(ctx, app); err != nil {
		t.Fatalf("DeleteReleases: %v", err)
	}
	if all, err := s.Releases(ctx, app, "default"); err != nil || len(all) != 0 {
		t.Fatalf("Releases after delete = %+v (err=%v), want empty", all, err)
	}
	// Deleting again (no releases) is a no-op.
	if err := s.DeleteReleases(ctx, app); err != nil {
		t.Errorf("DeleteReleases on empty: %v", err)
	}
}

// TestStoreReleasesPerEnvironment proves releases are keyed per (app, environment) in the store: the
// same app saved in staging and prod reads back isolated per environment, and the not-found LatestRelease
// respects the environment (ADR-0052 Phase 4a).
func TestStoreReleasesPerEnvironment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"

	staging := cp.Release{ID: t.Name() + "-s1", App: app, Environment: "staging", Image: "img:1", Status: cp.ReleaseDeployed}
	prod1 := cp.Release{ID: t.Name() + "-p1", App: app, Environment: "prod", Image: "img:1", Status: cp.ReleaseSuperseded}
	prod2 := cp.Release{ID: t.Name() + "-p2", App: app, Environment: "prod", Image: "img:2", Supersedes: prod1.ID, Status: cp.ReleaseDeployed}
	for _, r := range []cp.Release{staging, prod1, prod2} {
		if err := s.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease(%s): %v", r.ID, err)
		}
	}

	// prod has two releases; LatestRelease/Releases/ListReleases return only prod's rows.
	if latest, err := s.LatestRelease(ctx, app, "prod"); err != nil || latest.ID != prod2.ID {
		t.Fatalf("LatestRelease(prod) = %+v, err=%v, want %s", latest, err, prod2.ID)
	}
	if all, err := s.Releases(ctx, app, "prod"); err != nil || len(all) != 2 || all[0].ID != prod1.ID || all[1].ID != prod2.ID {
		t.Fatalf("Releases(prod) = %+v, err=%v, want [%s %s]", all, err, prod1.ID, prod2.ID)
	}
	if list, err := s.ListReleases(ctx, app, "prod"); err != nil || len(list) != 2 || list[0].ID != prod2.ID {
		t.Fatalf("ListReleases(prod) = %+v, err=%v, want prod2 newest", list, err)
	}
	// staging has exactly one, isolated from prod.
	if all, err := s.Releases(ctx, app, "staging"); err != nil || len(all) != 1 || all[0].ID != staging.ID {
		t.Fatalf("Releases(staging) = %+v, err=%v, want [%s]", all, err, staging.ID)
	}
	// An environment with no rows for this app: empty list, ErrNotFound from LatestRelease.
	if all, err := s.Releases(ctx, app, "default"); err != nil || len(all) != 0 {
		t.Fatalf("Releases(default) = %+v, err=%v, want empty", all, err)
	}
	if _, err := s.LatestRelease(ctx, app, "default"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("LatestRelease(default) err = %v, want ErrNotFound", err)
	}
}

// TestStoreReleaseProvenance proves the deploy-record provenance fields round-trip: an auto deploy
// carries its trigger, level, and tag, and a manual deploy (empty Trigger) reads back as "manual"
// (ADR-0052 §5).
func TestStoreReleaseProvenance(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"

	auto := cp.Release{
		ID: t.Name() + "-a1", App: app, Environment: "default", Image: "img:1.3.0",
		Status: cp.ReleaseDeployed, Trigger: cp.TriggerAuto, AutoLevel: cp.AutoDeployMinor, AutoTag: "1.3.0",
	}
	manual := cp.Release{ID: t.Name() + "-m1", App: app, Environment: "default", Image: "img:1.2.0", Status: cp.ReleaseDeployed}
	for _, r := range []cp.Release{auto, manual} {
		if err := s.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease(%s): %v", r.ID, err)
		}
	}

	gotAuto, err := s.Release(ctx, auto.ID)
	if err != nil {
		t.Fatalf("Release(auto): %v", err)
	}
	if gotAuto.Trigger != cp.TriggerAuto || gotAuto.AutoLevel != cp.AutoDeployMinor || gotAuto.AutoTag != "1.3.0" {
		t.Errorf("auto provenance = trigger %q level %q tag %q, want auto/minor/1.3.0", gotAuto.Trigger, gotAuto.AutoLevel, gotAuto.AutoTag)
	}
	// A manual deploy saved with an empty Trigger defaults to "manual" and carries no auto fields.
	gotManual, err := s.Release(ctx, manual.ID)
	if err != nil {
		t.Fatalf("Release(manual): %v", err)
	}
	if gotManual.Trigger != cp.TriggerManual || gotManual.AutoLevel != "" || gotManual.AutoTag != "" {
		t.Errorf("manual provenance = trigger %q level %q tag %q, want manual and no auto fields", gotManual.Trigger, gotManual.AutoLevel, gotManual.AutoTag)
	}
}

// TestStoreDisableAutoDeploy exercises the rollback/downgrade safety stop against a real database:
// DisableAutoDeploy sets the level to off with a reason, AutoDeployReason returns it, and
// SetAutoDeployLevel (the human re-enable) clears it (ADR-0052 §5).
func TestStoreDisableAutoDeploy(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"

	if err := s.DisableAutoDeploy(ctx, app, "default", "disabled by rollback"); err != nil {
		t.Fatalf("DisableAutoDeploy: %v", err)
	}
	if lvl, err := s.AutoDeployLevel(ctx, app, "default"); err != nil || lvl != cp.AutoDeployOff {
		t.Fatalf("level after disable = %q, err=%v, want off", lvl, err)
	}
	if reason, err := s.AutoDeployReason(ctx, app, "default"); err != nil || reason != "disabled by rollback" {
		t.Fatalf("reason = %q, err=%v, want disabled by rollback", reason, err)
	}
	// An environment with no row has no reason.
	if reason, err := s.AutoDeployReason(ctx, app, "prod"); err != nil || reason != "" {
		t.Fatalf("prod reason = %q, err=%v, want empty", reason, err)
	}
	// A human re-enable clears the reason.
	if err := s.SetAutoDeployLevel(ctx, app, "default", cp.AutoDeployMinor); err != nil {
		t.Fatalf("SetAutoDeployLevel: %v", err)
	}
	if reason, err := s.AutoDeployReason(ctx, app, "default"); err != nil || reason != "" {
		t.Fatalf("reason after re-enable = %q, err=%v, want empty", reason, err)
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

// TestStoreAppEnvRoundTrip exercises the per-app non-secret env store (ADR-0028) against a
// real database: set upserts, list returns the current map, and unset removes a key.
func TestStoreAppEnvRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"
	other := t.Name() + "-api"

	// An app with no env yields an empty map, not an error.
	got, err := s.AppEnv(ctx, app)
	if err != nil {
		t.Fatalf("AppEnv (empty): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("AppEnv (empty) = %v, want empty", got)
	}

	if err := s.SetAppEnv(ctx, app, "LOG_LEVEL", "debug"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	if err := s.SetAppEnv(ctx, app, "FEATURE", "on"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	// Upsert overwrites in place.
	if err := s.SetAppEnv(ctx, app, "LOG_LEVEL", "info"); err != nil {
		t.Fatalf("SetAppEnv (upsert): %v", err)
	}
	// A different app's env is isolated.
	if err := s.SetAppEnv(ctx, other, "LOG_LEVEL", "trace"); err != nil {
		t.Fatalf("SetAppEnv (other): %v", err)
	}

	got, err = s.AppEnv(ctx, app)
	if err != nil {
		t.Fatalf("AppEnv: %v", err)
	}
	if got["LOG_LEVEL"] != "info" || got["FEATURE"] != "on" || len(got) != 2 {
		t.Errorf("AppEnv = %v, want {LOG_LEVEL:info FEATURE:on}", got)
	}

	// Unset removes a key; removing a missing key is a no-op.
	if err := s.UnsetAppEnv(ctx, app, "FEATURE"); err != nil {
		t.Fatalf("UnsetAppEnv: %v", err)
	}
	if err := s.UnsetAppEnv(ctx, app, "NOPE"); err != nil {
		t.Fatalf("UnsetAppEnv (absent): %v", err)
	}
	got, err = s.AppEnv(ctx, app)
	if err != nil {
		t.Fatalf("AppEnv after unset: %v", err)
	}
	if _, present := got["FEATURE"]; present || got["LOG_LEVEL"] != "info" {
		t.Errorf("AppEnv after unset = %v, want only LOG_LEVEL:info", got)
	}

	// Cleanup so the shared database stays tidy across re-runs.
	_ = s.UnsetAppEnv(ctx, app, "LOG_LEVEL")
	_ = s.UnsetAppEnv(ctx, other, "LOG_LEVEL")
}
