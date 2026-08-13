// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestDeleteApp tears down the workload, routing, and release history of an existing app and
// succeeds with confirm, leaving the app unknown to both the cluster and the deploy record.
//
// app.delete is denied by default (ADR-0065 §3), so the teardown this test is about is only
// reachable once an operator relaxes the guardrail; the policy says so explicitly rather than
// leaning on a default.
func TestDeleteApp(t *testing.T) {
	e, k, d, _ := newEngine(t, permissive().With(cp.GuardrailAppDelete, cp.DispositionConfirm))
	ctx := context.Background()

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := k.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("Expose: %v", err)
	}

	if err := e.DeleteApp(ctx, "web", "", true); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	if _, err := k.WorkloadStatus(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("workload after delete: err = %v, want ErrNotFound", err)
	}
	if err := k.Unexpose(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("routing after delete: err = %v, want ErrNotFound", err)
	}
	if rels, err := d.Releases(ctx, "web", "default"); err != nil || len(rels) != 0 {
		t.Errorf("releases after delete = %v (err %v), want empty", rels, err)
	}
}

// TestDeleteAppWorkloadOnly deletes an app that has a workload but was never exposed and has no
// recorded releases — the already-absent routing is tolerated, not an error.
func TestDeleteAppWorkloadOnly(t *testing.T) {
	e, k, _, _ := newEngine(t, permissive().With(cp.GuardrailAppDelete, cp.DispositionConfirm))
	ctx := context.Background()

	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	if err := e.DeleteApp(ctx, "web", "", true); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if _, err := k.WorkloadStatus(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("workload after delete: err = %v, want ErrNotFound", err)
	}
}

// TestDeleteAppUnknown reports ErrNotFound when the app has neither releases nor a live workload.
func TestDeleteAppUnknown(t *testing.T) {
	e, _, _, _ := newEngine(t, permissive())
	if err := e.DeleteApp(context.Background(), "web", "", true); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("DeleteApp unknown err = %v, want ErrNotFound", err)
	}
}

// TestDeleteAppGuardrailHolds confirms the app.delete guardrail holds the delete for
// confirmation when not confirmed, and proceeds once confirmed.
//
// It doubles as the "an explicit disposition wins over the changed default" case (ADR-0065 §3):
// the policy carries a stored confirm, so the deny default does not reach it and an operator who
// deliberately chose confirm keeps confirm.
func TestDeleteAppGuardrailHolds(t *testing.T) {
	policy := cp.DefaultPolicy().With(cp.GuardrailAppDelete, cp.DispositionConfirm)
	e, k, _, _ := newEngine(t, policy)
	ctx := context.Background()

	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	err := e.DeleteApp(ctx, "web", "", false)
	mustGuardrail(t, err, cp.GuardrailAppDelete)
	g, _ := cp.AsGuardrail(err)
	if !g.NeedsConfirmation {
		t.Errorf("NeedsConfirmation = false, want true")
	}
	// The workload survives a held delete.
	if _, err := k.WorkloadStatus(ctx, "web"); err != nil {
		t.Errorf("workload should survive a held delete: %v", err)
	}

	if err := e.DeleteApp(ctx, "web", "", true); err != nil {
		t.Fatalf("DeleteApp confirmed: %v", err)
	}
}

// TestDeleteAppDeniedByDefault confirms app.delete is denied — not held — by the built-in policy
// (ADR-0065 §3). Deleting an app destroys its release history along with the workload and routing,
// so there is nothing to roll back to afterwards and a confirmation protects only an attentive
// reader. The refusal is a plain deny that --confirm cannot open, the app survives it, and the
// message names the per-environment lever rather than a global one.
func TestDeleteAppDeniedByDefault(t *testing.T) {
	e, k, _, _ := newEngine(t, cp.DefaultPolicy())
	ctx := context.Background()

	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	for _, confirm := range []bool{false, true} {
		err := e.DeleteApp(ctx, "web", "", confirm)
		mustGuardrail(t, err, cp.GuardrailAppDelete)
		g, _ := cp.AsGuardrail(err)
		if g.NeedsConfirmation {
			t.Errorf("confirm=%v: NeedsConfirmation = true, want a plain deny no confirm can satisfy", confirm)
		}
		if !strings.Contains(g.Message, "guard set --env") {
			t.Errorf("confirm=%v: refusal %q should point at per-environment scoping", confirm, g.Message)
		}
	}

	if _, err := k.WorkloadStatus(ctx, "web"); err != nil {
		t.Errorf("workload should survive a denied delete: %v", err)
	}
}

// TestDeleteAppDenyDefaultIsAPerEnvironmentFloor confirms the deny default is a floor, not a fixed
// setting (ADR-0065 §3): app.delete is env-scopable, so `guard set --env dev app.delete allow`
// relaxes it for one environment while every other environment keeps the default deny. This is the
// gradient the default exists to be the strict end of.
func TestDeleteAppDenyDefaultIsAPerEnvironmentFloor(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	for _, env := range []string{"dev", "staging"} {
		if _, err := e.AddEnvironment(ctx, env, "burrow-apps-"+env); err != nil {
			t.Fatalf("AddEnvironment(%s): %v", env, err)
		}
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: env, Image: "registry.example.com/web:1", Replicas: 1}); err != nil {
			t.Fatalf("Deploy(%s): %v", env, err)
		}
	}

	if err := e.SetGuardrail(asOperator(ctx), cp.GuardrailScope{Env: "dev"}, "", cp.GuardrailAppDelete, cp.DispositionAllow); err != nil {
		t.Fatalf("SetGuardrail(dev, app.delete, allow): %v", err)
	}

	// dev was relaxed deliberately: the delete runs, unconfirmed.
	if err := e.DeleteApp(ctx, "web", "dev", false); err != nil {
		t.Errorf("DeleteApp(dev) = %v, want it to proceed under the environment's allow", err)
	}
	// staging inherits the default: still denied, and --confirm does not help.
	err := e.DeleteApp(ctx, "web", "staging", true)
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("DeleteApp(staging) = %v, want a GuardrailError", err)
	}
	if g.Code != cp.GuardrailAppDelete || g.NeedsConfirmation {
		t.Errorf("staging delete guardrail = %+v, want a plain deny on app.delete", g)
	}
	if !strings.Contains(g.Message, "guard set --env staging app.delete") {
		t.Errorf("refusal %q should name the environment it was refused in", g.Message)
	}
}
