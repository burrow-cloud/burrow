// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// TestDeployApplyFailureLeavesPriorRunning verifies that when the cluster rejects a new
// deploy, the previously running release is untouched: the cluster keeps the old image
// and the failed attempt is recorded as Failed, not Deployed.
func TestDeployApplyFailureLeavesPriorRunning(t *testing.T) {
	ctx := context.Background()
	e, k, r, d, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	r.Add("img:2", "sha256:2")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}

	k.SetError(fake.OpApply, errors.New("apiserver unavailable"))
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err == nil {
		t.Fatalf("deploy v2 should fail when apply errors")
	}

	// Cluster still on img:1.
	if spec, _ := k.Spec("web"); spec.Image != "img:1" {
		t.Errorf("cluster image = %q, want img:1 (prior release untouched)", spec.Image)
	}
	// History: v1 deployed, v2 failed.
	all, _ := d.Releases(ctx, "web")
	if len(all) != 2 {
		t.Fatalf("releases = %d, want 2", len(all))
	}
	if all[0].Status != cp.ReleaseDeployed {
		t.Errorf("v1 status = %q, want deployed", all[0].Status)
	}
	if all[1].Status != cp.ReleaseFailed || all[1].Image != "img:2" {
		t.Errorf("v2 = %+v, want failed img:2", all[1])
	}

	// Recovery: clearing the fault lets a redeploy succeed.
	k.SetError(fake.OpApply, nil)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("redeploy after recovery: %v", err)
	}
	if spec, _ := k.Spec("web"); spec.Image != "img:2" {
		t.Errorf("cluster image = %q, want img:2 after recovery", spec.Image)
	}
}

func TestDeployRegistryError(t *testing.T) {
	ctx := context.Background()
	e, k, r, d, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	r.SetError(fake.OpResolve, errors.New("registry timeout"))

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err == nil {
		t.Fatalf("deploy should fail when the registry errors")
	}
	if errors.Is(err, cp.ErrNotFound) {
		t.Errorf("a registry timeout is not a not-found; err = %v", err)
	}
	if _, ok := k.Spec("web"); ok {
		t.Errorf("nothing should be applied when resolve fails")
	}
	if all, _ := d.Releases(ctx, "web"); len(all) != 0 {
		t.Errorf("nothing should be recorded when resolve fails, got %d", len(all))
	}
}

func TestDeploySaveErrorBeforeApply(t *testing.T) {
	ctx := context.Background()
	e, k, r, d, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	d.SetError(fake.OpSaveRelease, errors.New("db unavailable"))

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err == nil {
		t.Fatalf("deploy should fail when the pending-release save fails")
	}
	// The pending save fails before apply, so the cluster is never touched.
	if _, ok := k.Spec("web"); ok {
		t.Errorf("nothing should be applied when the pending save fails")
	}
}

// TestSeededSchedule drives a deterministic, seeded sequence of operations with random
// injected seam failures and checks one invariant after every step: the image running
// in the cluster always equals the last image a deploy or rollback successfully
// applied. Every failure mode the engine can hit aborts before mutating the cluster, so
// a failed operation must never change what is running.
func TestSeededSchedule(t *testing.T) {
	ctx := context.Background()
	e, k, r, d, _ := newEngine(t, cp.Policy{MaxReplicas: 1000}.
		With(cp.GuardrailScaleToZero, cp.DispositionAllow).
		With(cp.GuardrailAppDeploy, cp.DispositionAllow))

	images := []string{"img:a", "img:b", "img:c"}
	for _, im := range images {
		r.Add(im, "sha256:"+im)
	}
	const app = "web"

	// setErr routes an injected error to the fake that owns the operation.
	setErr := func(op fake.Op, err error) {
		switch op {
		case fake.OpResolve:
			r.SetError(op, err)
		case fake.OpApply, fake.OpStatus, fake.OpScale, fake.OpLogs, fake.OpDelete:
			k.SetError(op, err)
		default: // database ops: OpReleases, OpRelease, OpSaveRelease, OpLatestRelease
			d.SetError(op, err)
		}
	}
	injectable := []fake.Op{fake.OpResolve, fake.OpReleases, fake.OpRelease, fake.OpSaveRelease, fake.OpApply, fake.OpStatus, fake.OpScale}

	rng := rand.New(rand.NewSource(1))
	expectedImage := "" // last image a deploy/rollback successfully applied

	for i := 0; i < 600; i++ {
		// Optionally inject one fault for this step.
		var injectedOp fake.Op
		injected := false
		if rng.Intn(100) < 35 {
			injectedOp = injectable[rng.Intn(len(injectable))]
			setErr(injectedOp, fmt.Errorf("injected fault step %d", i))
			injected = true
		}

		// Pick and run an operation (deploy weighted higher to build history).
		switch rng.Intn(4) {
		case 0, 1:
			img := images[rng.Intn(len(images))]
			res, err := e.Deploy(ctx, cp.DeployRequest{App: app, Image: img, Replicas: int32(1 + rng.Intn(4))})
			if err == nil {
				expectedImage = res.Release.Image
			}
		case 2:
			_, _ = e.Scale(ctx, app, "", int32(1+rng.Intn(5)), false)
		case 3:
			res, err := e.Rollback(ctx, app, "", false)
			if err == nil {
				expectedImage = res.Release.Image
			}
		}

		// Clear the injected fault before asserting the invariant.
		if injected {
			setErr(injectedOp, nil)
		}

		spec, ok := k.Spec(app)
		if expectedImage == "" {
			if ok {
				t.Fatalf("step %d: a workload exists but no deploy has succeeded", i)
			}
			continue
		}
		if !ok {
			t.Fatalf("step %d: expected workload running %q, but none exists", i, expectedImage)
		}
		if spec.Image != expectedImage {
			t.Fatalf("step %d: cluster image = %q, want %q (last successfully applied)", i, spec.Image, expectedImage)
		}
	}
}
