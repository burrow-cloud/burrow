// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// assertAmbiguous asserts err is an AmbiguousEnvironmentError that lists (and names in its message)
// each expected environment, so the agent can re-issue the call with an explicit target (ADR-0047 §1).
func assertAmbiguous(t *testing.T, err error, wantNames ...string) {
	t.Helper()
	a, ok := cp.AsAmbiguousEnvironment(err)
	if !ok {
		t.Fatalf("err = %v, want AmbiguousEnvironmentError", err)
	}
	listed := map[string]bool{}
	for _, env := range a.Environments {
		listed[env.Name] = true
	}
	msg := a.Error()
	for _, n := range wantNames {
		if !listed[n] {
			t.Errorf("AmbiguousEnvironmentError does not list %q (has %+v)", n, a.Environments)
		}
		if !strings.Contains(msg, n) {
			t.Errorf("error message %q does not name %q", msg, n)
		}
	}
}

// TestMutatingRefusesAmbiguousEnvironment confirms a mutating operation with no environment named is
// refused with the structured AmbiguousEnvironmentError once more than one environment is registered
// (the implicit default plus a named one) — burrowd will not silently pick the default (ADR-0047 §1).
func TestMutatingRefusesAmbiguousEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	// Deploy with no env is refused: default + staging are both registered.
	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 1})
	assertAmbiguous(t, err, "default", "staging")

	// Delete likewise. The guard runs before the app-existence check, so even a missing app refuses on
	// the ambiguous target rather than reporting not-found — the target question is answered first.
	assertAmbiguous(t, e.DeleteApp(ctx, "web", "", false), "default", "staging")
}

// TestSingleEnvironmentMutatesWithoutEnv confirms the common single-environment self-hoster is
// unaffected: with only the implicit default registered there is no ambiguity, so a bare mutating call
// proceeds against it with no forcing function (ADR-0047 §2).
func TestSingleEnvironmentMutatesWithoutEnv(t *testing.T) {
	ctx := context.Background()
	e, k, _ := newRoutingEngine(t, "burrow-apps")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy with no env and a single environment: %v", err)
	}
	if _, ok := k.SpecInNamespace("burrow-apps", "web"); !ok {
		t.Errorf("workload not found in default namespace burrow-apps")
	}
}

// TestReadOnlyExemptFromAmbiguityGuard confirms a read-only survey stays frictionless even with
// several environments registered: it may default to the current context and is never refused for an
// ambiguous target (ADR-0047 §3).
func TestReadOnlyExemptFromAmbiguityGuard(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	if _, err := e.ListApps(ctx, ""); err != nil {
		t.Fatalf("ListApps with no env and multiple environments: %v", err)
	}
	if _, err := e.Status(ctx, "web", ""); err != nil {
		if _, ok := cp.AsAmbiguousEnvironment(err); ok {
			t.Fatalf("Status refused as ambiguous, want exempt: %v", err)
		}
	}
}
