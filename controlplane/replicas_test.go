// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// deployReplicas deploys web at the given requested count and returns the replica count the engine
// resolved and applied to the cluster.
func deployReplicas(t *testing.T, e *cp.Engine, k interface {
	Spec(string) (cp.WorkloadSpec, bool)
}, requested int32) int32 {
	t.Helper()
	if _, err := e.Deploy(context.Background(), cp.DeployRequest{App: "web", Image: "img:1", Replicas: requested}); err != nil {
		t.Fatalf("Deploy(replicas=%d): %v", requested, err)
	}
	spec, ok := k.Spec("web")
	if !ok {
		t.Fatalf("no cluster spec after deploy")
	}
	return spec.Replicas
}

// TestDeployResolvesReplicasNewApp: a brand-new app resolves an unspecified count to 1 and an
// explicit count verbatim (no HPA, nothing running).
func TestDeployResolvesReplicasNewApp(t *testing.T) {
	t.Run("unspecified defaults to 1", func(t *testing.T) {
		e, k, _, _ := newEngine(t, permissive())
		if got := deployReplicas(t, e, k, 0); got != 1 {
			t.Errorf("new app, unspecified replicas resolved to %d, want 1", got)
		}
	})
	t.Run("explicit honored", func(t *testing.T) {
		e, k, _, _ := newEngine(t, permissive())
		if got := deployReplicas(t, e, k, 3); got != 3 {
			t.Errorf("new app, explicit 3 resolved to %d, want 3", got)
		}
	})
}

// TestDeployPreservesCurrentReplicas: an existing app redeployed with an unspecified count keeps its
// running replica count rather than being reset (or scaled to zero).
func TestDeployPreservesCurrentReplicas(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if got := deployReplicas(t, e, k, 4); got != 4 {
		t.Fatalf("initial deploy resolved to %d, want 4", got)
	}
	// A later scale takes it to 6; a redeploy that omits replicas must not undo it.
	if _, err := e.Scale(ctx, "web", "", 6, false); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if got := deployReplicas(t, e, k, 0); got != 6 {
		t.Errorf("redeploy with unspecified replicas resolved to %d, want 6 (current preserved)", got)
	}
}

// TestDeployExplicitReplicasWithoutAutoscaler: an existing app redeployed with an explicit count and
// no HPA takes the explicit count — deploy-time scaling stays possible without an autoscaler.
func TestDeployExplicitReplicasWithoutAutoscaler(t *testing.T) {
	e, k, _, _ := newEngine(t, permissive())
	if got := deployReplicas(t, e, k, 4); got != 4 {
		t.Fatalf("initial deploy resolved to %d, want 4", got)
	}
	if got := deployReplicas(t, e, k, 2); got != 2 {
		t.Errorf("redeploy explicit 2 (no HPA) resolved to %d, want 2", got)
	}
}

// TestDeployRespectsActiveAutoscaler: an active HPA owns the replica count, so a redeploy leaves the
// live count untouched whether the request is explicit or omitted — a deploy must not reset the
// HPA's scaling.
func TestDeployRespectsActiveAutoscaler(t *testing.T) {
	ctx := context.Background()

	t.Run("explicit count ignored", func(t *testing.T) {
		e, k, _, _ := newEngine(t, permissive())
		if got := deployReplicas(t, e, k, 4); got != 4 {
			t.Fatalf("initial deploy resolved to %d, want 4", got)
		}
		// Model the HPA having scaled the workload to 7, then mark it active.
		if _, err := e.Scale(ctx, "web", "", 7, false); err != nil {
			t.Fatalf("Scale: %v", err)
		}
		k.SetAutoscalerActive("web", true)
		if got := deployReplicas(t, e, k, 2); got != 7 {
			t.Errorf("redeploy explicit 2 under an active HPA resolved to %d, want 7 (HPA respected, explicit ignored)", got)
		}
	})

	t.Run("unspecified count preserves live count", func(t *testing.T) {
		e, k, _, _ := newEngine(t, permissive())
		if got := deployReplicas(t, e, k, 4); got != 4 {
			t.Fatalf("initial deploy resolved to %d, want 4", got)
		}
		if _, err := e.Scale(ctx, "web", "", 7, false); err != nil {
			t.Fatalf("Scale: %v", err)
		}
		k.SetAutoscalerActive("web", true)
		if got := deployReplicas(t, e, k, 0); got != 7 {
			t.Errorf("redeploy unspecified under an active HPA resolved to %d, want 7 (HPA respected)", got)
		}
	})
}

// TestRollbackResolvesReplicas: a rollback restores the prior image but resolves replicas the same
// way a deploy does — it preserves the running count (or defers to an active HPA) rather than
// resetting to the target release's count.
func TestRollbackResolvesReplicas(t *testing.T) {
	ctx := context.Background()

	t.Run("preserves current, not the target release's count", func(t *testing.T) {
		e, k, _, _ := newEngine(t, permissive())
		// v1 at 2 replicas, v2 at 5. A scale then takes the app to 8.
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
			t.Fatalf("deploy v1: %v", err)
		}
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 5}); err != nil {
			t.Fatalf("deploy v2: %v", err)
		}
		if _, err := e.Scale(ctx, "web", "", 8, false); err != nil {
			t.Fatalf("Scale: %v", err)
		}
		if _, err := e.Rollback(ctx, "web", "", false); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		spec, _ := k.Spec("web")
		if spec.Image != "img:1" {
			t.Errorf("rollback image = %q, want img:1", spec.Image)
		}
		if spec.Replicas != 8 {
			t.Errorf("rollback replicas = %d, want 8 (current preserved, not v1's 2)", spec.Replicas)
		}
	})

	t.Run("defers to an active autoscaler", func(t *testing.T) {
		e, k, _, _ := newEngine(t, permissive())
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
			t.Fatalf("deploy v1: %v", err)
		}
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 5}); err != nil {
			t.Fatalf("deploy v2: %v", err)
		}
		if _, err := e.Scale(ctx, "web", "", 9, false); err != nil {
			t.Fatalf("Scale: %v", err)
		}
		k.SetAutoscalerActive("web", true)
		if _, err := e.Rollback(ctx, "web", "", false); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		spec, _ := k.Spec("web")
		if spec.Replicas != 9 {
			t.Errorf("rollback under an active HPA resolved to %d, want 9 (HPA respected)", spec.Replicas)
		}
	})
}

// TestReapplyEnvPreservesReplicas: a config change re-renders the running workload without rescaling
// it — the current replica count is preserved (an active HPA is left to own it).
func TestReapplyEnvPreservesReplicas(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 3}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if _, err := e.Scale(ctx, "web", "", 6, false); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if err := e.SetConfig(ctx, "web", "", "K", "V", false); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	spec, _ := k.Spec("web")
	if spec.Replicas != 6 {
		t.Errorf("config reapply resolved replicas to %d, want 6 (current preserved)", spec.Replicas)
	}
}

// TestDeployCeilingUsesResolvedReplicas: the replica-ceiling guardrail is evaluated against the
// RESOLVED count. An active HPA that has scaled the app above the ceiling still trips the ceiling on
// a redeploy, and scale-to-zero never trips on a deploy because the resolved count is always >= 1.
func TestDeployCeilingUsesResolvedReplicas(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, cp.Policy{MaxReplicas: 5}.
		With(cp.GuardrailAppDeploy, cp.DispositionAllow).
		With(cp.GuardrailReplicaCeiling, cp.DispositionDeny).
		With(cp.GuardrailScaleToZero, cp.DispositionDeny))

	// Seed a running app within the ceiling, then model the HPA scaling it above the ceiling
	// directly in the cluster (the HPA changes the live count without going through burrow's scale).
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 5}); err != nil {
		t.Fatalf("initial deploy: %v", err)
	}
	if err := k.ScaleWorkload(ctx, "web", 8); err != nil {
		t.Fatalf("ScaleWorkload: %v", err)
	}
	k.SetAutoscalerActive("web", true)
	// The resolved count (8, preserved from the HPA) exceeds the ceiling of 5, so the redeploy trips
	// the replica-ceiling guardrail rather than silently applying an over-ceiling count.
	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 0})
	mustGuardrail(t, err, cp.GuardrailReplicaCeiling)
}
