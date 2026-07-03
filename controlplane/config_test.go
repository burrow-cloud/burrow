// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestSetConfigPersistsAndLists(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	// No running release yet: set still persists and is a no-op apply, not an error.
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("SetConfig (no release): %v", err)
	}
	cfg, err := e.ListConfig(ctx, "web", "")
	if err != nil {
		t.Fatalf("ListConfig: %v", err)
	}
	if cfg["LOG_LEVEL"] != "debug" {
		t.Errorf("config = %+v, want LOG_LEVEL=debug", cfg)
	}
}

func TestSetConfigRollsRunningWorkload(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// A default set re-applies the workload: the new value appears in the live spec.
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("SetConfig: %v", err)
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

func TestSetConfigNoRestartDoesNotRoll(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// --no-restart persists but does not re-apply: the live spec keeps the old (empty) env.
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true); err != nil {
		t.Fatalf("SetConfig no-restart: %v", err)
	}
	spec, _ := k.Spec("web")
	if _, present := spec.Env["LOG_LEVEL"]; present {
		t.Errorf("spec env = %+v, want LOG_LEVEL absent until the next deploy", spec.Env)
	}
	// But it is persisted in the store.
	cfg, _ := e.ListConfig(ctx, "web", "")
	if cfg["LOG_LEVEL"] != "debug" {
		t.Errorf("store config = %+v, want LOG_LEVEL=debug persisted", cfg)
	}

	// The next deploy picks it up from the store.
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("Deploy v2: %v", err)
	}
	spec, _ = k.Spec("web")
	if spec.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("after deploy, spec env = %+v, want LOG_LEVEL=debug from the store", spec.Env)
	}
}

func TestUnsetConfigRemovesAndRolls(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if err := e.SetConfig(ctx, "web", "", "A", "1", true); err != nil {
		t.Fatalf("SetConfig A: %v", err)
	}
	if err := e.SetConfig(ctx, "web", "", "B", "2", true); err != nil {
		t.Fatalf("SetConfig B: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := e.UnsetConfig(ctx, "web", "", "A", false); err != nil {
		t.Fatalf("UnsetConfig: %v", err)
	}
	cfg, _ := e.ListConfig(ctx, "web", "")
	if _, present := cfg["A"]; present {
		t.Errorf("store config = %+v, want A removed", cfg)
	}
	if cfg["B"] != "2" {
		t.Errorf("store config = %+v, want B=2 retained", cfg)
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

func TestConfigInvalidKey(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	if err := e.SetConfig(ctx, "web", "", "1BAD", "x", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetConfig bad key err = %v, want ErrInvalid", err)
	}
	if err := e.SetConfig(ctx, "web", "", "has-dash", "x", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetConfig dashed key err = %v, want ErrInvalid", err)
	}
	if err := e.UnsetConfig(ctx, "web", "", "", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("UnsetConfig empty key err = %v, want ErrInvalid", err)
	}
	if _, err := e.ListConfig(ctx, "BadApp!", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("ListConfig bad app err = %v, want ErrInvalid", err)
	}
}

func TestRollbackRendersCurrentStoreConfig(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())

	if err := e.SetConfig(ctx, "web", "", "A", "1", true); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy v1: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("Deploy v2: %v", err)
	}
	// Change config after v2 but before rollback: the rollback must render the CURRENT store
	// config, not whatever v1 had snapshotted.
	if err := e.SetConfig(ctx, "web", "", "A", "2", true); err != nil {
		t.Fatalf("SetConfig after v2: %v", err)
	}

	res, err := e.Rollback(ctx, "web", "", false)
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
