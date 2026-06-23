// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
)

// Engine is the control plane's deploy orchestrator: the product. It turns an agent's
// deploy / status / logs / rollback / scale requests into guarded operations against
// the cluster, records every deploy, and returns structured results
// (ADR-0002, ADR-0006). It owns no global state and reads no ambient time or
// randomness — every external dependency is an injected seam (ADR-0010), so the engine
// is deterministic and unit-testable against fakes.
type Engine struct {
	k8s      Kubernetes
	registry Registry
	db       Database
	clock    Clock
	ids      IDSource
	policy   Policy
}

// Deps are the dependencies an Engine needs. All seams are required; Policy defaults to
// a conservative guardrail policy via DefaultPolicy if left zero is *not* done here —
// callers pass an explicit, validated Policy.
type Deps struct {
	Kubernetes Kubernetes
	Registry   Registry
	Database   Database
	Clock      Clock
	IDs        IDSource
	Policy     Policy
}

// New constructs an Engine, validating that every seam is supplied and the policy is
// coherent. It returns an error rather than panicking so wiring mistakes surface at
// startup.
func New(d Deps) (*Engine, error) {
	switch {
	case d.Kubernetes == nil:
		return nil, fmt.Errorf("controlplane: New: Kubernetes seam is required")
	case d.Registry == nil:
		return nil, fmt.Errorf("controlplane: New: Registry seam is required")
	case d.Database == nil:
		return nil, fmt.Errorf("controlplane: New: Database seam is required")
	case d.Clock == nil:
		return nil, fmt.Errorf("controlplane: New: Clock seam is required")
	case d.IDs == nil:
		return nil, fmt.Errorf("controlplane: New: IDs seam is required")
	}
	if err := d.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("controlplane: New: policy: %w", err)
	}
	return &Engine{
		k8s:      d.Kubernetes,
		registry: d.Registry,
		db:       d.Database,
		clock:    d.Clock,
		ids:      d.IDs,
		policy:   d.Policy,
	}, nil
}

// Deploy rolls out an image by reference (ADR-0007). It validates the request, applies
// the guardrails, resolves the image in the registry, records a new release, applies it
// to the cluster, and records the outcome — superseding the previously running release
// on success. The image bytes never pass through here; only the reference does
// (ADR-0004).
func (e *Engine) Deploy(ctx context.Context, req DeployRequest) (DeployResult, error) {
	if err := (App{Name: req.App}).Validate(); err != nil {
		return DeployResult{}, fmt.Errorf("deploy: %w", err)
	}
	if req.Image == "" {
		return DeployResult{}, fmt.Errorf("deploy %s: image reference is empty", req.App)
	}
	if req.Replicas < 0 {
		return DeployResult{}, fmt.Errorf("deploy %s: replicas %d is negative", req.App, req.Replicas)
	}
	if err := e.policy.checkReplicas("deploy", req.Replicas); err != nil {
		return DeployResult{}, err
	}

	info, err := e.registry.Resolve(ctx, req.Image)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return DeployResult{}, fmt.Errorf("deploy %s: image %q is not present in the registry: %w", req.App, req.Image, err)
		}
		return DeployResult{}, fmt.Errorf("deploy %s: resolving image %q: %w", req.App, req.Image, err)
	}

	releases, err := e.db.Releases(ctx, req.App)
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: reading release history: %w", req.App, err)
	}
	prev, hasPrev := lastDeployed(releases)

	rel := Release{
		ID:        e.ids.NewID(),
		App:       req.App,
		Image:     req.Image,
		Digest:    info.Digest,
		Env:       req.Env,
		Command:   req.Command,
		Replicas:  req.Replicas,
		Status:    ReleasePending,
		CreatedAt: e.clock.Now(),
	}
	if hasPrev {
		rel.Supersedes = prev.ID
	}
	if err := rel.Validate(); err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: %w", req.App, err)
	}
	if err := e.db.SaveRelease(ctx, rel); err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: recording release: %w", req.App, err)
	}

	spec := DeploymentSpec{App: req.App, Image: req.Image, Env: req.Env, Command: req.Command, Replicas: req.Replicas}
	if err := e.k8s.ApplyDeployment(ctx, spec); err != nil {
		rel.Status = ReleaseFailed
		_ = e.db.SaveRelease(ctx, rel) // best effort: record the failure
		return DeployResult{}, fmt.Errorf("deploy %s: applying to cluster: %w", req.App, err)
	}

	// The cluster is updated. From here a SaveRelease failure leaves the record behind
	// the cluster (the release stays Pending though the new image is live) — a drift
	// the reconcile loop closes in a later phase. v0.1 surfaces the error honestly.
	rel.Status = ReleaseDeployed
	if err := e.db.SaveRelease(ctx, rel); err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: recording successful release: %w", req.App, err)
	}

	superseded := ""
	if hasPrev {
		prev.Status = ReleaseSuperseded
		if err := e.db.SaveRelease(ctx, prev); err != nil {
			return DeployResult{}, fmt.Errorf("deploy %s: superseding prior release %s: %w", req.App, prev.ID, err)
		}
		superseded = prev.ID
	}
	return DeployResult{Release: rel, SupersededReleaseID: superseded}, nil
}

// Status returns the combined control-plane and cluster view of an app: the most recent
// recorded release and the live workload state. It returns ErrNotFound only when the
// app is unknown to both.
func (e *Engine) Status(ctx context.Context, app string) (StatusResult, error) {
	res := StatusResult{App: app}

	latest, errL := e.db.LatestRelease(ctx, app)
	if errL != nil && !errors.Is(errL, ErrNotFound) {
		return StatusResult{}, fmt.Errorf("status %s: reading release: %w", app, errL)
	}
	if errL == nil {
		res.HasRelease = true
		res.Release = latest
	}

	st, errK := e.k8s.DeploymentStatus(ctx, app)
	if errK != nil && !errors.Is(errK, ErrNotFound) {
		return StatusResult{}, fmt.Errorf("status %s: reading cluster: %w", app, errK)
	}
	if errK == nil {
		res.Running = true
		res.Deployment = st
	}

	if !res.HasRelease && !res.Running {
		return StatusResult{}, fmt.Errorf("status %s: unknown app: %w", app, ErrNotFound)
	}
	return res, nil
}

// Logs returns recent log lines for an app's workload.
func (e *Engine) Logs(ctx context.Context, app string, opts LogOptions) ([]LogLine, error) {
	lines, err := e.k8s.Logs(ctx, app, opts)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("logs %s: no running deployment: %w", app, err)
		}
		return nil, fmt.Errorf("logs %s: %w", app, err)
	}
	return lines, nil
}

// Scale changes an app's replica count, guarded against scale-to-zero and the policy
// ceiling (ADR-0006). It does not create a new release: scaling adjusts the running
// workload, while a release records a deploy.
func (e *Engine) Scale(ctx context.Context, app string, replicas int32) (ScaleResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return ScaleResult{}, fmt.Errorf("scale: %w", err)
	}
	if replicas < 0 {
		return ScaleResult{}, fmt.Errorf("scale %s: replicas %d is negative", app, replicas)
	}
	if err := e.policy.checkReplicas("scale", replicas); err != nil {
		return ScaleResult{}, err
	}

	st, err := e.k8s.DeploymentStatus(ctx, app)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ScaleResult{}, fmt.Errorf("scale %s: no running deployment: %w", app, err)
		}
		return ScaleResult{}, fmt.Errorf("scale %s: reading current state: %w", app, err)
	}
	prev := st.DesiredReplicas

	if err := e.k8s.ScaleDeployment(ctx, app, replicas); err != nil {
		return ScaleResult{}, fmt.Errorf("scale %s: %w", app, err)
	}
	return ScaleResult{App: app, PreviousReplicas: prev, Replicas: replicas}, nil
}

// Rollback restores the app's previously running release by redeploying its reference
// (ADR-0007). It finds the current running release, re-applies the release that one
// superseded, and records the rollback as a new release. It returns ErrNotFound when
// there is nothing to roll back from or to.
func (e *Engine) Rollback(ctx context.Context, app string) (RollbackResult, error) {
	releases, err := e.db.Releases(ctx, app)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: reading release history: %w", app, err)
	}
	cur, ok := lastDeployed(releases)
	if !ok {
		return RollbackResult{}, fmt.Errorf("rollback %s: no deployed release to roll back from: %w", app, ErrNotFound)
	}
	if cur.Supersedes == "" {
		return RollbackResult{}, fmt.Errorf("rollback %s: release %s has no prior release to roll back to: %w", app, cur.ID, ErrNotFound)
	}

	target, err := e.db.Release(ctx, cur.Supersedes)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: reading prior release %s: %w", app, cur.Supersedes, err)
	}

	rel := Release{
		ID:         e.ids.NewID(),
		App:        app,
		Image:      target.Image,
		Digest:     target.Digest,
		Env:        target.Env,
		Command:    target.Command,
		Replicas:   target.Replicas,
		Status:     ReleasePending,
		Supersedes: cur.ID,
		CreatedAt:  e.clock.Now(),
	}
	if err := e.db.SaveRelease(ctx, rel); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: recording release: %w", app, err)
	}

	spec := DeploymentSpec{App: app, Image: target.Image, Env: target.Env, Command: target.Command, Replicas: target.Replicas}
	if err := e.k8s.ApplyDeployment(ctx, spec); err != nil {
		rel.Status = ReleaseFailed
		_ = e.db.SaveRelease(ctx, rel)
		return RollbackResult{}, fmt.Errorf("rollback %s: applying to cluster: %w", app, err)
	}

	rel.Status = ReleaseDeployed
	if err := e.db.SaveRelease(ctx, rel); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: recording successful release: %w", app, err)
	}
	cur.Status = ReleaseSuperseded
	if err := e.db.SaveRelease(ctx, cur); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: superseding release %s: %w", app, cur.ID, err)
	}
	return RollbackResult{Release: rel, RolledBackToReleaseID: target.ID, SupersededReleaseID: cur.ID}, nil
}

// lastDeployed returns the most recent release in deployed state — the one currently
// running — given releases in oldest-first order.
func lastDeployed(releases []Release) (Release, bool) {
	for i := len(releases) - 1; i >= 0; i-- {
		if releases[i].Status == ReleaseDeployed {
			return releases[i], true
		}
	}
	return Release{}, false
}
