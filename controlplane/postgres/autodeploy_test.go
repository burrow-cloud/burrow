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
