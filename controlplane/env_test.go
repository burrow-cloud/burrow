// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestSetEnvPersistsAndLists(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newEngine(t, permissive())

	// No running release yet: set still persists and is a no-op apply, not an error.
	if err := e.SetEnv(ctx, "web", "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("SetEnv (no release): %v", err)
	}
	env, err := e.ListEnv(ctx, "web")
	if err != nil {
		t.Fatalf("ListEnv: %v", err)
	}
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("env = %+v, want LOG_LEVEL=debug", env)
	}
}

func TestSetEnvRollsRunningWorkload(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// A default set re-applies the workload: the new env appears in the live spec.
	if err := e.SetEnv(ctx, "web", "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	spec, ok := k.Spec("web")
	if !ok {
		t.Fatal("no workload after set")
	}
	if spec.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("spec env = %+v, want LOG_LEVEL=debug after a restarting set", spec.Env)
	}
	// The re-apply preserves the running release's image and replicas.
	if spec.Image != "img:1" || spec.Replicas != 2 {
		t.Errorf("spec = %+v, want image img:1 x2 preserved", spec)
	}
}

func TestSetEnvNoRestartDoesNotRoll(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// --no-restart persists but does not re-apply: the live spec keeps the old (empty) env.
	if err := e.SetEnv(ctx, "web", "LOG_LEVEL", "debug", true); err != nil {
		t.Fatalf("SetEnv no-restart: %v", err)
	}
	spec, _ := k.Spec("web")
	if _, present := spec.Env["LOG_LEVEL"]; present {
		t.Errorf("spec env = %+v, want LOG_LEVEL absent until the next deploy", spec.Env)
	}
	// But it is persisted in the store.
	env, _ := e.ListEnv(ctx, "web")
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("store env = %+v, want LOG_LEVEL=debug persisted", env)
	}

	// The next deploy picks it up from the store.
	r.Add("img:2", "sha256:2")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("Deploy v2: %v", err)
	}
	spec, _ = k.Spec("web")
	if spec.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("after deploy, spec env = %+v, want LOG_LEVEL=debug from the store", spec.Env)
	}
}

func TestUnsetEnvRemovesAndRolls(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	if err := e.SetEnv(ctx, "web", "A", "1", true); err != nil {
		t.Fatalf("SetEnv A: %v", err)
	}
	if err := e.SetEnv(ctx, "web", "B", "2", true); err != nil {
		t.Fatalf("SetEnv B: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := e.UnsetEnv(ctx, "web", "A", false); err != nil {
		t.Fatalf("UnsetEnv: %v", err)
	}
	env, _ := e.ListEnv(ctx, "web")
	if _, present := env["A"]; present {
		t.Errorf("store env = %+v, want A removed", env)
	}
	if env["B"] != "2" {
		t.Errorf("store env = %+v, want B=2 retained", env)
	}
	// The running workload rolled with A gone.
	spec, _ := k.Spec("web")
	if _, present := spec.Env["A"]; present {
		t.Errorf("spec env = %+v, want A absent after unset", spec.Env)
	}
	if spec.Env["B"] != "2" {
		t.Errorf("spec env = %+v, want B=2", spec.Env)
	}
}

func TestEnvInvalidKey(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newEngine(t, permissive())

	if err := e.SetEnv(ctx, "web", "1BAD", "x", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetEnv bad key err = %v, want ErrInvalid", err)
	}
	if err := e.SetEnv(ctx, "web", "has-dash", "x", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetEnv dashed key err = %v, want ErrInvalid", err)
	}
	if err := e.UnsetEnv(ctx, "web", "", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("UnsetEnv empty key err = %v, want ErrInvalid", err)
	}
	if _, err := e.ListEnv(ctx, "BadApp!"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("ListEnv bad app err = %v, want ErrInvalid", err)
	}
}

func TestRollbackRendersCurrentStoreEnv(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	r.Add("img:2", "sha256:2")

	if err := e.SetEnv(ctx, "web", "A", "1", true); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy v1: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("Deploy v2: %v", err)
	}
	// Change env after v2 but before rollback: the rollback must render the CURRENT store env,
	// not whatever v1 had snapshotted.
	if err := e.SetEnv(ctx, "web", "A", "2", true); err != nil {
		t.Fatalf("SetEnv after v2: %v", err)
	}

	res, err := e.Rollback(ctx, "web", false)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.Release.Image != "img:1" {
		t.Errorf("rollback image = %q, want img:1", res.Release.Image)
	}
	spec, _ := k.Spec("web")
	if spec.Image != "img:1" {
		t.Errorf("spec image = %q, want img:1", spec.Image)
	}
	if spec.Env["A"] != "2" {
		t.Errorf("spec env = %+v, want A=2 (current store value), not the v1 snapshot", spec.Env)
	}
}
