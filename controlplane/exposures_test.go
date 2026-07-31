// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestExposeRecordsAndForgetsIntent: an expose records what was asked for in the registry, and an
// unexpose or an app deletion removes it. Without the row, an Ingress that later disappears looks
// exactly like an app that was never exposed (ADR-0074 §6); with a STALE row, the observer would
// report a missing Ingress for routing someone removed on purpose, which is the false positive most
// likely to teach a reader to ignore the surface.
func TestExposeRecordsAndForgetsIntent(t *testing.T) {
	ctx := context.Background()
	policy := cp.DefaultPolicy().
		With(cp.GuardrailExposePublic, cp.DispositionAllow).
		With(cp.GuardrailAppDelete, cp.DispositionAllow)
	e, _, d, _ := newEngine(t, policy)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := e.Expose(ctx, cp.ExposeRequest{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("expose: %v", err)
	}

	recorded, err := d.Exposures(ctx)
	if err != nil {
		t.Fatalf("Exposures: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("got %d recorded exposures, want 1: %+v", len(recorded), recorded)
	}
	ex := recorded[0]
	if ex.App != "web" || ex.Host != "web.example.com" || ex.Port != 8080 || ex.Environment != cp.DefaultEnvironment {
		t.Errorf("recorded exposure = %+v, want web at web.example.com:8080 in prod", ex)
	}
	if ex.CreatedAt.IsZero() {
		t.Errorf("recorded exposure has no timestamp; it comes from the injected clock, not ambient time")
	}

	if err := e.Unexpose(ctx, "web", ""); err != nil {
		t.Fatalf("unexpose: %v", err)
	}
	if recorded, err = d.Exposures(ctx); err != nil || len(recorded) != 0 {
		t.Fatalf("after unexpose: %d recorded exposures (err %v), want none", len(recorded), err)
	}

	// Deleting the app drops the intent too — the app is gone, so the exposure is intent about
	// nothing.
	if _, err := e.Expose(ctx, cp.ExposeRequest{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("re-expose: %v", err)
	}
	if err := e.DeleteApp(ctx, "web", "", true); err != nil {
		t.Fatalf("delete app: %v", err)
	}
	if recorded, err = d.Exposures(ctx); err != nil || len(recorded) != 0 {
		t.Errorf("after deleting the app: %d recorded exposures (err %v), want none", len(recorded), err)
	}
}
