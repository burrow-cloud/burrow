// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestAutoDeployDefaultAndSet covers the level lifecycle through the engine: an app with no stored
// level reads the default (minor), a set is reflected on the next read, and an invalid level is
// rejected as ErrInvalid (ADR-0052 §2).
func TestAutoDeployDefaultAndSet(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	// A brand-new app has no stored row, so it reads the built-in default.
	got, err := e.AutoDeploy(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeploy: %v", err)
	}
	if got != cp.DefaultAutoDeployLevel {
		t.Fatalf("default level = %q, want %q", got, cp.DefaultAutoDeployLevel)
	}

	// A set is reflected on the next read.
	if err := e.SetAutoDeploy(ctx, "web", "", cp.AutoDeployOff); err != nil {
		t.Fatalf("SetAutoDeploy: %v", err)
	}
	got, err = e.AutoDeploy(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeploy after set: %v", err)
	}
	if got != cp.AutoDeployOff {
		t.Fatalf("level after set = %q, want off", got)
	}

	// An invalid level is rejected as ErrInvalid and does not change the stored value.
	if err := e.SetAutoDeploy(ctx, "web", "", cp.AutoDeployLevel("sometimes")); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("invalid level err = %v, want ErrInvalid", err)
	}
	if got, _ := e.AutoDeploy(ctx, "web", ""); got != cp.AutoDeployOff {
		t.Fatalf("level after rejected set = %q, want off (unchanged)", got)
	}

	// An invalid app name is rejected as ErrInvalid before any store access.
	if _, err := e.AutoDeploy(ctx, "Bad Name", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("invalid app name err = %v, want ErrInvalid", err)
	}
}

// TestAutoDeployPerEnvironment proves the level is keyed per environment: prod and the default
// environment carry independent levels, and an unknown environment is a clear ErrNotFound on both the
// read and the write (ADR-0052 §2).
func TestAutoDeployPerEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	// prod at patch leaves the default environment at its default (minor).
	if err := e.SetAutoDeploy(ctx, "web", "prod", cp.AutoDeployPatch); err != nil {
		t.Fatalf("SetAutoDeploy prod: %v", err)
	}
	if got, _ := e.AutoDeploy(ctx, "web", "prod"); got != cp.AutoDeployPatch {
		t.Fatalf("prod level = %q, want patch", got)
	}
	if got, _ := e.AutoDeploy(ctx, "web", "default"); got != cp.DefaultAutoDeployLevel {
		t.Fatalf("default env level = %q, want %q", got, cp.DefaultAutoDeployLevel)
	}

	// An unknown environment is ErrNotFound on both read and write.
	if _, err := e.AutoDeploy(ctx, "web", "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("read unknown env err = %v, want ErrNotFound", err)
	}
	if err := e.SetAutoDeploy(ctx, "web", "ghost", cp.AutoDeployMajor); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("set unknown env err = %v, want ErrNotFound", err)
	}
}

// TestAutoDeploySetRefusesAmbiguousEnvironment confirms setting the level with no environment named is
// refused once more than one environment is registered, like every other per-app mutation (ADR-0047).
func TestAutoDeploySetRefusesAmbiguousEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	err := e.SetAutoDeploy(ctx, "web", "", cp.AutoDeployMajor)
	if _, ok := cp.AsAmbiguousEnvironment(err); !ok {
		t.Fatalf("set with ambiguous env = %v, want AmbiguousEnvironmentError", err)
	}
}
