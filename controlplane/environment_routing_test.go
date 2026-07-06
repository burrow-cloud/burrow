// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// newRoutingEngine builds an engine with a known app namespace and a permissive policy, returning the
// engine, the fake cluster, and the database so a test can drive a per-app operation into a named
// environment and inspect which namespace it landed in (ADR-0035 phase 2b).
func newRoutingEngine(t *testing.T, appNamespace string) (*cp.Engine, *fake.Kubernetes, *fake.Database) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	p := cp.DefaultPolicy()
	p.MaxReplicas = 1000
	d.SetPolicy(p.With(cp.GuardrailScaleToZero, cp.DispositionAllow))
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		AppNamespace: appNamespace,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, d
}

// TestDeployRoutesToEnvironmentNamespace confirms a deploy targeting a registered environment lands
// in that environment's namespace, leaving the default environment's namespace untouched (ADR-0035
// phase 2b).
func TestDeployRoutesToEnvironmentNamespace(t *testing.T) {
	ctx := context.Background()
	e, k, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: "staging", Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy(staging): %v", err)
	}

	// The workload landed in staging's namespace, not the default app namespace.
	if _, ok := k.SpecInNamespace("burrow-apps-staging", "web"); !ok {
		t.Errorf("workload not found in staging namespace burrow-apps-staging")
	}
	if _, ok := k.SpecInNamespace("burrow-apps", "web"); ok {
		t.Errorf("workload unexpectedly present in the default app namespace burrow-apps")
	}

	// The same app name deploys independently into the default environment. With staging also
	// registered the target is ambiguous, so the default must be named explicitly (ADR-0047 §1).
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: cp.DefaultEnvironment, Image: "registry.example.com/web:1", Replicas: 2}); err != nil {
		t.Fatalf("Deploy(default): %v", err)
	}
	def, ok := k.SpecInNamespace("burrow-apps", "web")
	if !ok || def.Replicas != 2 {
		t.Errorf("default-env workload = %+v ok=%v, want 2 replicas in burrow-apps", def, ok)
	}
	stg, ok := k.SpecInNamespace("burrow-apps-staging", "web")
	if !ok || stg.Replicas != 1 {
		t.Errorf("staging workload = %+v ok=%v, want 1 replica in burrow-apps-staging", stg, ok)
	}
}

// TestUnknownEnvironmentIsAClearError confirms an operation naming an unregistered environment fails
// with a clear ErrNotFound before any cluster write (ADR-0035 phase 2b).
func TestUnknownEnvironmentIsAClearError(t *testing.T) {
	ctx := context.Background()
	e, k, _ := newRoutingEngine(t, "burrow-apps")

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: "ghost", Image: "registry.example.com/web:1", Replicas: 1})
	if !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("Deploy(ghost) err = %v, want ErrNotFound", err)
	}
	// The error is actionable (ADR-0006): it names the unknown environment and tells the caller the
	// exact command to register it, so an agent can guide the user to `burrow env add <name>`.
	msg := err.Error()
	if !strings.Contains(msg, "unknown environment") {
		t.Errorf("error = %q, want it to mention the unknown environment", err)
	}
	if !strings.Contains(msg, "ghost") {
		t.Errorf("error = %q, want it to name the environment %q", err, "ghost")
	}
	if !strings.Contains(msg, "burrow env add ghost") {
		t.Errorf("error = %q, want it to tell the caller to run `burrow env add ghost`", err)
	}
	// Nothing was applied to any namespace.
	if _, ok := k.SpecInNamespace("ghost", "web"); ok {
		t.Errorf("a workload was applied despite the unknown environment")
	}

	// A read operation reports the same error.
	if _, err := e.Status(ctx, "web", "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Status(ghost) err = %v, want ErrNotFound", err)
	}
}

// TestSecretLandsInEnvironmentNamespace confirms a per-app secret set against a named environment is
// written into that environment's namespace, and a list against another environment does not see it
// (ADR-0035 phase 2b).
func TestSecretLandsInEnvironmentNamespace(t *testing.T) {
	ctx := context.Background()
	e, k, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	if err := e.SetSecret(ctx, "web", "staging", "DATABASE_URL", "postgres://staging", true); err != nil {
		t.Fatalf("SetSecret(staging): %v", err)
	}

	if v, ok := k.SecretValueInNamespace("burrow-apps-staging", "web", "DATABASE_URL"); !ok || v != "postgres://staging" {
		t.Errorf("staging secret = %q ok=%v, want postgres://staging in burrow-apps-staging", v, ok)
	}
	if _, ok := k.SecretValueInNamespace("burrow-apps", "web", "DATABASE_URL"); ok {
		t.Errorf("secret unexpectedly present in the default app namespace burrow-apps")
	}

	// A list scoped to staging sees the key; a list scoped to the default environment does not.
	stgKeys, err := e.ListSecrets(ctx, "web", "staging")
	if err != nil {
		t.Fatalf("ListSecrets(staging): %v", err)
	}
	if len(stgKeys) != 1 || stgKeys[0] != "DATABASE_URL" {
		t.Errorf("staging secret keys = %v, want [DATABASE_URL]", stgKeys)
	}
	defKeys, err := e.ListSecrets(ctx, "web", "")
	if err != nil {
		t.Fatalf("ListSecrets(default): %v", err)
	}
	if len(defKeys) != 0 {
		t.Errorf("default secret keys = %v, want none", defKeys)
	}
}

// TestDefaultEnvironmentResolvesToAppNamespace confirms an empty env and the reserved "default" both
// resolve to the engine's app namespace, the implicit default environment (ADR-0035 phase 2b).
func TestDefaultEnvironmentResolvesToAppNamespace(t *testing.T) {
	ctx := context.Background()
	e, k, _ := newRoutingEngine(t, "burrow-apps")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "empty", Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy(empty env): %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "named", Env: "default", Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy(default env): %v", err)
	}
	if _, ok := k.SpecInNamespace("burrow-apps", "empty"); !ok {
		t.Errorf("empty-env deploy did not land in the app namespace burrow-apps")
	}
	if _, ok := k.SpecInNamespace("burrow-apps", "named"); !ok {
		t.Errorf("default-env deploy did not land in the app namespace burrow-apps")
	}
}

// TestPerEnvironmentGuardrailGatesOnlyThatEnv is the headline of ADR-0035 phase 2c: a prod-scoped
// rule denies an operation in prod while the same operation runs in another environment under the
// permissive global policy. It locks app.delete in prod, leaves it allowed globally, then deletes the
// same app in staging (allowed) and prod (denied).
func TestPerEnvironmentGuardrailGatesOnlyThatEnv(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	for _, env := range []string{"prod", "staging"} {
		if _, err := e.AddEnvironment(ctx, env, "burrow-apps-"+env); err != nil {
			t.Fatalf("AddEnvironment(%s): %v", env, err)
		}
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: env, Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
			t.Fatalf("Deploy(%s): %v", env, err)
		}
	}

	// Permissive globally, locked in prod.
	if err := e.SetGuardrail(ctx, "", cp.GuardrailAppDelete, cp.DispositionAllow); err != nil {
		t.Fatalf("SetGuardrail(global): %v", err)
	}
	if err := e.SetGuardrail(ctx, "prod", cp.GuardrailAppDelete, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(prod): %v", err)
	}

	// staging inherits the permissive global rule: the delete runs.
	if err := e.DeleteApp(ctx, "web", "staging", false); err != nil {
		t.Errorf("DeleteApp(staging) = %v, want it to proceed under the global allow", err)
	}
	// prod has its own deny: the same delete is refused outright.
	err := e.DeleteApp(ctx, "web", "prod", false)
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("DeleteApp(prod) = %v, want a GuardrailError", err)
	}
	if g.Code != cp.GuardrailAppDelete || g.NeedsConfirmation {
		t.Errorf("prod delete guardrail = %+v, want a plain deny on app.delete", g)
	}
}

// TestSetGuardrailEnvValidation confirms env-scoping is validated: a valid app-level set against a
// registered env stores the env-prefixed code, an unknown env is ErrNotFound, and a cluster-level
// guardrail cannot be env-scoped (ADR-0035 phase 2c).
func TestSetGuardrailEnvValidation(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	// A registered env + app-level code stores the env-prefixed disposition, visible in prod's listing.
	if err := e.SetGuardrail(ctx, "prod", cp.GuardrailAppDelete, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(prod, app.delete): %v", err)
	}
	gs, err := e.Guardrails(ctx, "prod")
	if err != nil {
		t.Fatalf("Guardrails(prod): %v", err)
	}
	var saw bool
	for _, g := range gs {
		if g.Code == cp.GuardrailAppDelete {
			saw = true
			if g.Disposition != cp.DispositionDeny || g.Source != "env" {
				t.Errorf("prod app.delete = (%q, %q), want (deny, env)", g.Disposition, g.Source)
			}
		}
	}
	if !saw {
		t.Errorf("prod listing missing app.delete")
	}

	// An unknown environment is a clear ErrNotFound (catches typos).
	if err := e.SetGuardrail(ctx, "ghost", cp.GuardrailAppDelete, cp.DispositionDeny); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("SetGuardrail(ghost) = %v, want ErrNotFound", err)
	}
	// A cluster-level guardrail cannot be scoped to an environment.
	if err := e.SetGuardrail(ctx, "prod", cp.GuardrailAddonInstall, cp.DispositionDeny); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetGuardrail(prod, addon.install) = %v, want ErrInvalid (cluster-level)", err)
	}
	if err := e.SetGuardrail(ctx, "prod", cp.GuardrailDNSWrite, cp.DispositionDeny); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetGuardrail(prod, dns.write) = %v, want ErrInvalid (cluster-level)", err)
	}

	// Listing an unknown environment is likewise a clear error.
	if _, err := e.Guardrails(ctx, "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Guardrails(ghost) = %v, want ErrNotFound", err)
	}
}

// TestClusterLevelGuardrailIgnoresEnv confirms a cluster-level operation (an add-on install) is
// gated by the global disposition regardless of any environment, since it is set without env and
// evaluated with an empty env (ADR-0035 phase 2c).
func TestClusterLevelGuardrailIgnoresEnv(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	// Deny add-on install globally; there is no way to make it depend on an environment.
	if err := e.SetGuardrail(ctx, "", cp.GuardrailAddonInstall, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(addon.install): %v", err)
	}
	_, err := e.InstallAddon(ctx, cp.AddonLogs, false)
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("InstallAddon = %v, want a GuardrailError from the global deny", err)
	}
	if g.Code != cp.GuardrailAddonInstall {
		t.Errorf("guardrail code = %q, want addon.install", g.Code)
	}
}

// TestGuardedOpRecordsEnvironment confirms a guarded environment-scoped operation records the
// environment in its redacted audit args, so the trail shows which environment was touched while
// guardrail policy stays global until phase 2c (ADR-0035 phase 2b, ADR-0027).
func TestGuardedOpRecordsEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, d := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: "staging", Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy(staging): %v", err)
	}

	var found bool
	for _, row := range d.AuditRows() {
		if row.Operation == "deploy" && row.Args["env"] == "staging" {
			found = true
		}
	}
	if !found {
		t.Errorf("no deploy audit row recorded env=staging; rows = %+v", d.AuditRows())
	}
}
