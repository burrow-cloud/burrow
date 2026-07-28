// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestInstallCreatesOneEnvironmentNamedProd is ADR-0067 §2 as a fresh install sees it: burrowd's
// startup ensure registers exactly one environment, it is called `prod`, and it maps to the app
// namespace the install already uses (§3) rather than to a namespace derived from its name.
func TestInstallCreatesOneEnvironmentNamedProd(t *testing.T) {
	ctx := context.Background()
	e, _ := newEnvEngine(t, "burrow-apps")

	env, err := e.EnsureDefaultEnvironment(ctx)
	if err != nil {
		t.Fatalf("EnsureDefaultEnvironment: %v", err)
	}
	if env.Name != "prod" {
		t.Errorf("environment name = %q, want prod — the name is what makes `guard set --env prod app.delete allow` read as relaxing production (ADR-0067 §2)", env.Name)
	}
	if env.Namespace != "burrow-apps" {
		t.Errorf("environment namespace = %q, want burrow-apps; `prod` maps to the EXISTING app namespace, never burrow-apps-prod (ADR-0067 §3)", env.Namespace)
	}
	if !env.Default {
		t.Error("the environment install creates must be marked Default: it is what an operation naming none resolves to")
	}

	envs, err := e.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("environments after install = %+v, want exactly one", envs)
	}
	if envs[0].Name != cp.DefaultEnvironment || envs[0].Namespace != "burrow-apps" || !envs[0].Default {
		t.Errorf("listed environment = %+v, want prod -> burrow-apps, marked default", envs[0])
	}

	// One environment means no ambiguity: the single-environment self-hoster never types --env
	// (ADR-0047 §2, ADR-0067 §4).
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
		t.Errorf("bare deploy after install = %v, want it to proceed against the one environment", err)
	}
}

// TestEnsureDefaultEnvironmentIsIdempotent covers the re-run, the restart and the upgrade, which are
// the same call: burrowd ensures the environment on every start, so it must converge rather than
// duplicate or fail the second time.
func TestEnsureDefaultEnvironmentIsIdempotent(t *testing.T) {
	ctx := context.Background()
	e, _ := newEnvEngine(t, "burrow-apps")

	for i := 0; i < 3; i++ {
		env, err := e.EnsureDefaultEnvironment(ctx)
		if err != nil {
			t.Fatalf("EnsureDefaultEnvironment run %d: %v", i+1, err)
		}
		if env.Name != cp.DefaultEnvironment || env.Namespace != "burrow-apps" {
			t.Fatalf("run %d returned %+v, want the same prod -> burrow-apps mapping", i+1, env)
		}
	}
	envs, err := e.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 1 {
		t.Errorf("environments after three ensures = %+v, want exactly one (re-install must not duplicate)", envs)
	}

	// `env add prod` is refused for the same reason: it exists already.
	if _, err := e.AddEnvironment(ctx, cp.DefaultEnvironment, "somewhere-else"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("AddEnvironment(%s) = %v, want ErrInvalid", cp.DefaultEnvironment, err)
	}
	// So is the retired name, which nothing resolves through any more.
	if _, err := e.AddEnvironment(ctx, "default", "burrow-apps-default"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("AddEnvironment(default) = %v, want ErrInvalid (the retired name is reserved)", err)
	}
}

// TestExistingInstallAcquiresProdWithoutMoving is the upgrade case, and the one most able to break
// someone. An install predating ADR-0067 has apps in `burrow-apps`, a Postgres instance called
// `burrow-postgres`, and NO environment row at all — the first environment was synthesized, never
// stored. After the ensure it has an environment named `prod`, and every one of those things is
// exactly where it was.
func TestExistingInstallAcquiresProdWithoutMoving(t *testing.T) {
	ctx := context.Background()
	e, k, _ := newRoutingEngine(t, "burrow-apps")

	// Before: an app deployed with no environment named, as every pre-ADR-0067 deploy was.
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
		t.Fatalf("pre-upgrade deploy: %v", err)
	}
	if _, ok := k.SpecInNamespace("burrow-apps", "web"); !ok {
		t.Fatal("pre-upgrade deploy did not land in burrow-apps")
	}

	// The upgrade: burrowd starts under the new version and ensures the environment.
	env, err := e.EnsureDefaultEnvironment(ctx)
	if err != nil {
		t.Fatalf("EnsureDefaultEnvironment on an existing install: %v", err)
	}
	if env.Name != cp.DefaultEnvironment || env.Namespace != "burrow-apps" {
		t.Fatalf("backfilled environment = %+v, want prod -> the namespace the apps are already in", env)
	}

	// After: the app still resolves, from a bare call and from the environment by name, and both
	// reach the same namespace — nothing was re-pointed.
	if _, err := e.Status(ctx, "web", ""); err != nil {
		t.Errorf("bare status after the upgrade = %v, want the app to still resolve", err)
	}
	if _, err := e.Status(ctx, "web", cp.DefaultEnvironment); err != nil {
		t.Errorf("status --env prod after the upgrade = %v, want the same app", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: cp.DefaultEnvironment, Image: "registry.example.com/web:2", Replicas: 1}); err != nil {
		t.Fatalf("deploy --env prod after the upgrade: %v", err)
	}
	if _, ok := k.SpecInNamespace("burrow-apps", "web"); !ok {
		t.Error("a deploy naming prod left burrow-apps; the environment's name changed, its namespace did not (ADR-0067 §3)")
	}
	if _, ok := k.SpecInNamespace("burrow-apps-prod", "web"); ok {
		t.Error("a deploy naming prod created burrow-apps-prod; §3 maps prod to the EXISTING app namespace precisely so nothing moves")
	}

	// And the add-on instance keeps the name it has in the cluster: `burrow-postgres`, not
	// `burrow-postgres-prod`. This is the assertion that says no live Deployment, PVC or superuser
	// Secret was renamed by the environment being renamed.
	name, err := cp.AddonInstanceName(cp.AddonPostgres, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	if name != "burrow-postgres" {
		t.Errorf("postgres instance for the default environment = %q, want burrow-postgres (an existing install must not migrate)", name)
	}
}

// TestEnsureDefaultEnvironmentRefusesAConflictingProd covers the install that already used `prod` for
// a namespace of its own. Repointing it would send every unqualified operation somewhere other than
// where this control plane deploys apps, so the ensure refuses and names the conflict rather than
// choosing. (Migration 00018 refuses the same case first; this is the second net.)
func TestEnsureDefaultEnvironmentRefusesAConflictingProd(t *testing.T) {
	ctx := context.Background()
	e, d := newEnvEngine(t, "burrow-apps")

	// Registered straight into the store, since AddEnvironment now rejects the name.
	if err := d.CreateEnvironment(ctx, cp.DefaultEnvironment, "burrow-apps-prod"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	_, err := e.EnsureDefaultEnvironment(ctx)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("EnsureDefaultEnvironment with a conflicting prod = %v, want ErrInvalid", err)
	}
}
