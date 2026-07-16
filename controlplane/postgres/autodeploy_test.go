// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestStoreAutoDeployLevel exercises the app_autodeploy migration and its get/set round-trip against a
// real database (ADR-0052): a missing row reads as the default, a set is read back, an update
// overwrites in place, and the level is keyed per (app, environment).
func TestStoreAutoDeployLevel(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"

	// A missing row resolves to the built-in default, with no row required for an app to have a level.
	got, err := s.AutoDeployLevel(ctx, app, "default")
	if err != nil {
		t.Fatalf("AutoDeployLevel (absent): %v", err)
	}
	if got != cp.DefaultAutoDeployLevel {
		t.Fatalf("absent level = %q, want %q", got, cp.DefaultAutoDeployLevel)
	}

	// A set is read back.
	if err := s.SetAutoDeployLevel(ctx, app, "default", cp.AutoDeployPatch); err != nil {
		t.Fatalf("SetAutoDeployLevel: %v", err)
	}
	if got, _ := s.AutoDeployLevel(ctx, app, "default"); got != cp.AutoDeployPatch {
		t.Fatalf("level after set = %q, want patch", got)
	}

	// A second set overwrites in place (upsert on the (app, environment) primary key).
	if err := s.SetAutoDeployLevel(ctx, app, "default", cp.AutoDeployOff); err != nil {
		t.Fatalf("SetAutoDeployLevel (update): %v", err)
	}
	if got, _ := s.AutoDeployLevel(ctx, app, "default"); got != cp.AutoDeployOff {
		t.Fatalf("level after update = %q, want off", got)
	}

	// A different environment carries an independent level; the default env is untouched, and an env
	// with no row still reads the default.
	if err := s.SetAutoDeployLevel(ctx, app, "prod", cp.AutoDeployMajor); err != nil {
		t.Fatalf("SetAutoDeployLevel prod: %v", err)
	}
	if got, _ := s.AutoDeployLevel(ctx, app, "prod"); got != cp.AutoDeployMajor {
		t.Fatalf("prod level = %q, want major", got)
	}
	if got, _ := s.AutoDeployLevel(ctx, app, "default"); got != cp.AutoDeployOff {
		t.Fatalf("default env level after prod set = %q, want off (independent)", got)
	}
	if got, _ := s.AutoDeployLevel(ctx, app, "staging"); got != cp.DefaultAutoDeployLevel {
		t.Fatalf("unset env level = %q, want %q", got, cp.DefaultAutoDeployLevel)
	}

	// An invalid level is rejected before it reaches the table.
	if err := s.SetAutoDeployLevel(ctx, app, "default", cp.AutoDeployLevel("bogus")); err == nil {
		t.Fatalf("SetAutoDeployLevel with invalid level should error")
	}
}

// TestStoreAutoDeployCandidates proves the poller's candidate enumeration against a real database
// (ADR-0052 Phase 4b): it returns the distinct (app, environment) pairs that have a recorded release
// — regardless of whether an app_autodeploy row exists (the poller reads each pair's level and skips
// those that are off, which is the opt-in default — ADR-0054) — and includes the queried pairs.
func TestStoreAutoDeployCandidates(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"
	api := t.Name() + "-api"

	// Two apps: web has two releases in default (one distinct pair) and one in prod; api has one in
	// default. Distinct pairs: (web, default), (web, prod), (api, default).
	rels := []cp.Release{
		{ID: app + "-r1", App: app, Image: "ghcr.io/u/web:1.0.0", Environment: "default", Status: cp.ReleaseSuperseded},
		{ID: app + "-r2", App: app, Image: "ghcr.io/u/web:1.1.0", Environment: "default", Status: cp.ReleaseDeployed},
		{ID: app + "-p1", App: app, Image: "ghcr.io/u/web:1.1.0", Environment: "prod", Status: cp.ReleaseDeployed},
		{ID: api + "-r1", App: api, Image: "ghcr.io/u/api:2.0.0", Environment: "default", Status: cp.ReleaseDeployed},
	}
	for _, r := range rels {
		if err := s.SaveRelease(ctx, r); err != nil {
			t.Fatalf("SaveRelease(%s): %v", r.ID, err)
		}
	}

	got, err := s.AutoDeployCandidates(ctx)
	if err != nil {
		t.Fatalf("AutoDeployCandidates: %v", err)
	}
	want := map[cp.AppEnvRef]bool{
		{App: app, Env: "default"}: false,
		{App: app, Env: "prod"}:    false,
		{App: api, Env: "default"}: false,
	}
	for _, ref := range got {
		if _, ok := want[ref]; ok {
			want[ref] = true
		}
	}
	for ref, seen := range want {
		if !seen {
			t.Errorf("candidate %+v missing from %v", ref, got)
		}
	}
}
