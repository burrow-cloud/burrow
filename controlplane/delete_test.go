// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestDeleteApp tears down the workload, routing, and release history of an existing app and
// succeeds with confirm, leaving the app unknown to both the cluster and the deploy record.
func TestDeleteApp(t *testing.T) {
	e, k, r, d, _ := newEngine(t, permissive())
	ctx := context.Background()

	r.Add("img:1", "sha256:deadbeef")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := k.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("Expose: %v", err)
	}

	if err := e.DeleteApp(ctx, "web", true); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	if _, err := k.WorkloadStatus(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("workload after delete: err = %v, want ErrNotFound", err)
	}
	if err := k.Unexpose(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("routing after delete: err = %v, want ErrNotFound", err)
	}
	if rels, err := d.Releases(ctx, "web"); err != nil || len(rels) != 0 {
		t.Errorf("releases after delete = %v (err %v), want empty", rels, err)
	}
}

// TestDeleteAppWorkloadOnly deletes an app that has a workload but was never exposed and has no
// recorded releases — the already-absent routing is tolerated, not an error.
func TestDeleteAppWorkloadOnly(t *testing.T) {
	e, k, _, _, _ := newEngine(t, permissive())
	ctx := context.Background()

	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	if err := e.DeleteApp(ctx, "web", true); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if _, err := k.WorkloadStatus(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("workload after delete: err = %v, want ErrNotFound", err)
	}
}

// TestDeleteAppUnknown reports ErrNotFound when the app has neither releases nor a live workload.
func TestDeleteAppUnknown(t *testing.T) {
	e, _, _, _, _ := newEngine(t, permissive())
	if err := e.DeleteApp(context.Background(), "web", true); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("DeleteApp unknown err = %v, want ErrNotFound", err)
	}
}

// TestDeleteAppGuardrailHolds confirms the app_delete guardrail holds the delete for
// confirmation when not confirmed, and proceeds once confirmed.
func TestDeleteAppGuardrailHolds(t *testing.T) {
	policy := cp.DefaultPolicy().With(cp.GuardrailAppDelete, cp.DispositionConfirm)
	e, k, _, _, _ := newEngine(t, policy)
	ctx := context.Background()

	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	err := e.DeleteApp(ctx, "web", false)
	mustGuardrail(t, err, cp.GuardrailAppDelete)
	g, _ := cp.AsGuardrail(err)
	if !g.NeedsConfirmation {
		t.Errorf("NeedsConfirmation = false, want true")
	}
	// The workload survives a held delete.
	if _, err := k.WorkloadStatus(ctx, "web"); err != nil {
		t.Errorf("workload should survive a held delete: %v", err)
	}

	if err := e.DeleteApp(ctx, "web", true); err != nil {
		t.Fatalf("DeleteApp confirmed: %v", err)
	}
}
