// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The store side of the deploy-time dependency check's one stored fact (ADR-0076 §4). Every app name
// is scoped to the test's own name, so these are safe against the shared database the suite runs on.

func checksApp(t *testing.T, suffix string) string {
	t.Helper()
	return strings.ToLower(t.Name()) + "-" + suffix
}

// TestStoreDependencyChecksDefaultToOn is the load-bearing row. The check is Burrow's DEFAULT, so an
// app nobody has thought about must read as enabled — not ErrNotFound, which would make the deploy
// path special-case the state every app is in.
func TestStoreDependencyChecksDefaultToOn(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := checksApp(t, "web")

	enabled, err := s.DependencyChecksEnabled(ctx, app, "prod")
	if err != nil {
		t.Fatalf("DependencyChecksEnabled for an app with no row returned an error, want true: %v", err)
	}
	if !enabled {
		t.Error("an app with no recorded decision is not checked; the check is supposed to be the default")
	}
}

// TestStoreDependencyChecksRoundTrip: disable, read back, re-enable. Re-enabling WRITES a row rather
// than deleting one, so "somebody looked at this and left it on" stays distinguishable from "nobody
// has ever touched it".
func TestStoreDependencyChecksRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := checksApp(t, "web")
	at := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)

	if err := s.SetDependencyChecks(ctx, app, "prod", false, at); err != nil {
		t.Fatalf("SetDependencyChecks(false): %v", err)
	}
	enabled, err := s.DependencyChecksEnabled(ctx, app, "prod")
	if err != nil {
		t.Fatalf("DependencyChecksEnabled: %v", err)
	}
	if enabled {
		t.Error("the disable did not take")
	}

	if err := s.SetDependencyChecks(ctx, app, "prod", true, at.Add(time.Hour)); err != nil {
		t.Fatalf("SetDependencyChecks(true): %v", err)
	}
	enabled, err = s.DependencyChecksEnabled(ctx, app, "prod")
	if err != nil {
		t.Fatalf("DependencyChecksEnabled: %v", err)
	}
	if !enabled {
		t.Error("the re-enable did not take")
	}
}

// TestStoreDependencyChecksAreKeyedByEnvironment: the same app in two environments has different
// dependencies, because there is one Postgres instance per environment (ADR-0067 §1). Turning the
// check off in one must not reach the other.
func TestStoreDependencyChecksAreKeyedByEnvironment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := checksApp(t, "web")
	at := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)

	if err := s.SetDependencyChecks(ctx, app, "staging", false, at); err != nil {
		t.Fatalf("SetDependencyChecks: %v", err)
	}
	prod, err := s.DependencyChecksEnabled(ctx, app, "prod")
	if err != nil {
		t.Fatalf("DependencyChecksEnabled(prod): %v", err)
	}
	if !prod {
		t.Error("disabling in staging turned the check off in production too")
	}
	staging, err := s.DependencyChecksEnabled(ctx, app, "staging")
	if err != nil {
		t.Fatalf("DependencyChecksEnabled(staging): %v", err)
	}
	if staging {
		t.Error("the staging setting did not take")
	}
}

// TestStoreDeleteDependencyCheckSettings is the durable side of an app teardown: an app created later
// under the same name must start checked rather than inherit a stranger's opt-out.
func TestStoreDeleteDependencyCheckSettings(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := checksApp(t, "web")
	at := time.Date(2026, 7, 31, 3, 0, 0, 0, time.UTC)

	if err := s.SetDependencyChecks(ctx, app, "prod", false, at); err != nil {
		t.Fatalf("SetDependencyChecks: %v", err)
	}
	if err := s.SetDependencyChecks(ctx, app, "staging", false, at); err != nil {
		t.Fatalf("SetDependencyChecks: %v", err)
	}
	if err := s.DeleteDependencyCheckSettings(ctx, app); err != nil {
		t.Fatalf("DeleteDependencyCheckSettings: %v", err)
	}
	for _, env := range []string{"prod", "staging"} {
		enabled, err := s.DependencyChecksEnabled(ctx, app, env)
		if err != nil {
			t.Fatalf("DependencyChecksEnabled(%s): %v", env, err)
		}
		if !enabled {
			t.Errorf("%s kept the opt-out after the app was deleted", env)
		}
	}
	// Deleting for an app that never recorded one is a no-op, not an error.
	if err := s.DeleteDependencyCheckSettings(ctx, checksApp(t, "never-existed")); err != nil {
		t.Errorf("DeleteDependencyCheckSettings for an unknown app: %v", err)
	}
}
