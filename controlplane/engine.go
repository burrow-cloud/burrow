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
	resolver Resolver
}

// Deps are the dependencies an Engine needs. All seams are required. The guardrail policy
// is not a dependency here: the engine reads the live policy from the Database seam on each
// guarded operation (ADR-0020), so a `guard set` takes effect without restarting.
type Deps struct {
	Kubernetes Kubernetes
	Registry   Registry
	Database   Database
	Clock      Clock
	IDs        IDSource
	Resolver   Resolver
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
	case d.Resolver == nil:
		return nil, fmt.Errorf("controlplane: New: Resolver seam is required")
	}
	return &Engine{
		k8s:      d.Kubernetes,
		registry: d.Registry,
		db:       d.Database,
		clock:    d.Clock,
		ids:      d.IDs,
		resolver: d.Resolver,
	}, nil
}

// Deploy rolls out an image by reference (ADR-0007). It validates the request, applies
// the guardrails, resolves the image in the registry, records a new release, applies it
// to the cluster, and records the outcome — superseding the previously running release
// on success. The image bytes never pass through here; only the reference does
// (ADR-0004).
func (e *Engine) Deploy(ctx context.Context, req DeployRequest) (DeployResult, error) {
	if err := (App{Name: req.App}).Validate(); err != nil {
		return DeployResult{}, fmt.Errorf("deploy: %w: %w", ErrInvalid, err)
	}
	if req.Image == "" {
		return DeployResult{}, fmt.Errorf("deploy %s: image reference is empty: %w", req.App, ErrInvalid)
	}
	if req.Replicas < 0 {
		return DeployResult{}, fmt.Errorf("deploy %s: replicas %d is negative: %w", req.App, req.Replicas, ErrInvalid)
	}
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: loading guardrail policy: %w", req.App, err)
	}
	if err := pol.evaluateReplicas("deploy", req.Replicas, req.Confirm); err != nil {
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

	spec := WorkloadSpec{App: req.App, Kind: WorkloadDeployment, Image: req.Image, Env: req.Env, Command: req.Command, Replicas: req.Replicas}
	if err := e.k8s.ApplyWorkload(ctx, spec); err != nil {
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

	st, errK := e.k8s.WorkloadStatus(ctx, app)
	if errK != nil && !errors.Is(errK, ErrNotFound) {
		return StatusResult{}, fmt.Errorf("status %s: reading cluster: %w", app, errK)
	}
	if errK == nil {
		res.Running = true
		res.Workload = st
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
			return nil, fmt.Errorf("logs %s: no running workload: %w", app, err)
		}
		return nil, fmt.Errorf("logs %s: %w", app, err)
	}
	return lines, nil
}

// Scale changes an app's replica count, guarded against scale-to-zero and the policy
// ceiling (ADR-0006). It does not create a new release: scaling adjusts the running
// workload, while a release records a deploy.
func (e *Engine) Scale(ctx context.Context, app string, replicas int32, confirm bool) (ScaleResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return ScaleResult{}, fmt.Errorf("scale: %w: %w", ErrInvalid, err)
	}
	if replicas < 0 {
		return ScaleResult{}, fmt.Errorf("scale %s: replicas %d is negative: %w", app, replicas, ErrInvalid)
	}
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return ScaleResult{}, fmt.Errorf("scale %s: loading guardrail policy: %w", app, err)
	}
	if err := pol.evaluateReplicas("scale", replicas, confirm); err != nil {
		return ScaleResult{}, err
	}

	st, err := e.k8s.WorkloadStatus(ctx, app)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ScaleResult{}, fmt.Errorf("scale %s: no running workload: %w", app, err)
		}
		return ScaleResult{}, fmt.Errorf("scale %s: reading current state: %w", app, err)
	}
	prev := st.DesiredReplicas

	if err := e.k8s.ScaleWorkload(ctx, app, replicas); err != nil {
		return ScaleResult{}, fmt.Errorf("scale %s: %w", app, err)
	}
	return ScaleResult{App: app, PreviousReplicas: prev, Replicas: replicas}, nil
}

// Expose makes an app reachable at a hostname through an Ingress (ADR-0018). It is a guarded
// operation: public exposure trips the expose_public guardrail, which holds for confirmation
// by default. The app must already be deployed.
func (e *Engine) Expose(ctx context.Context, req ExposeRequest) (ExposeResult, error) {
	if err := (App{Name: req.App}).Validate(); err != nil {
		return ExposeResult{}, fmt.Errorf("expose: %w: %w", ErrInvalid, err)
	}
	if req.Host == "" {
		return ExposeResult{}, fmt.Errorf("expose %s: host is empty: %w", req.App, ErrInvalid)
	}
	if req.Port <= 0 {
		return ExposeResult{}, fmt.Errorf("expose %s: port %d must be positive: %w", req.App, req.Port, ErrInvalid)
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return ExposeResult{}, fmt.Errorf("expose %s: loading guardrail policy: %w", req.App, err)
	}
	if err := pol.evaluateGuardrail("expose", GuardrailExposePublic, req.Confirm, fmt.Sprintf("exposing %s at %s", req.App, req.Host)); err != nil {
		return ExposeResult{}, err
	}

	// The app must be deployed: exposing a workload that does not exist would create a
	// Service with no backends.
	if _, err := e.k8s.WorkloadStatus(ctx, req.App); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ExposeResult{}, fmt.Errorf("expose %s: no running workload — deploy it first: %w", req.App, err)
		}
		return ExposeResult{}, fmt.Errorf("expose %s: reading workload: %w", req.App, err)
	}

	if err := e.k8s.Expose(ctx, ExposeSpec{App: req.App, Host: req.Host, Port: req.Port}); err != nil {
		return ExposeResult{}, fmt.Errorf("expose %s: %w", req.App, err)
	}
	return ExposeResult{App: req.App, Host: req.Host, Port: req.Port, URL: "http://" + req.Host}, nil
}

// Reachability reports, link by link, whether an app is reachable at its hostname (ADR-0018):
// deployed and ready, exposed, given an external address by an ingress controller, and DNS
// pointing the host at that address. It returns a structured chain plus a one-line plain
// summary for a non-expert; it never errors on a missing link — that is the answer.
func (e *Engine) Reachability(ctx context.Context, app string) (ReachabilityResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return ReachabilityResult{}, fmt.Errorf("reachability: %w: %w", ErrInvalid, err)
	}
	res := ReachabilityResult{App: app}

	ws, err := e.k8s.WorkloadStatus(ctx, app)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			res.Summary = fmt.Sprintf("%s is not deployed yet — deploy it first.", app)
			return res, nil
		}
		return ReachabilityResult{}, fmt.Errorf("reachability %s: reading workload: %w", app, err)
	}
	res.Deployed = true
	res.Ready = ws.Available

	exp, err := e.k8s.ExposureStatus(ctx, app)
	if err != nil {
		return ReachabilityResult{}, fmt.Errorf("reachability %s: reading exposure: %w", app, err)
	}
	res.Exposed = exp.Exposed
	res.Host = exp.Host
	res.Address = exp.Address

	if exp.Exposed && exp.Host != "" {
		if addrs, err := e.resolver.LookupHost(ctx, exp.Host); err == nil {
			res.DNSAddresses = addrs
			for _, a := range addrs {
				if exp.Address != "" && a == exp.Address {
					res.DNSPointsAtCluster = true
					break
				}
			}
		}
	}

	res.Reachable = res.Ready && res.Exposed && res.Address != "" && res.DNSPointsAtCluster
	res.Summary = reachabilitySummary(res)
	return res, nil
}

// reachabilitySummary turns the chain into a one-line, plain-English verdict naming the
// first unsatisfied link and the next action (ADR-0022's novice altitude).
func reachabilitySummary(r ReachabilityResult) string {
	switch {
	case !r.Ready:
		return fmt.Sprintf("%s is deployed but not ready yet — check `burrow logs %s`.", r.App, r.App)
	case !r.Exposed:
		return fmt.Sprintf("%s is running but not exposed — run `burrow expose %s --host <name> --port <n>`.", r.App, r.App)
	case r.Address == "":
		return fmt.Sprintf("%s is exposed at %s but no external address is assigned yet — is an ingress controller installed and running?", r.App, r.Host)
	case !r.DNSPointsAtCluster:
		return fmt.Sprintf("%s is exposed at %s, but DNS for %s doesn't point at the cluster yet — add a DNS record pointing %s at %s.", r.App, r.Host, r.Host, r.Host, r.Address)
	default:
		return fmt.Sprintf("%s is reachable at http://%s.", r.App, r.Host)
	}
}

// Unexpose removes an app's exposure (its Service and Ingress). It does not affect the
// workload. Unexposing an app that was never exposed returns ErrNotFound.
func (e *Engine) Unexpose(ctx context.Context, app string) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("unexpose: %w: %w", ErrInvalid, err)
	}
	if err := e.k8s.Unexpose(ctx, app); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("unexpose %s: not exposed: %w", app, err)
		}
		return fmt.Errorf("unexpose %s: %w", app, err)
	}
	return nil
}

// Guardrails returns the current guardrail policy as a list for inspection (ADR-0020).
func (e *Engine) Guardrails(ctx context.Context) ([]GuardrailInfo, error) {
	p, err := e.db.Policy(ctx)
	if err != nil {
		return nil, fmt.Errorf("guardrails: loading policy: %w", err)
	}
	return p.Guardrails(), nil
}

// SetGuardrail sets one guardrail's disposition (ADR-0020). It rejects an unknown guardrail
// or an invalid disposition as ErrInvalid. This is the operator's lever — exposed via the
// CLI, never as an MCP tool, so the agent cannot change its own guardrails.
func (e *Engine) SetGuardrail(ctx context.Context, code GuardrailCode, d Disposition) error {
	if !KnownGuardrail(code) {
		return fmt.Errorf("set guardrail: unknown guardrail %q: %w", code, ErrInvalid)
	}
	if !d.Valid() {
		return fmt.Errorf("set guardrail: invalid disposition %q (want allow, confirm, or deny): %w", d, ErrInvalid)
	}
	return e.db.SetGuardrail(ctx, code, d)
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

	spec := WorkloadSpec{App: app, Kind: WorkloadDeployment, Image: target.Image, Env: target.Env, Command: target.Command, Replicas: target.Replicas}
	if err := e.k8s.ApplyWorkload(ctx, spec); err != nil {
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
