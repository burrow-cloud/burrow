// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestStoreAppHooks exercises the app_hooks migration and its round-trip against a real database
// (ADR-0072 §1): an unset phase reads as no hook rather than an error, a set is read back with its
// argv intact, a second set replaces in place, and the hook is keyed per (app, environment, phase).
func TestStoreAppHooks(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"

	// Unset means no hook: the ordinary answer, not ErrNotFound.
	got, err := s.AppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreDeploy)
	if err != nil {
		t.Fatalf("AppHook (absent): %v", err)
	}
	if got != nil {
		t.Fatalf("absent hook = %v, want nil", got)
	}
	hooks, err := s.AppHooks(ctx, app, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AppHooks (absent): %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("hooks with none set = %+v, want an empty slice", hooks)
	}

	// A set is read back with argument boundaries intact — the reason the command is stored as an
	// argv rather than flattened to a line.
	command := []string{"sh", "-c", "./manage.py migrate --noinput"}
	if err := s.SetAppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreDeploy, command); err != nil {
		t.Fatalf("SetAppHook: %v", err)
	}
	got, err = s.AppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreDeploy)
	if err != nil {
		t.Fatalf("AppHook: %v", err)
	}
	if len(got) != 3 || got[2] != "./manage.py migrate --noinput" {
		t.Fatalf("hook command = %#v, want the argv unchanged", got)
	}

	// A second set replaces in place (upsert on the (app, environment, phase) primary key).
	if err := s.SetAppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreDeploy, []string{"./migrate"}); err != nil {
		t.Fatalf("SetAppHook (update): %v", err)
	}
	if got, _ := s.AppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreDeploy); strings.Join(got, " ") != "./migrate" {
		t.Fatalf("hook after update = %v, want it replaced", got)
	}

	// The other phase is independent: the two run from different images and default oppositely (§8).
	if got, _ := s.AppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreRollback); got != nil {
		t.Fatalf("pre-rollback hook = %v, want nil: setting pre-deploy must not set it", got)
	}
	if err := s.SetAppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreRollback, []string{"./migrate", "down"}); err != nil {
		t.Fatalf("SetAppHook pre-rollback: %v", err)
	}

	// A different environment carries independent hooks.
	if err := s.SetAppHook(ctx, app, "staging", cp.HookPreDeploy, []string{"./seed"}); err != nil {
		t.Fatalf("SetAppHook staging: %v", err)
	}
	hooks, err = s.AppHooks(ctx, app, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AppHooks: %v", err)
	}
	if len(hooks) != 2 {
		t.Fatalf("hooks in %s = %+v, want 2", cp.DefaultEnvironment, hooks)
	}
	for _, h := range hooks {
		if h.App != app || h.Environment != cp.DefaultEnvironment {
			t.Errorf("hook = %+v, want it keyed to this app and environment", h)
		}
	}
	staging, err := s.AppHooks(ctx, app, "staging")
	if err != nil {
		t.Fatalf("AppHooks staging: %v", err)
	}
	if len(staging) != 1 || strings.Join(staging[0].Command, " ") != "./seed" {
		t.Fatalf("staging hooks = %+v, want only the staging command", staging)
	}

	// Unsetting one phase leaves the other, and unsetting an absent hook is a no-op.
	if err := s.UnsetAppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreDeploy); err != nil {
		t.Fatalf("UnsetAppHook: %v", err)
	}
	if err := s.UnsetAppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreDeploy); err != nil {
		t.Fatalf("UnsetAppHook (absent): %v", err)
	}
	hooks, _ = s.AppHooks(ctx, app, cp.DefaultEnvironment)
	if len(hooks) != 1 || hooks[0].Phase != cp.HookPreRollback {
		t.Fatalf("hooks after unset = %+v, want only the pre-rollback hook", hooks)
	}

	// An empty command is refused rather than stored: a hook that names nothing to run is a row that
	// would fail at deploy time, where nobody is present.
	if err := s.SetAppHook(ctx, app, cp.DefaultEnvironment, cp.HookPreDeploy, nil); err == nil {
		t.Error("SetAppHook with an empty command succeeded, want a refusal")
	}

	// Teardown removes an app's hooks across every environment.
	if err := s.DeleteAppHooks(ctx, app); err != nil {
		t.Fatalf("DeleteAppHooks: %v", err)
	}
	for _, env := range []string{cp.DefaultEnvironment, "staging"} {
		if hooks, _ := s.AppHooks(ctx, app, env); len(hooks) != 0 {
			t.Errorf("hooks in %s after delete = %+v, want none", env, hooks)
		}
	}
	if err := s.DeleteAppHooks(ctx, app); err != nil {
		t.Fatalf("DeleteAppHooks (already gone): %v", err)
	}
}
