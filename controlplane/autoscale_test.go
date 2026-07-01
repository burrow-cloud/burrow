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

// TestAutoscaleCreatesAndUpdates covers the happy path: an autoscale applies an HPA with the
// requested band and CPU target, and a second call updates it in place.
func TestAutoscaleCreatesAndUpdates(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	res, err := e.Autoscale(ctx, "web", "", cp.AutoscaleSpec{MinReplicas: 2, MaxReplicas: 8, CPUPercent: 90}, false)
	if err != nil {
		t.Fatalf("Autoscale: %v", err)
	}
	if res.MinReplicas != 2 || res.MaxReplicas != 8 || res.CPUPercent != 90 {
		t.Errorf("result = %+v, want min 2 max 8 cpu 90", res)
	}
	if !res.MetricsAvailable || res.Warning != "" {
		t.Errorf("metrics present by default: got available=%v warning=%q", res.MetricsAvailable, res.Warning)
	}
	spec, ok := k.Autoscaler("web")
	if !ok {
		t.Fatalf("no HPA applied for web")
	}
	if spec.MinReplicas != 2 || spec.MaxReplicas != 8 || spec.CPUPercent != 90 || spec.MemoryPercent != 0 {
		t.Errorf("applied spec = %+v, want min 2 max 8 cpu 90 mem 0", spec)
	}

	// Update path: a second call replaces the shape, including a memory target.
	if _, err := e.Autoscale(ctx, "web", "", cp.AutoscaleSpec{MinReplicas: 3, MaxReplicas: 12, CPUPercent: 70, MemoryPercent: 60}, false); err != nil {
		t.Fatalf("Autoscale update: %v", err)
	}
	spec, _ = k.Autoscaler("web")
	if spec.MinReplicas != 3 || spec.MaxReplicas != 12 || spec.CPUPercent != 70 || spec.MemoryPercent != 60 {
		t.Errorf("updated spec = %+v, want min 3 max 12 cpu 70 mem 60", spec)
	}
}

// TestAutoscaleValidation rejects malformed specs before any cluster call.
func TestAutoscaleValidation(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newEngine(t, permissive())

	cases := []struct {
		name string
		spec cp.AutoscaleSpec
	}{
		{"min below one", cp.AutoscaleSpec{MinReplicas: 0, MaxReplicas: 5, CPUPercent: 80}},
		{"max below min", cp.AutoscaleSpec{MinReplicas: 5, MaxReplicas: 3, CPUPercent: 80}},
		{"cpu zero", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 0}},
		{"cpu over 100", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 150}},
		{"memory over 100", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80, MemoryPercent: 120}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.Autoscale(ctx, "web", "", tc.spec, false); !errors.Is(err, cp.ErrInvalid) {
				t.Errorf("Autoscale(%+v) = %v, want ErrInvalid", tc.spec, err)
			}
		})
	}
}

// TestAutoscaleMaxBoundedByCeiling proves the autoscaler's max is bounded by the same replica
// ceiling a manual scale is: a max above the ceiling is denied via app.replica_ceiling even though
// the autoscale guardrail itself allows the operation.
func TestAutoscaleMaxBoundedByCeiling(t *testing.T) {
	ctx := context.Background()
	// Ceiling of 10; autoscale allowed by default.
	e, k, _, _, _ := newEngine(t, cp.DefaultPolicy())

	_, err := e.Autoscale(ctx, "web", "", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 99, CPUPercent: 80}, false)
	mustGuardrail(t, err, cp.GuardrailReplicaCeiling)
	if _, ok := k.Autoscaler("web"); ok {
		t.Errorf("HPA should not be applied when the max is denied")
	}

	// A max at the ceiling is allowed.
	if _, err := e.Autoscale(ctx, "web", "", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 50, CPUPercent: 80}, false); err != nil {
		t.Fatalf("Autoscale at ceiling: %v", err)
	}
}

// TestAutoscaleGuardrail covers the autoscale guardrail itself: it is allow by default, and a
// per-environment deny blocks the operation in that environment.
func TestAutoscaleGuardrail(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newEngine(t, cp.DefaultPolicy())

	// Default is allow: an autoscale within the ceiling proceeds.
	if _, err := e.Autoscale(ctx, "web", "", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}, false); err != nil {
		t.Fatalf("default-allow autoscale: %v", err)
	}

	// A per-environment deny blocks the operation in that environment only.
	if _, err := e.AddEnvironment(ctx, "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if err := e.SetGuardrail(ctx, "prod", cp.GuardrailAutoscale, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(prod, app.autoscale): %v", err)
	}
	_, err := e.Autoscale(ctx, "web", "prod", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}, false)
	mustGuardrail(t, err, cp.GuardrailAutoscale)
	// The default environment is unaffected.
	if _, err := e.Autoscale(ctx, "web", "", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}, false); err != nil {
		t.Fatalf("default env still allowed: %v", err)
	}
}

// TestDisableAutoscale removes the HPA and is idempotent.
func TestDisableAutoscale(t *testing.T) {
	ctx := context.Background()
	e, k, _, _, _ := newEngine(t, permissive())

	if _, err := e.Autoscale(ctx, "web", "", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}, false); err != nil {
		t.Fatalf("Autoscale: %v", err)
	}
	if _, ok := k.Autoscaler("web"); !ok {
		t.Fatalf("HPA should exist before disable")
	}
	if err := e.DisableAutoscale(ctx, "web", "", false); err != nil {
		t.Fatalf("DisableAutoscale: %v", err)
	}
	if _, ok := k.Autoscaler("web"); ok {
		t.Errorf("HPA should be gone after disable")
	}
	// Idempotent: disabling an app with no HPA succeeds.
	if err := e.DisableAutoscale(ctx, "web", "", false); err != nil {
		t.Errorf("DisableAutoscale (idempotent) = %v, want nil", err)
	}
}

// TestAutoscaleMetricsAbsentWarns proves the HPA is applied even when metrics-server is absent, and
// the result carries the plain warning that it will not scale until it is installed.
func TestAutoscaleMetricsAbsentWarns(t *testing.T) {
	ctx := context.Background()
	e, k, _, _, _ := newEngine(t, permissive())
	k.SetMetricsAvailable(false)

	res, err := e.Autoscale(ctx, "web", "", cp.AutoscaleSpec{MinReplicas: 1, MaxReplicas: 5, CPUPercent: 80}, false)
	if err != nil {
		t.Fatalf("Autoscale: %v", err)
	}
	if res.MetricsAvailable {
		t.Errorf("metrics should be reported absent")
	}
	if !strings.Contains(res.Warning, "metrics-server") {
		t.Errorf("warning = %q, want it to mention metrics-server", res.Warning)
	}
	if strings.Contains(res.Warning, "—") {
		t.Errorf("warning must not contain an em-dash: %q", res.Warning)
	}
	// The HPA is applied regardless — its creation does not need metrics-server.
	if _, ok := k.Autoscaler("web"); !ok {
		t.Errorf("HPA should be applied even without metrics-server")
	}
}
