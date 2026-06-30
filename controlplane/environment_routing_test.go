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
// engine, the fake cluster, the registry, and the database so a test can drive a per-app operation
// into a named environment and inspect which namespace it landed in (ADR-0035 phase 2b).
func newRoutingEngine(t *testing.T, appNamespace string) (*cp.Engine, *fake.Kubernetes, *fake.Registry, *fake.Database) {
	t.Helper()
	k := fake.NewKubernetes()
	r := fake.NewRegistry()
	d := fake.NewDatabase()
	p := cp.DefaultPolicy()
	p.MaxReplicas = 1000
	d.SetPolicy(p.With(cp.GuardrailScaleToZero, cp.DispositionAllow))
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Registry: r, Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		AppNamespace: appNamespace,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, r, d
}

// TestDeployRoutesToEnvironmentNamespace confirms a deploy targeting a registered environment lands
// in that environment's namespace, leaving the default environment's namespace untouched (ADR-0035
// phase 2b).
func TestDeployRoutesToEnvironmentNamespace(t *testing.T) {
	ctx := context.Background()
	e, k, r, _ := newRoutingEngine(t, "burrow-apps")
	r.Add("registry.example.com/web:1", "sha256:web1")
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

	// The same app name deploys independently into the default environment.
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 2}); err != nil {
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
	e, k, r, _ := newRoutingEngine(t, "burrow-apps")
	r.Add("registry.example.com/web:1", "sha256:web1")

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: "ghost", Image: "registry.example.com/web:1", Replicas: 1})
	if !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("Deploy(ghost) err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "unknown environment") {
		t.Errorf("error = %q, want it to mention the unknown environment", err)
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
	e, k, _, _ := newRoutingEngine(t, "burrow-apps")
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
	e, k, r, _ := newRoutingEngine(t, "burrow-apps")
	r.Add("registry.example.com/web:1", "sha256:web1")

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

// TestGuardedOpRecordsEnvironment confirms a guarded environment-scoped operation records the
// environment in its redacted audit args, so the trail shows which environment was touched while
// guardrail policy stays global until phase 2c (ADR-0035 phase 2b, ADR-0027).
func TestGuardedOpRecordsEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, r, d := newRoutingEngine(t, "burrow-apps")
	r.Add("registry.example.com/web:1", "sha256:web1")
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
