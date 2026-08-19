// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

func TestSetConfigPersistsAndLists(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	// No running release yet: set still persists and is a no-op apply, not an error.
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", false, false); err != nil {
		t.Fatalf("SetConfig (no release): %v", err)
	}
	cfg, err := e.ListConfig(ctx, "web", cp.DefaultEnvironment)
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
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", false, false); err != nil {
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
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true, false); err != nil {
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
	if err := e.SetConfig(ctx, "web", "", "A", "1", true, false); err != nil {
		t.Fatalf("SetConfig A: %v", err)
	}
	if err := e.SetConfig(ctx, "web", "", "B", "2", true, false); err != nil {
		t.Fatalf("SetConfig B: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := e.UnsetConfig(ctx, "web", "", "A", false, false); err != nil {
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

	if err := e.SetConfig(ctx, "web", "", "1BAD", "x", true, false); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetConfig bad key err = %v, want ErrInvalid", err)
	}
	if err := e.SetConfig(ctx, "web", "", "has-dash", "x", true, false); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetConfig dashed key err = %v, want ErrInvalid", err)
	}
	if err := e.UnsetConfig(ctx, "web", "", "", true, false); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("UnsetConfig empty key err = %v, want ErrInvalid", err)
	}
	if _, err := e.ListConfig(ctx, "BadApp!", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("ListConfig bad app err = %v, want ErrInvalid", err)
	}
}

func TestRollbackRendersCurrentStoreConfig(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())

	if err := e.SetConfig(ctx, "web", "", "A", "1", true, false); err != nil {
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
	if err := e.SetConfig(ctx, "web", "", "A", "2", true, false); err != nil {
		t.Fatalf("SetConfig after v2: %v", err)
	}

	res, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{})
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

// TestConfigWriteCarriesTheEnvironment pins what the config seam is told: a config write happens IN
// an environment — the engine resolves that environment's namespace and re-applies that
// environment's workload — so the environment travels with the write and an implementation of
// controlplane.Database can tell a staging change from a production one.
//
// The other half is that a write lands ONLY where it was made (ADR-0028): a value set while pointed
// at staging is not in the config production renders, and a removal in staging does not remove
// production's row for the same key. Both halves need two environments to be observable at all —
// with one environment every write and every read agree by default, which is exactly the state a
// test asserting nothing would also pass in.
func TestConfigWriteCarriesTheEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	// --no-restart throughout: this is about the store call, and no app is deployed anywhere.
	if err := e.SetConfig(ctx, "web", "staging", "LOG_LEVEL", "debug", true, false); err != nil {
		t.Fatalf("SetConfig (staging): %v", err)
	}
	// The default environment is named rather than left empty: with a second environment
	// registered, Burrow refuses to pick one for a mutating operation.
	if err := e.SetConfig(ctx, "web", cp.DefaultEnvironment, "REGION", "eu", true, false); err != nil {
		t.Fatalf("SetConfig (default environment): %v", err)
	}
	if err := e.UnsetConfig(ctx, "web", "staging", "REGION", true, false); err != nil {
		t.Fatalf("UnsetConfig (staging): %v", err)
	}

	want := []fake.AppEnvWrite{
		{App: "web", Env: "staging", Key: "LOG_LEVEL"},
		{App: "web", Env: cp.DefaultEnvironment, Key: "REGION"},
		{App: "web", Env: "staging", Key: "REGION", Unset: true},
	}
	got := d.AppEnvWrites()
	if len(got) != len(want) {
		t.Fatalf("writes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("write %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Staging holds what staging set, and nothing the default environment set.
	staging, err := e.ListConfig(ctx, "web", "staging")
	if err != nil {
		t.Fatalf("ListConfig (staging): %v", err)
	}
	if staging["LOG_LEVEL"] != "debug" {
		t.Errorf("staging config = %+v, want the staging write LOG_LEVEL=debug", staging)
	}
	if _, present := staging["REGION"]; present {
		t.Errorf("staging config = %+v, want REGION absent — it was set in %s", staging, cp.DefaultEnvironment)
	}

	// The default environment holds its own write, is untouched by the staging one, and STILL HOLDS
	// REGION: the unset above named staging, where REGION was never set, so it removed nothing here.
	// That is the assertion an app-global store cannot pass — under it the removal would have taken
	// this row with it.
	def, err := e.ListConfig(ctx, "web", cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("ListConfig (%s): %v", cp.DefaultEnvironment, err)
	}
	if def["REGION"] != "eu" {
		t.Errorf("%s config = %+v, want REGION=eu to survive a staging unset", cp.DefaultEnvironment, def)
	}
	if _, present := def["LOG_LEVEL"]; present {
		t.Errorf("%s config = %+v, want LOG_LEVEL absent — it was set in staging", cp.DefaultEnvironment, def)
	}
}

// TestDeployRendersOnlyTheTargetEnvironmentsConfig is the read half of ADR-0028's per-environment
// config, asserted where it decides what a running app sees: the workload deploy renders.
//
// It needs two environments and both directions, because either alone passes under the old
// app-global store: an app deployed to staging must come up with staging's value and WITHOUT the
// default environment's, and the app deployed to the default environment must still come up with
// its own. A read that silently returned another environment's rows, or none, would be a deploy
// that came up misconfigured rather than one that failed.
func TestDeployRendersOnlyTheTargetEnvironmentsConfig(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	if err := e.SetConfig(ctx, "web", cp.DefaultEnvironment, "ENDPOINT", "https://api.example.com", true, false); err != nil {
		t.Fatalf("SetConfig (%s): %v", cp.DefaultEnvironment, err)
	}
	if err := e.SetConfig(ctx, "web", "staging", "ENDPOINT", "https://api.staging.example.com", true, false); err != nil {
		t.Fatalf("SetConfig (staging): %v", err)
	}
	// Set in the default environment only: staging must not inherit it.
	if err := e.SetConfig(ctx, "web", cp.DefaultEnvironment, "PROD_ONLY", "1", true, false); err != nil {
		t.Fatalf("SetConfig (%s, prod-only key): %v", cp.DefaultEnvironment, err)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Env: "staging"}); err != nil {
		t.Fatalf("Deploy (staging): %v", err)
	}
	spec, ok := k.SpecInNamespace("burrow-apps-staging", "web")
	if !ok {
		t.Fatal("no workload after the staging deploy")
	}
	if spec.Env["ENDPOINT"] != "https://api.staging.example.com" {
		t.Errorf("staging spec env = %+v, want staging's own ENDPOINT", spec.Env)
	}
	if _, present := spec.Env["PROD_ONLY"]; present {
		t.Errorf("staging spec env = %+v, want PROD_ONLY absent — it is set in %s only", spec.Env, cp.DefaultEnvironment)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Env: cp.DefaultEnvironment}); err != nil {
		t.Fatalf("Deploy (%s): %v", cp.DefaultEnvironment, err)
	}
	spec, ok = k.Spec("web")
	if !ok {
		t.Fatalf("no workload after the %s deploy", cp.DefaultEnvironment)
	}
	if spec.Env["ENDPOINT"] != "https://api.example.com" {
		t.Errorf("%s spec env = %+v, want its own ENDPOINT", cp.DefaultEnvironment, spec.Env)
	}
	if spec.Env["PROD_ONLY"] != "1" {
		t.Errorf("%s spec env = %+v, want PROD_ONLY=1", cp.DefaultEnvironment, spec.Env)
	}
}

// TestANewEnvironmentStartsWithNoConfig states, as a test rather than only as a comment, what a user
// meets the moment they add an environment: nothing carries over. There is no wildcard scope and no
// fall-back, so an app with config in the default environment has NONE in a freshly registered one
// until it is set there, and a deploy into it renders an empty environment rather than inheriting.
func TestANewEnvironmentStartsWithNoConfig(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true, false); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	cfg, err := e.ListConfig(ctx, "web", "staging")
	if err != nil {
		t.Fatalf("ListConfig (staging): %v", err)
	}
	if len(cfg) != 0 {
		t.Errorf("staging config = %+v, want empty — a new environment starts with no config", cfg)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Env: "staging"}); err != nil {
		t.Fatalf("Deploy (staging): %v", err)
	}
	spec, ok := k.SpecInNamespace("burrow-apps-staging", "web")
	if !ok {
		t.Fatal("no workload after the staging deploy")
	}
	if _, present := spec.Env["LOG_LEVEL"]; present {
		t.Errorf("staging spec env = %+v, want LOG_LEVEL absent — it was set in %s", spec.Env, cp.DefaultEnvironment)
	}
}

// TestConfigWriteCarriesTheCanonicalEnvironmentName pins the OTHER half of the seam's contract: the
// name that arrives is canonical (ADR-0067 §2), so an implementation keying anything off it never
// has to know that an empty selector and "prod" are the same environment. A caller may leave the
// environment unnamed when there is only one, and that is the case this covers — with a second
// environment registered Burrow refuses to choose for a mutating operation at all.
func TestConfigWriteCarriesTheCanonicalEnvironmentName(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())

	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true, false); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := e.UnsetConfig(ctx, "web", "", "LOG_LEVEL", true, false); err != nil {
		t.Fatalf("UnsetConfig: %v", err)
	}
	for _, w := range d.AppEnvWrites() {
		if w.Env != cp.DefaultEnvironment {
			t.Errorf("write %+v carried %q, want the canonical %q", w, w.Env, cp.DefaultEnvironment)
		}
	}
}
