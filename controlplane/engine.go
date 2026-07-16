// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Engine is the control plane's deploy orchestrator: the product. It turns an agent's
// deploy / status / logs / rollback / scale requests into guarded operations against
// the cluster, records every deploy, and returns structured results
// (ADR-0002, ADR-0006). It owns no global state and reads no ambient time or
// randomness — every external dependency is an injected seam (ADR-0010), so the engine
// is deterministic and unit-testable against fakes.
type Engine struct {
	k8s         Kubernetes
	db          Database
	clock       Clock
	ids         IDSource
	resolver    Resolver
	credentials Credentials
	dns         DNSFactory
	// logs maps a backend id (e.g. "victorialogs", "loki") to the querier that serves it.
	// Optional: a logs query errors cleanly when the map is empty or has no querier for the
	// add-on's backend (ADR-0026).
	logs map[string]LogsQuerier
	// metrics maps a backend id (e.g. "prometheus", "victoriametrics") to the querier that serves it.
	// Optional: a metrics query errors cleanly when the map is empty or has no querier for the
	// add-on's backend (ADR-0026).
	metrics map[string]MetricsQuerier
	// dbProvisioner provisions a per-app database and role on the installed Postgres add-on
	// (ADR-0031). Optional: an attach errors cleanly (ErrNotImplemented) when it is nil.
	dbProvisioner DatabaseProvisioner
	// prober detects the cluster's read-only capabilities (ADR-0034). Optional: a capabilities
	// read errors cleanly (ErrNotImplemented) when it is nil.
	prober ClusterProber
	// registry lists an image repository's tags for the auto-deploy read/watch (ADR-0052).
	// Optional: nil is allowed, and the auto-deploy show degrades to reporting the level alone
	// when it is not wired. It is OUTBOUND-only and never touched on the core deploy path, which
	// stays independent of registry reachability (ADR-0040).
	registry RegistryClient
	// builder builds an image from a git source reference inside the cluster for the optional
	// in-cluster build path (ADR-0053). Optional: nil is allowed, and a build errors cleanly
	// (ErrNotImplemented) when it is not wired — Burrow stays client-build-first, so build is never
	// required for deploy (ADR-0053 §1).
	builder Builder
	// buildRegistry is the in-cluster registry reference host:port that the optional in-cluster
	// build defaults its push target to when the caller supplies none (ADR-0053 §5). Optional: when
	// empty, an in-cluster build requires an explicit target and a missing one errors. A
	// caller-supplied target always overrides it, so external registries stay fully supported.
	buildRegistry string
	// appNamespace is the namespace burrowd deploys apps into (BURROW_NAMESPACE) — the namespace
	// of the implicit `default` environment (ADR-0035 phase 2). It mirrors the kube Adapter's
	// namespace so the engine can synthesize the default environment in ListEnvironments.
	appNamespace string
}

// Deps are the dependencies an Engine needs. All seams are required. The guardrail policy
// is not a dependency here: the engine reads the live policy from the Database seam on each
// guarded operation (ADR-0020), so a `guard set` takes effect without restarting.
type Deps struct {
	Kubernetes Kubernetes
	Database   Database
	Clock      Clock
	IDs        IDSource
	Resolver   Resolver
	// Credentials reads vendor tokens from the burrow-credentials Secret (ADR-0023).
	Credentials Credentials
	// DNS builds a DNSProvider for a vendor type and token (ADR-0023).
	DNS DNSFactory
	// Logs maps a backend id (e.g. "victorialogs", "loki") to the querier serving an installed
	// or connected logs add-on. Optional — an empty or nil map is allowed, and the engine errors
	// cleanly on a logs query when no querier is wired for the add-on's backend (ADR-0026).
	Logs map[string]LogsQuerier
	// Metrics maps a backend id (e.g. "prometheus", "victoriametrics") to the querier serving an
	// installed or connected metrics add-on. Optional — an empty or nil map is allowed, and the
	// engine errors cleanly on a metrics query when no querier is wired for the add-on's backend
	// (ADR-0026).
	Metrics map[string]MetricsQuerier
	// DatabaseProvisioner provisions a per-app database and role on the installed Postgres add-on
	// (ADR-0031). Optional — nil is allowed, and the engine errors cleanly (ErrNotImplemented) on a
	// Postgres attach when it is not wired.
	DatabaseProvisioner DatabaseProvisioner
	// ClusterProber detects the cluster's read-only capabilities (ADR-0034). Optional — nil is
	// allowed, and the engine errors cleanly (ErrNotImplemented) on a capabilities read when it is
	// not wired.
	ClusterProber ClusterProber
	// RegistryClient lists an image repository's tags for the auto-deploy read/watch (ADR-0052).
	// Optional — nil is allowed, and the auto-deploy show degrades to reporting the level alone
	// when it is not wired. It is OUTBOUND-only and never used on the core deploy path (ADR-0040).
	RegistryClient RegistryClient
	// Builder builds an image from a git source reference inside the cluster for the optional
	// in-cluster build path (ADR-0053). Optional — nil is allowed, and the engine errors cleanly
	// (ErrNotImplemented) on a build when it is not wired.
	Builder Builder
	// BuildRegistry is the in-cluster registry reference host:port the in-cluster build defaults its
	// push target to when the caller supplies none — the zero-config default push target for a build
	// (ADR-0053 §5). Optional — an empty value means a build with no explicit target errors, and a
	// caller-supplied target always overrides it (external registries stay fully supported). burrowd
	// sets it from BURROW_BUILD_REGISTRY, which `install --with-registry` wires to the in-cluster
	// registry it deploys.
	BuildRegistry string
	// AppNamespace is the namespace burrowd deploys apps into (BURROW_NAMESPACE) — the namespace of
	// the implicit `default` environment (ADR-0035 phase 2). Optional — an empty value defaults to
	// "default", matching the kube Adapter.
	AppNamespace string
}

// New constructs an Engine, validating that every seam is supplied and the policy is
// coherent. It returns an error rather than panicking so wiring mistakes surface at
// startup.
func New(d Deps) (*Engine, error) {
	switch {
	case d.Kubernetes == nil:
		return nil, fmt.Errorf("controlplane: New: Kubernetes seam is required")
	case d.Database == nil:
		return nil, fmt.Errorf("controlplane: New: Database seam is required")
	case d.Clock == nil:
		return nil, fmt.Errorf("controlplane: New: Clock seam is required")
	case d.IDs == nil:
		return nil, fmt.Errorf("controlplane: New: IDs seam is required")
	case d.Resolver == nil:
		return nil, fmt.Errorf("controlplane: New: Resolver seam is required")
	case d.Credentials == nil:
		return nil, fmt.Errorf("controlplane: New: Credentials seam is required")
	case d.DNS == nil:
		return nil, fmt.Errorf("controlplane: New: DNS seam is required")
	}
	appNamespace := d.AppNamespace
	if appNamespace == "" {
		appNamespace = "default"
	}
	return &Engine{
		k8s:           d.Kubernetes,
		db:            d.Database,
		clock:         d.Clock,
		ids:           d.IDs,
		resolver:      d.Resolver,
		credentials:   d.Credentials,
		dns:           d.DNS,
		logs:          d.Logs,
		metrics:       d.Metrics,
		dbProvisioner: d.DatabaseProvisioner,
		prober:        d.ClusterProber,
		registry:      d.RegistryClient,
		builder:       d.Builder,
		buildRegistry: d.BuildRegistry,
		appNamespace:  appNamespace,
	}, nil
}

// resolveReplicas computes the effective replica count for a workload apply — deploy, rollback, or a
// config/secret reapply. A deploy ships the image; it must not rescale the app or fight an active
// autoscaler. The rules, in order: (1) an active HorizontalPodAutoscaler owns the count, so the
// current Deployment's desired replicas are preserved and the request is ignored; (2) otherwise an
// explicit request (> 0) is honored — deploy-time scaling stays possible without an HPA; (3)
// otherwise (unspecified / 0) the current count is preserved, defaulting to 1 for a new app with no
// Deployment yet. The result is therefore always >= 1: a deploy can never scale an app to zero, and
// explicit scale-to-zero stays a `burrow scale <app> 0` operation. k is the namespace-scoped view.
func (e *Engine) resolveReplicas(ctx context.Context, k Kubernetes, app string, requested int32) (int32, error) {
	current, hasCurrent, err := currentReplicas(ctx, k, app)
	if err != nil {
		return 0, err
	}
	active, err := k.AutoscalerActive(ctx, app)
	if err != nil {
		return 0, fmt.Errorf("checking autoscaler for %s: %w", app, err)
	}
	switch {
	case active:
		// The HPA owns the count; preserve what is running, or default to 1 for the unusual case of
		// an HPA with no Deployment yet, never resetting to zero.
		if hasCurrent {
			return current, nil
		}
		return 1, nil
	case requested > 0:
		return requested, nil
	case hasCurrent:
		return current, nil
	default:
		return 1, nil // a new app with no Deployment and no explicit count
	}
}

// currentReplicas returns app's current desired replica count and whether a Deployment exists.
// ErrNotFound (a new app with nothing running) is reported as hasCurrent == false, not an error.
func currentReplicas(ctx context.Context, k Kubernetes, app string) (replicas int32, hasCurrent bool, err error) {
	st, err := k.WorkloadStatus(ctx, app)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("reading current replicas for %s: %w", app, err)
	}
	return st.DesiredReplicas, true, nil
}

// deployProvenance records how a deploy was triggered, so the release and audit row carry it
// (ADR-0052 §5). Public Deploy stamps a manual provenance; the Phase 4b pull-based watcher will call
// the unexported deploy with an auto provenance carrying the level and the resolved tag.
type deployProvenance struct {
	trigger ReleaseTrigger
	level   AutoDeployLevel // set only for an auto trigger
	tag     string          // the resolved tag the watcher took, set only for an auto trigger
}

// manualProvenance is the provenance of an explicit CLI or agent deploy — the default for every
// deploy today (ADR-0052 §5).
func manualProvenance() deployProvenance { return deployProvenance{trigger: TriggerManual} }

// Deploy rolls out an image by reference (ADR-0007). It validates the request, applies
// the guardrails, records a new release, applies it to the cluster, and records the
// outcome — superseding the previously running release on success. burrowd never contacts
// the registry: the workload is applied by image reference and the kubelet resolves and
// pulls it with the imagePullSecret (ADR-0040). The image bytes never pass through here;
// only the reference does (ADR-0004).
//
// Deploy is the explicit, human- or agent-triggered path (ADR-0007): it records a manual
// provenance. The Phase 4b pull-based watcher (ADR-0052) will drive the same rollout through the
// unexported deploy with an auto provenance.
func (e *Engine) Deploy(ctx context.Context, req DeployRequest) (DeployResult, error) {
	return e.deploy(ctx, req, manualProvenance())
}

// deploy is the shared rollout path behind the explicit Deploy and the Phase 4b auto-update watcher.
// prov records how the deploy was triggered (ADR-0052 §5): it stamps the release and audit row, and
// a manual deploy that moves the app to a strictly lower semver than it is running disables
// auto-deploy so the watcher does not fight the deliberate downgrade (§5).
func (e *Engine) deploy(ctx context.Context, req DeployRequest, prov deployProvenance) (DeployResult, error) {
	if err := (App{Name: req.App}).Validate(); err != nil {
		return DeployResult{}, fmt.Errorf("deploy: %w: %w", ErrInvalid, err)
	}
	if req.Image == "" {
		return DeployResult{}, fmt.Errorf("deploy %s: image reference is empty: %w", req.App, ErrInvalid)
	}
	if req.Replicas < 0 {
		return DeployResult{}, fmt.Errorf("deploy %s: replicas %d is negative: %w", req.App, req.Replicas, ErrInvalid)
	}
	// Resolve the target environment to its namespace up front so an unknown environment fails fast,
	// before the guardrail decision or any cluster write (ADR-0035 phase 2b).
	ns, err := e.resolveMutatingNamespace(ctx, req.Env)
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: %w", req.App, err)
	}
	k := e.k8s.WithNamespace(ns)
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: loading guardrail policy: %w", req.App, err)
	}
	// Resolve the effective replica count before the guardrail so the guardrail sees the real count:
	// a deploy ships the image and must not rescale — an active HPA keeps its count, an unspecified
	// count preserves the running value (or 1 for a new app), and only an explicit count without an
	// HPA changes scale. The resolved count is always >= 1, so a deploy never trips scale-to-zero
	// (ADR-0007).
	replicas, err := e.resolveReplicas(ctx, k, req.App, req.Replicas)
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: %w", req.App, err)
	}
	args := map[string]string{"image": req.Image, "replicas": strconv.Itoa(int(replicas)), "env": envName(req.Env), "trigger": string(prov.trigger)}
	// An auto deploy records the level that applied and the tag the watcher took, so the audit trail
	// distinguishes an unattended update from an explicit one (ADR-0052 §5).
	if prov.trigger == TriggerAuto {
		args["auto_level"] = string(prov.level)
		args["auto_tag"] = prov.tag
	}
	if err := e.recordDecision(ctx, auditOpDeploy, req.App, args, GuardrailAppDeploy,
		pol.evaluateDeploy(req.Env, replicas, req.Confirm)); err != nil {
		return DeployResult{}, err
	}

	releases, err := e.db.Releases(ctx, req.App, envName(req.Env))
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: reading release history: %w", req.App, err)
	}
	prev, hasPrev := lastDeployed(releases)

	// Env is app-global current state held in the store, the single source of truth (ADR-0028):
	// load it here and render it into the workload rather than taking it from the request, so a
	// release boots with whatever env the app currently has set.
	env, err := e.db.AppEnv(ctx, req.App)
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: reading env: %w", req.App, err)
	}

	rel := Release{
		ID:          e.ids.NewID(),
		App:         req.App,
		Environment: envName(req.Env),
		Image:       req.Image,
		Env:         env,
		Command:     req.Command,
		MetricsPort: req.MetricsPort,
		Replicas:    replicas,
		Status:      ReleasePending,
		Trigger:     prov.trigger,
		CreatedAt:   e.clock.Now(),
	}
	if prov.trigger == TriggerAuto {
		rel.AutoLevel = prov.level
		rel.AutoTag = prov.tag
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

	// The execution-row args carry the env KEY NAMES only — never values (ADR-0027).
	args["env_keys"] = auditKeys(env)

	spec := WorkloadSpec{App: req.App, Kind: WorkloadDeployment, Image: req.Image, Env: env, Command: req.Command, MetricsPort: req.MetricsPort, Replicas: replicas, ReleaseID: rel.ID}
	if err := k.ApplyWorkload(ctx, spec); err != nil {
		rel.Status = ReleaseFailed
		_ = e.db.SaveRelease(ctx, rel) // best effort: record the failure
		e.recordExecution(ctx, auditOpDeploy, req.App, args, err)
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
	e.recordExecution(ctx, auditOpDeploy, req.App, args, nil)

	// A manual deploy that moves the app to a strictly lower semver than it was running is a
	// deliberate downgrade: disable auto-deploy so the watcher does not re-apply the higher version
	// the operator just backed away from (ADR-0052 §5). A forward manual deploy leaves the level
	// untouched, and an auto deploy never disables. The deploy has landed and is recorded, so a
	// disable failure is surfaced by returning it wrapped.
	if prov.trigger == TriggerManual && hasPrev && isDowngrade(imageTag(prev.Image), imageTag(req.Image)) {
		if err := e.db.DisableAutoDeploy(ctx, req.App, envName(req.Env), reasonDisabledByDowngrade); err != nil {
			return DeployResult{}, fmt.Errorf("deploy %s: disabling auto-deploy after downgrade: %w", req.App, err)
		}
	}
	res := DeployResult{Release: rel, SupersededReleaseID: superseded}
	// Nudge toward semver when the deployed tag cannot be classified for auto-update (ADR-0052 §8).
	// This is a non-blocking hint on an otherwise-successful deploy, not a gate: the deploy has
	// already landed. An auto deploy always carries a semver tag, so only a manual non-semver deploy
	// ever trips it.
	if stableSemver(imageTag(req.Image)) == "" {
		res.Hints = append(res.Hints, nonSemverDeployHint)
	}
	return res, nil
}

// SetConfig upserts one non-secret config var for an app in the config store (ADR-0028). The store
// is the single source of truth for the app's config. By default the change re-applies the running
// workload so it rolls and the running app picks the value up; with noRestart the value is only
// persisted and lands on the next deploy. An app with no running release simply persists and
// skips the apply — not an error. Config vars are non-secret, so there is no guardrail.
func (e *Engine) SetConfig(ctx context.Context, app, env, key, value string, noRestart bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("set config: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return fmt.Errorf("set config %s: %w: %w", app, ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return fmt.Errorf("set config %s: %w", app, err)
	}
	if err := e.db.SetAppEnv(ctx, app, key, value); err != nil {
		return fmt.Errorf("set config %s: persisting %s: %w", app, key, err)
	}
	if noRestart {
		return nil
	}
	return e.reapplyEnv(ctx, e.k8s.WithNamespace(ns), app, envName(env))
}

// UnsetConfig removes one config var for an app from the config store (ADR-0028). Like SetConfig it
// re-applies the running workload by default so the running app drops the value, or only
// persists with noRestart. An app with no running release simply persists and skips the apply.
func (e *Engine) UnsetConfig(ctx context.Context, app, env, key string, noRestart bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("unset config: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return fmt.Errorf("unset config %s: %w: %w", app, ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return fmt.Errorf("unset config %s: %w", app, err)
	}
	if err := e.db.UnsetAppEnv(ctx, app, key); err != nil {
		return fmt.Errorf("unset config %s: removing %s: %w", app, key, err)
	}
	if noRestart {
		return nil
	}
	return e.reapplyEnv(ctx, e.k8s.WithNamespace(ns), app, envName(env))
}

// ListConfig returns the app's non-secret config store (ADR-0028). An app with no config yields an
// empty map and no error.
func (e *Engine) ListConfig(ctx context.Context, app, env string) (map[string]string, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return nil, fmt.Errorf("list config: %w: %w", ErrInvalid, err)
	}
	// Resolve the environment so an unknown name is a clear error, even though the config store is
	// app-global today: its values are sourced into whichever environment's namespace a deploy
	// targets (ADR-0035 phase 2b).
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return nil, fmt.Errorf("list config %s: %w", app, err)
	}
	cfg, err := e.db.AppEnv(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("list config %s: %w", app, err)
	}
	return cfg, nil
}

// reapplyEnv re-renders the running workload with the current store env so a mutation rolls the
// Deployment (ADR-0028). It reconstructs the WorkloadSpec from the app's currently running release
// and the store. With no running release there is nothing to roll: the change is persisted and
// will land on the next deploy, so this is a no-op, not an error.
func (e *Engine) reapplyEnv(ctx context.Context, k Kubernetes, app, env string) error {
	releases, err := e.db.Releases(ctx, app, env)
	if err != nil {
		return fmt.Errorf("set env %s: reading release history: %w", app, err)
	}
	cur, ok := lastDeployed(releases)
	if !ok {
		return nil // no running workload yet; the change lands on the next deploy
	}
	cfg, err := e.db.AppEnv(ctx, app)
	if err != nil {
		return fmt.Errorf("set env %s: reading env: %w", app, err)
	}
	// A config/secret reapply re-renders the running workload; it must not rescale it. Resolve with
	// no explicit request so the current count is preserved (or the HPA left to own it).
	replicas, err := e.resolveReplicas(ctx, k, app, 0)
	if err != nil {
		return fmt.Errorf("set env %s: %w", app, err)
	}
	spec := WorkloadSpec{App: app, Kind: WorkloadDeployment, Image: cur.Image, Env: cfg, Command: cur.Command, MetricsPort: cur.MetricsPort, Replicas: replicas, ReleaseID: cur.ID}
	if err := k.ApplyWorkload(ctx, spec); err != nil {
		return fmt.Errorf("set env %s: applying to cluster: %w", app, err)
	}
	return nil
}

// ListSecrets returns the env-var KEYS in an app's per-app Secret, sorted, never the values
// (ADR-0028/0004). Secret values live only in the Kubernetes Secret and never cross the API or
// MCP, so this read returns keys only. An app with no secrets yields an empty slice.
func (e *Engine) ListSecrets(ctx context.Context, app, env string) ([]string, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return nil, fmt.Errorf("list secrets: %w: %w", ErrInvalid, err)
	}
	ns, err := e.resolveNamespace(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("list secrets %s: %w", app, err)
	}
	keys, err := e.k8s.WithNamespace(ns).SecretKeys(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("list secrets %s: %w", app, err)
	}
	return keys, nil
}

// SetSecret upserts one key=value into an app's per-app Secret and, unless noRestart, rolls the
// running workload so it picks the value up (ADR-0029). The value arrives over burrowd's
// authenticated control-plane API and is written here through the Kubernetes seam; it is NEVER
// logged, never audited, never stored in Postgres, and never carried over MCP — only its KEY name
// appears in any error (the value is never formatted into one). Setting a value still cannot be
// done over MCP: there is no secret-set MCP tool. An app with no running workload just writes the
// Secret; the change lands on the next deploy.
func (e *Engine) SetSecret(ctx context.Context, app, env, key, value string, noRestart bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("set secret: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return fmt.Errorf("set secret %s: %w: %w", app, ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return fmt.Errorf("set secret %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)
	if err := k.SetSecretValue(ctx, app, key, value); err != nil {
		// Wrap with the app and key NAME only — never the value (ADR-0029).
		return fmt.Errorf("set secret %s: writing %s: %w", app, key, err)
	}
	if noRestart {
		return nil
	}
	// envFrom is read only at pod start, so writing a value under an existing key does not roll
	// the Deployment on its own — bump the restart annotation. A missing workload means nothing is
	// running yet: not an error, the change lands on the next deploy.
	if err := k.RestartWorkload(ctx, app, e.clock.Now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("set secret %s: rolling workload: %w", app, err)
	}
	return nil
}

// UnsetSecret removes one key from an app's per-app Secret and, unless noRestart, rolls the
// running workload so it drops the value (ADR-0028). Removing a key carries no value, and this is
// MCP-allowed. An app with no running workload just updates the Secret; the change lands on the
// next deploy. Removing an absent key succeeds.
func (e *Engine) UnsetSecret(ctx context.Context, app, env, key string, noRestart bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("unset secret: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return fmt.Errorf("unset secret %s: %w: %w", app, ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return fmt.Errorf("unset secret %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)
	if err := k.UnsetSecretKey(ctx, app, key); err != nil {
		return fmt.Errorf("unset secret %s: removing %s: %w", app, key, err)
	}
	if noRestart {
		return nil
	}
	// envFrom is read only at pod start, so removing a key from the Secret does not roll the
	// Deployment on its own — bump the restart annotation. A missing workload means nothing is
	// running yet: not an error, the change lands on the next deploy.
	if err := k.RestartWorkload(ctx, app, e.clock.Now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("unset secret %s: rolling workload: %w", app, err)
	}
	return nil
}

// Status returns the combined control-plane and cluster view of an app: the most recent
// ListApps returns the workload status of every Burrow-managed app, for an apps listing. It
// reads the cluster — the source of truth for what is running.
func (e *Engine) ListApps(ctx context.Context, env string) ([]WorkloadStatus, error) {
	ns, err := e.resolveNamespace(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	apps, err := e.k8s.WithNamespace(ns).ListWorkloads(ctx)
	if err != nil {
		return nil, fmt.Errorf("list apps: reading cluster: %w", err)
	}
	return apps, nil
}

// InstallAddon deploys the vetted backing service for the named add-on type and registers it as
// a queryable capability (ADR-0025/0026). It is guarded by addon.install.
func (e *Engine) InstallAddon(ctx context.Context, t AddonType, confirm bool) (AddonInfo, error) {
	spec, ok := LookupAddon(t)
	if !ok {
		return AddonInfo{}, fmt.Errorf("install addon: unknown type %q: %w", t, ErrInvalid)
	}
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return AddonInfo{}, fmt.Errorf("install addon %s: loading guardrail policy: %w", t, err)
	}
	args := map[string]string{"type": string(t), "image": spec.Image}
	if err := e.recordDecision(ctx, auditOpAddonInstall, string(t), args, GuardrailAddonInstall,
		pol.evaluateGuardrail("", "addon install", GuardrailAddonInstall, confirm, fmt.Sprintf("installing the %s add-on (%s)", t, spec.Image))); err != nil {
		return AddonInfo{}, err
	}
	info, err := e.k8s.DeployAddon(ctx, spec)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonInstall, string(t), args, err)
		return AddonInfo{}, fmt.Errorf("install addon %s: %w", t, err)
	}
	// Record the add-on in the registry — the DB is the source of truth for what add-ons exist
	// (ADR-0025), like the provider registry. Readiness is never stored; it is probed live.
	info.CreatedAt = e.clock.Now()
	if err := e.db.SaveAddon(ctx, info); err != nil {
		e.recordExecution(ctx, auditOpAddonInstall, string(t), args, err)
		return AddonInfo{}, fmt.Errorf("install addon %s: recording in the registry: %w", t, err)
	}
	e.recordExecution(ctx, auditOpAddonInstall, string(t), args, nil)
	return info, nil
}

// ConnectAddon registers an existing backend the user already runs (e.g. an in-cluster Loki) as a
// queryable add-on, recording its endpoint and derived capabilities in the registry (ADR-0026).
// Unlike install it deploys nothing and is not guarded — connect is registration-only. Connecting
// the same backend twice upserts, updating the endpoint. secretKey is the (non-secret) key under
// which a bearer token for an authenticated backend lives in the burrow-credentials Secret; "" means
// the backend is unauthenticated. token is the bearer token VALUE for an authenticated backend: it
// arrives over burrowd's authenticated control-plane API and is written into burrow-credentials under
// secretKey (ADR-0030). The value is never logged, never stored in Postgres, never returned, and
// never carried over MCP — only the key is recorded in the registry (ADR-0004/0023). A token without
// a secretKey is invalid. The registry entry that crosses the API holds only the key.
func (e *Engine) ConnectAddon(ctx context.Context, backend, endpoint, secretKey, token string) (AddonInfo, error) {
	b, ok := LookupConnectBackend(backend)
	if !ok {
		return AddonInfo{}, fmt.Errorf("connect addon: unknown backend %q: %w", backend, ErrInvalid)
	}
	if endpoint == "" {
		return AddonInfo{}, fmt.Errorf("connect addon %s: endpoint is empty: %w", backend, ErrInvalid)
	}
	if token != "" && secretKey == "" {
		return AddonInfo{}, fmt.Errorf("connect addon %s: a token needs a secret key to store it under: %w", backend, ErrInvalid)
	}
	// Write the bearer token into burrow-credentials before recording the entry, so a connected
	// authenticated backend has its credential available the first time it is queried. The value is
	// used here and never logged or placed in an error.
	if token != "" {
		if err := e.credentials.SetToken(ctx, secretKey, token); err != nil {
			return AddonInfo{}, fmt.Errorf("connect addon %s: storing the token: %w", backend, err)
		}
	}
	info := AddonInfo{
		Name:         backend,
		Type:         AddonType(backend),
		Mode:         "connected",
		Backend:      backend,
		Endpoint:     endpoint,
		Capabilities: b.Capabilities,
		SecretKey:    secretKey,
		CreatedAt:    e.clock.Now(),
	}
	if err := e.db.SaveAddon(ctx, info); err != nil {
		return AddonInfo{}, fmt.Errorf("connect addon %s: recording in the registry: %w", backend, err)
	}
	return info, nil
}

// ListAddons returns the registered add-on instances from the registry, with live readiness
// probed from the cluster for installed ones (ADR-0025). A readiness probe failure leaves an
// entry not-ready rather than failing the whole listing.
func (e *Engine) ListAddons(ctx context.Context) ([]AddonInfo, error) {
	addons, err := e.db.Addons(ctx)
	if err != nil {
		return nil, fmt.Errorf("list addons: reading the registry: %w", err)
	}
	for i := range addons {
		if addons[i].Mode != "installed" {
			continue
		}
		ready, err := e.k8s.AddonReady(ctx, addons[i].Name)
		if err != nil {
			continue // leave Ready=false; a probe failure must not fail the listing
		}
		addons[i].Ready = ready
	}
	return addons, nil
}

// RemoveAddon removes the named add-on instance. It is guarded by addon.remove (removing a
// backing service can break dependent apps).
func (e *Engine) RemoveAddon(ctx context.Context, name string, confirm bool) error {
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return fmt.Errorf("remove addon %s: loading guardrail policy: %w", name, err)
	}
	if err := e.recordDecision(ctx, auditOpAddonRemove, name, nil, GuardrailAddonRemove,
		pol.evaluateGuardrail("", "addon remove", GuardrailAddonRemove, confirm, fmt.Sprintf("removing the add-on %q", name))); err != nil {
		return err
	}
	// The registry is the source of truth for what add-ons exist (ADR-0025): load it first so an
	// unknown add-on is ErrNotFound, and only tear down cluster resources for an installed one.
	info, err := e.db.Addon(ctx, name)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonRemove, name, nil, err)
		return fmt.Errorf("remove addon %s: %w", name, err)
	}
	if info.Mode == "installed" {
		if err := e.k8s.DeleteAddon(ctx, name); err != nil {
			e.recordExecution(ctx, auditOpAddonRemove, name, nil, err)
			return fmt.Errorf("remove addon %s: %w", name, err)
		}
	}
	if err := e.db.DeleteAddon(ctx, name); err != nil {
		e.recordExecution(ctx, auditOpAddonRemove, name, nil, err)
		return fmt.Errorf("remove addon %s: %w", name, err)
	}
	e.recordExecution(ctx, auditOpAddonRemove, name, nil, nil)
	return nil
}

// AttachResult is the outcome of attaching an app to an add-on (ADR-0031). It carries the KEY
// NAME the connection string was written under (e.g. "DATABASE_URL") — never the value, which
// lives only in the app's Kubernetes Secret.
type AttachResult struct {
	App   string    `json:"app"`
	Addon AddonType `json:"addon"`
	// SecretKey is the env-var name under which the generated connection string was written into
	// the app's per-app Secret. The value is never returned (ADR-0029/0031).
	SecretKey string `json:"secret_key"`
}

// AttachAddon gives app its own database on the installed Postgres add-on and wires it into the
// app (ADR-0031). burrowd provisions an isolated database + login role on the shared instance,
// generates the DATABASE_URL server-side, writes it into the app's per-app Secret via the
// SetSecretValue path (ADR-0029), and restarts the app so envFrom picks it up. Attach provisions
// and destroys nothing, so it is allowed by default (no guardrail) and is safe over MCP: no secret
// value crosses MCP — the agent supplies only the app name; burrowd generates the value and never
// returns it. The audit row records {addon, app} only — never the URL.
func (e *Engine) AttachAddon(ctx context.Context, t AddonType, app string) (AttachResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return AttachResult{}, fmt.Errorf("attach addon: %w: %w", ErrInvalid, err)
	}
	if t != AddonPostgres {
		return AttachResult{}, fmt.Errorf("attach addon %s: only the postgres add-on supports attach: %w", t, ErrInvalid)
	}
	if e.dbProvisioner == nil {
		return AttachResult{}, fmt.Errorf("attach addon %s: database provisioning is not configured: %w", t, ErrNotImplemented)
	}
	// The redacted audit args carry the add-on and app NAMES only — never the generated URL (ADR-0031).
	args := map[string]string{"addon": string(t), "app": app}

	// Provision the database/role and compose the connection string. The returned url is a SECRET
	// value: from here it is handed only to SetSecretValue and never logged, audited, or returned.
	url, err := e.dbProvisioner.EnsureAppDatabase(ctx, app)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonAttach, app, args, err)
		// EnsureAppDatabase's error names the app/identifier only, never the URL.
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: %w", t, app, err)
	}

	// Write the connection string into the app's per-app Secret and roll the app to pick it up —
	// the ADR-0029 secret path, the same one `secret set` uses. The value never crosses the audit
	// log, MCP, or Postgres.
	const key = "DATABASE_URL"
	if err := e.k8s.SetSecretValue(ctx, app, key, url); err != nil {
		e.recordExecution(ctx, auditOpAddonAttach, app, args, err)
		// SetSecretValue's error names the app and key only — never the value.
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: writing %s: %w", t, app, key, err)
	}
	if err := e.k8s.RestartWorkload(ctx, app, e.clock.Now()); err != nil && !errors.Is(err, ErrNotFound) {
		e.recordExecution(ctx, auditOpAddonAttach, app, args, err)
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: rolling workload: %w", t, app, err)
	}
	e.recordExecution(ctx, auditOpAddonAttach, app, args, nil)
	return AttachResult{App: app, Addon: t, SecretKey: key}, nil
}

// DetachAddon removes app's DATABASE_URL and, behind the addon.detach confirm guardrail (it
// destroys data), drops app's database and role from the cluster-shared Postgres instance (ADR-0031). The
// audit row records {addon, app} only.
func (e *Engine) DetachAddon(ctx context.Context, t AddonType, app string, confirm bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("detach addon: %w: %w", ErrInvalid, err)
	}
	if t != AddonPostgres {
		return fmt.Errorf("detach addon %s: only the postgres add-on supports detach: %w", t, ErrInvalid)
	}
	if e.dbProvisioner == nil {
		return fmt.Errorf("detach addon %s: database provisioning is not configured: %w", t, ErrNotImplemented)
	}
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return fmt.Errorf("detach addon %s: loading guardrail policy: %w", t, err)
	}
	args := map[string]string{"addon": string(t), "app": app}
	if err := e.recordDecision(ctx, auditOpAddonDetach, app, args, GuardrailAddonDetach,
		pol.evaluateGuardrail("", "addon detach", GuardrailAddonDetach, confirm,
			fmt.Sprintf("detaching %q from the %s add-on (drops its database and role)", app, t))); err != nil {
		return err
	}

	// Remove the DATABASE_URL key first (the app stops seeing the credential), then drop the
	// database/role. A missing key is a no-op.
	if err := e.k8s.UnsetSecretKey(ctx, app, "DATABASE_URL"); err != nil {
		e.recordExecution(ctx, auditOpAddonDetach, app, args, err)
		return fmt.Errorf("detach addon %s for %s: removing DATABASE_URL: %w", t, app, err)
	}
	if err := e.dbProvisioner.DropAppDatabase(ctx, app); err != nil {
		e.recordExecution(ctx, auditOpAddonDetach, app, args, err)
		return fmt.Errorf("detach addon %s for %s: %w", t, app, err)
	}
	// Roll the app so it drops the removed credential. A missing workload is not an error.
	if err := e.k8s.RestartWorkload(ctx, app, e.clock.Now()); err != nil && !errors.Is(err, ErrNotFound) {
		e.recordExecution(ctx, auditOpAddonDetach, app, args, err)
		return fmt.Errorf("detach addon %s for %s: rolling workload: %w", t, app, err)
	}
	e.recordExecution(ctx, auditOpAddonDetach, app, args, nil)
	return nil
}

// BackupAddon backs up app's database on the installed Postgres add-on (ADR-0032): burrowd records a
// pending backup, runs an in-cluster Job that pg_dumps the database to the backup PVC, and marks the
// backup completed (or failed). The backup is recorded in the control-plane database — burrowd is not
// mounted to the backup PVC, so the database, not the volume, is the index of backups. It moves no
// secret value: the Job reads the superuser password only via secretKeyRef, and the audit row and the
// returned result name the add-on, app, backup id, path, and size — never a credential. Backup is
// allowed by default (it destroys nothing) and safe over MCP.
func (e *Engine) BackupAddon(ctx context.Context, t AddonType, app string) (BackupResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return BackupResult{}, fmt.Errorf("backup addon: %w: %w", ErrInvalid, err)
	}
	if t != AddonPostgres {
		return BackupResult{}, fmt.Errorf("backup addon %s: only the postgres add-on supports backup: %w", t, ErrInvalid)
	}

	backupID := e.ids.NewID()
	// The redacted audit args carry the add-on, app, and backup NAMES only — never a credential (ADR-0032).
	args := map[string]string{"addon": string(t), "app": app, "backup": backupID}

	backup := Backup{
		ID:        backupID,
		App:       app,
		CreatedAt: e.clock.Now(),
		Path:      BackupPath(app, backupID),
		Status:    BackupPending,
	}
	if err := e.db.RecordBackup(ctx, backup); err != nil {
		e.recordExecution(ctx, auditOpAddonBackup, app, args, err)
		return BackupResult{}, fmt.Errorf("backup addon %s for %s: recording backup: %w", t, app, err)
	}

	size, err := e.k8s.RunBackupJob(ctx, app, backupID)
	if err != nil {
		_ = e.db.SetBackupStatus(ctx, backupID, BackupFailed, 0)
		e.recordExecution(ctx, auditOpAddonBackup, app, args, err)
		return BackupResult{}, fmt.Errorf("backup addon %s for %s: %w", t, app, err)
	}
	if err := e.db.SetBackupStatus(ctx, backupID, BackupCompleted, size); err != nil {
		e.recordExecution(ctx, auditOpAddonBackup, app, args, err)
		return BackupResult{}, fmt.Errorf("backup addon %s for %s: recording completion: %w", t, app, err)
	}
	backup.Status = BackupCompleted
	backup.SizeBytes = size
	e.recordExecution(ctx, auditOpAddonBackup, app, args, nil)
	return BackupResult{Backup: backup}, nil
}

// ListBackups returns recorded backups, newest first, from the control-plane database (ADR-0032).
// An empty app lists every app's backups; a non-empty app restricts to that app. Read-only and safe
// over MCP — it names the app, size, time, and on-PVC path, never a credential.
func (e *Engine) ListBackups(ctx context.Context, t AddonType, app string) ([]Backup, error) {
	if t != AddonPostgres {
		return nil, fmt.Errorf("list backups %s: only the postgres add-on supports backups: %w", t, ErrInvalid)
	}
	if app != "" {
		if err := (App{Name: app}).Validate(); err != nil {
			return nil, fmt.Errorf("list backups: %w: %w", ErrInvalid, err)
		}
	}
	backups, err := e.db.ListBackups(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	return backups, nil
}

// RestoreAddon restores app's database from a recorded backup, overwriting its live contents
// (ADR-0032). It is behind the addon.restore confirm guardrail (it destroys live data), runs an
// in-cluster Job that pg_restores the named dump, and records the restore in the audit log. The Job
// reads the superuser password only via secretKeyRef; the audit row records {addon, app, backup}
// only — never a credential.
func (e *Engine) RestoreAddon(ctx context.Context, t AddonType, app, backupID string, confirm bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("restore addon: %w: %w", ErrInvalid, err)
	}
	if t != AddonPostgres {
		return fmt.Errorf("restore addon %s: only the postgres add-on supports restore: %w", t, ErrInvalid)
	}
	if backupID == "" {
		return fmt.Errorf("restore addon %s: a backup id is required: %w", t, ErrInvalid)
	}

	// The backup must exist and belong to the app — resolve it before evaluating the guardrail so a
	// bad id reads as ErrNotFound rather than a spurious confirmation prompt (mirrors Rollback).
	backup, err := e.db.GetBackup(ctx, backupID)
	if err != nil {
		return fmt.Errorf("restore addon %s for %s: backup %q: %w", t, app, backupID, err)
	}
	if backup.App != app {
		return fmt.Errorf("restore addon %s for %s: backup %q belongs to app %q: %w", t, app, backupID, backup.App, ErrInvalid)
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return fmt.Errorf("restore addon %s: loading guardrail policy: %w", t, err)
	}
	args := map[string]string{"addon": string(t), "app": app, "backup": backupID}
	if err := e.recordDecision(ctx, auditOpAddonRestore, app, args, GuardrailAddonRestore,
		pol.evaluateGuardrail("", "addon restore", GuardrailAddonRestore, confirm,
			fmt.Sprintf("restoring %q from backup %s (overwrites its live database)", app, backupID))); err != nil {
		return err
	}

	if err := e.k8s.RunRestoreJob(ctx, app, backupID); err != nil {
		e.recordExecution(ctx, auditOpAddonRestore, app, args, err)
		return fmt.Errorf("restore addon %s for %s: %w", t, app, err)
	}
	e.recordExecution(ctx, auditOpAddonRestore, app, args, nil)
	return nil
}

// DeleteApp removes an app entirely: its workload, its routing (Service/Ingress), and its
// release history, so the app disappears from the apps listing and from status. It is guarded
// by app.delete, which holds the destructive teardown for confirmation by default (ADR-0020).
// The app must exist — it has either recorded releases or a live workload; an app unknown to
// both is ErrNotFound. Teardown tolerates an already-absent piece: an ErrNotFound from the
// workload or routing delete means that piece is already gone, not a failure.
func (e *Engine) DeleteApp(ctx context.Context, app, env string, confirm bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("delete app: %w: %w", ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return fmt.Errorf("delete app %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)

	// Existence: an app exists if it has releases OR a live workload. Determine this before
	// evaluating the guardrail so an unknown app is ErrNotFound rather than a confirm prompt.
	releases, err := e.db.Releases(ctx, app, envName(env))
	if err != nil {
		return fmt.Errorf("delete app %s: reading release history: %w", app, err)
	}
	exists := len(releases) > 0
	if !exists {
		if _, err := k.WorkloadStatus(ctx, app); err != nil {
			if !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("delete app %s: reading workload: %w", app, err)
			}
		} else {
			exists = true
		}
	}
	if !exists {
		return fmt.Errorf("delete app %s: unknown app: %w", app, ErrNotFound)
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return fmt.Errorf("delete app %s: loading guardrail policy: %w", app, err)
	}
	args := map[string]string{"env": envName(env)}
	if err := e.recordDecision(ctx, auditOpAppDelete, app, args, GuardrailAppDelete,
		pol.evaluateGuardrail(env, "app delete", GuardrailAppDelete, confirm, fmt.Sprintf("deleting the app %q (its workload, routing, and release history)", app))); err != nil {
		return err
	}

	// Tear down, tolerating already-absent pieces: workload, then routing, then release records.
	if err := k.DeleteWorkload(ctx, app); err != nil && !errors.Is(err, ErrNotFound) {
		e.recordExecution(ctx, auditOpAppDelete, app, args, err)
		return fmt.Errorf("delete app %s: removing workload: %w", app, err)
	}
	if err := k.Unexpose(ctx, app); err != nil && !errors.Is(err, ErrNotFound) {
		e.recordExecution(ctx, auditOpAppDelete, app, args, err)
		return fmt.Errorf("delete app %s: removing routing: %w", app, err)
	}
	if err := e.db.DeleteReleases(ctx, app); err != nil {
		e.recordExecution(ctx, auditOpAppDelete, app, args, err)
		return fmt.Errorf("delete app %s: removing release history: %w", app, err)
	}
	e.recordExecution(ctx, auditOpAppDelete, app, args, nil)
	return nil
}

// hasCapability reports whether a carries the named capability.
func hasCapability(a AddonInfo, capability string) bool {
	for _, c := range a.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// selectAddon picks the add-on to query for a capability. With an empty backend it returns the
// first add-on advertising the capability (the historical first-match behavior). With a non-empty
// backend it returns the add-on advertising the capability whose concrete Backend (e.g.
// "victorialogs", "loki") OR registry Name matches — matching either is forgiving and intuitive
// when more than one add-on serves the same capability. The bool is false when nothing matches.
func selectAddon(addons []AddonInfo, capability, backend string) (AddonInfo, bool) {
	for _, a := range addons {
		if !hasCapability(a, capability) {
			continue
		}
		if backend == "" || a.Backend == backend || a.Name == backend {
			return a, true
		}
	}
	return AddonInfo{}, false
}

// availableBackends lists, in registry order, the add-on names that serve a capability — used to
// name the alternatives in a "no add-on with backend X" error.
func availableBackends(addons []AddonInfo, capability string) []string {
	var names []string
	for _, a := range addons {
		if hasCapability(a, capability) {
			names = append(names, a.Name)
		}
	}
	return names
}

// QueryLogs runs query against the installed logs add-on and returns up to limit records. It is
// the read path behind the agent's logs-query tool: it locates the add-on advertising the "logs"
// capability and queries it through the LogsQuerier seam (ADR-0026). An empty backend picks the
// first logs add-on; a non-empty backend targets a specific one (by its concrete backend or its
// registry name) when more than one serves logs.
func (e *Engine) QueryLogs(ctx context.Context, query string, limit int, backend string) ([]LogEntry, error) {
	if len(e.logs) == 0 {
		return nil, fmt.Errorf("query logs: logs querying is not configured: %w", ErrNotImplemented)
	}
	addons, err := e.db.Addons(ctx)
	if err != nil {
		return nil, fmt.Errorf("query logs: %w", err)
	}
	addon, found := selectAddon(addons, "logs", backend)
	if !found {
		if backend != "" {
			return nil, fmt.Errorf("query logs: no logs add-on with backend %q (have: %s): %w", backend, strings.Join(availableBackends(addons, "logs"), ", "), ErrNotFound)
		}
		return nil, fmt.Errorf("query logs: no logs add-on is installed — run `burrow addon install logs`: %w", ErrNotFound)
	}
	q := e.logs[addon.Backend]
	if q == nil {
		return nil, fmt.Errorf("query logs: no logs querier for backend %q: %w", addon.Backend, ErrNotImplemented)
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	// An authenticated backend records the key under which its bearer token lives in the
	// burrow-credentials Secret; read it at query time so a rotation is picked up with no restart
	// (ADR-0023). An empty SecretKey means the backend is unauthenticated — pass no token.
	token := ""
	if addon.SecretKey != "" {
		token, err = e.credentials.Token(ctx, addon.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("query logs: reading token for add-on %q under key %q: %w", addon.Name, addon.SecretKey, err)
		}
	}
	entries, err := q.QueryLogs(ctx, addon.Endpoint, query, limit, token)
	if err != nil {
		return nil, fmt.Errorf("query logs: %w", err)
	}
	return entries, nil
}

// QueryMetrics runs an instant PromQL query against the connected metrics add-on and returns the
// matching samples. It is the read path behind the agent's metrics-query tool: it locates the add-on
// advertising the "metrics" capability and queries it through the MetricsQuerier seam (ADR-0026). An
// empty backend picks the first metrics add-on; a non-empty backend targets a specific one (by its
// concrete backend or its registry name) when more than one serves metrics.
func (e *Engine) QueryMetrics(ctx context.Context, query string, backend string) ([]MetricSample, error) {
	if len(e.metrics) == 0 {
		return nil, fmt.Errorf("query metrics: metrics querying is not configured: %w", ErrNotImplemented)
	}
	addons, err := e.db.Addons(ctx)
	if err != nil {
		return nil, fmt.Errorf("query metrics: %w", err)
	}
	addon, found := selectAddon(addons, "metrics", backend)
	if !found {
		if backend != "" {
			return nil, fmt.Errorf("query metrics: no metrics add-on with backend %q (have: %s): %w", backend, strings.Join(availableBackends(addons, "metrics"), ", "), ErrNotFound)
		}
		return nil, fmt.Errorf("query metrics: no metrics add-on is connected — run `burrow addon connect prometheus`: %w", ErrNotFound)
	}
	q := e.metrics[addon.Backend]
	if q == nil {
		return nil, fmt.Errorf("query metrics: no metrics querier for backend %q: %w", addon.Backend, ErrNotImplemented)
	}
	// An authenticated backend records the key under which its bearer token lives in the
	// burrow-credentials Secret; read it at query time so a rotation is picked up with no restart
	// (ADR-0023). An empty SecretKey means the backend is unauthenticated — pass no token.
	token := ""
	if addon.SecretKey != "" {
		token, err = e.credentials.Token(ctx, addon.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("query metrics: reading token for add-on %q under key %q: %w", addon.Name, addon.SecretKey, err)
		}
	}
	samples, err := q.QueryMetrics(ctx, addon.Endpoint, query, token)
	if err != nil {
		return nil, fmt.Errorf("query metrics: %w", err)
	}
	return samples, nil
}

// recorded release and the live workload state. It returns ErrNotFound only when the
// app is unknown to both.
func (e *Engine) Status(ctx context.Context, app, env string) (StatusResult, error) {
	res := StatusResult{App: app}

	ns, err := e.resolveNamespace(ctx, env)
	if err != nil {
		return StatusResult{}, fmt.Errorf("status %s: %w", app, err)
	}

	latest, errL := e.db.LatestRelease(ctx, app, envName(env))
	if errL != nil && !errors.Is(errL, ErrNotFound) {
		return StatusResult{}, fmt.Errorf("status %s: reading release: %w", app, errL)
	}
	if errL == nil {
		res.HasRelease = true
		res.Release = latest
	}

	st, errK := e.k8s.WithNamespace(ns).WorkloadStatus(ctx, app)
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
func (e *Engine) Logs(ctx context.Context, app, env string, opts LogOptions) ([]LogLine, error) {
	ns, err := e.resolveNamespace(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("logs %s: %w", app, err)
	}
	lines, err := e.k8s.WithNamespace(ns).Logs(ctx, app, opts)
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
func (e *Engine) Scale(ctx context.Context, app, env string, replicas int32, confirm bool) (ScaleResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return ScaleResult{}, fmt.Errorf("scale: %w: %w", ErrInvalid, err)
	}
	if replicas < 0 {
		return ScaleResult{}, fmt.Errorf("scale %s: replicas %d is negative: %w", app, replicas, ErrInvalid)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return ScaleResult{}, fmt.Errorf("scale %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return ScaleResult{}, fmt.Errorf("scale %s: loading guardrail policy: %w", app, err)
	}
	args := map[string]string{"replicas": strconv.Itoa(int(replicas)), "env": envName(env)}
	if err := e.recordDecision(ctx, auditOpScale, app, args, "", pol.evaluateReplicas(env, "scale", replicas, confirm)); err != nil {
		return ScaleResult{}, err
	}

	st, err := k.WorkloadStatus(ctx, app)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ScaleResult{}, fmt.Errorf("scale %s: no running workload: %w", app, err)
		}
		return ScaleResult{}, fmt.Errorf("scale %s: reading current state: %w", app, err)
	}
	prev := st.DesiredReplicas

	if err := k.ScaleWorkload(ctx, app, replicas); err != nil {
		e.recordExecution(ctx, auditOpScale, app, args, err)
		return ScaleResult{}, fmt.Errorf("scale %s: %w", app, err)
	}
	e.recordExecution(ctx, auditOpScale, app, args, nil)
	return ScaleResult{App: app, PreviousReplicas: prev, Replicas: replicas}, nil
}

// metricsAbsentWarning is the note an autoscale carries when metrics-server is not detected: the HPA
// is applied (its creation needs no metrics-server), but it will not actually scale the app until
// metrics-server is installed to serve the CPU/memory metrics it reads. No em-dash: it is printed
// verbatim by the CLI.
const metricsAbsentWarning = "autoscaling needs metrics-server, which was not detected. The autoscaler is set but will not scale until metrics-server is installed."

// Autoscale configures autoscaling for an app: it applies an autoscaling/v2 HorizontalPodAutoscaler
// on the app's Deployment with the requested replica band and utilization targets (ADR-0006). It is
// guarded twice — the app.autoscale guardrail gates the operation (allow by default), and the
// app.replica_ceiling guardrail bounds the requested max the same way it bounds a manual scale, so a
// max above the ceiling is denied exactly like scaling above it. The HPA is applied even when
// metrics-server is absent (creating it needs no metrics); the result then carries a Warning that it
// will not scale until metrics-server is installed.
func (e *Engine) Autoscale(ctx context.Context, app, env string, spec AutoscaleSpec, confirm bool) (AutoscaleResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return AutoscaleResult{}, fmt.Errorf("autoscale: %w: %w", ErrInvalid, err)
	}
	if err := spec.validate(); err != nil {
		return AutoscaleResult{}, fmt.Errorf("autoscale %s: %w: %w", app, err, ErrInvalid)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return AutoscaleResult{}, fmt.Errorf("autoscale %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return AutoscaleResult{}, fmt.Errorf("autoscale %s: loading guardrail policy: %w", app, err)
	}
	args := map[string]string{
		"min":    strconv.Itoa(int(spec.MinReplicas)),
		"max":    strconv.Itoa(int(spec.MaxReplicas)),
		"cpu":    strconv.Itoa(int(spec.CPUPercent)),
		"memory": strconv.Itoa(int(spec.MemoryPercent)),
		"env":    envName(env),
	}
	if err := e.recordDecision(ctx, auditOpAutoscale, app, args, GuardrailAutoscale, pol.evaluateAutoscale(env, spec, confirm)); err != nil {
		return AutoscaleResult{}, err
	}

	if err := k.ApplyAutoscaler(ctx, app, spec); err != nil {
		e.recordExecution(ctx, auditOpAutoscale, app, args, err)
		return AutoscaleResult{}, fmt.Errorf("autoscale %s: %w", app, err)
	}
	e.recordExecution(ctx, auditOpAutoscale, app, args, nil)

	// metrics-server presence is a best-effort warning, never fatal: the HPA is already applied.
	metricsAvailable, warning := e.metricsAvailability(ctx, k)
	return AutoscaleResult{
		App:              app,
		Env:              envName(env),
		MinReplicas:      spec.MinReplicas,
		MaxReplicas:      spec.MaxReplicas,
		CPUPercent:       spec.CPUPercent,
		MemoryPercent:    spec.MemoryPercent,
		MetricsAvailable: metricsAvailable,
		Warning:          warning,
	}, nil
}

// metricsAvailability probes whether metrics-server is present through the workload seam, returning
// the warning when it is absent. It is best-effort: a discovery error is treated as absent (with the
// warning) rather than surfaced, so a probe hiccup never fails an autoscale whose HPA already applied.
func (e *Engine) metricsAvailability(ctx context.Context, k Kubernetes) (bool, string) {
	available, err := k.MetricsAPIAvailable(ctx)
	if err != nil || !available {
		return false, metricsAbsentWarning
	}
	return true, ""
}

// DisableAutoscale turns autoscaling off for an app by removing its HorizontalPodAutoscaler
// (ADR-0006). It is guarded by the same app.autoscale guardrail and audited. It is idempotent:
// removing autoscaling from an app that has none succeeds without error.
func (e *Engine) DisableAutoscale(ctx context.Context, app, env string, confirm bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("autoscale off: %w: %w", ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return fmt.Errorf("autoscale off %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return fmt.Errorf("autoscale off %s: loading guardrail policy: %w", app, err)
	}
	args := map[string]string{"env": envName(env), "off": "true"}
	if err := e.recordDecision(ctx, auditOpAutoscale, app, args, GuardrailAutoscale,
		pol.evaluateGuardrail(env, "autoscale", GuardrailAutoscale, confirm, "disabling autoscaling")); err != nil {
		return err
	}
	if err := k.DeleteAutoscaler(ctx, app); err != nil {
		e.recordExecution(ctx, auditOpAutoscale, app, args, err)
		return fmt.Errorf("autoscale off %s: %w", app, err)
	}
	e.recordExecution(ctx, auditOpAutoscale, app, args, nil)
	return nil
}

// Expose makes an app reachable at a hostname through an Ingress (ADR-0018). It is a guarded
// operation: public exposure trips the app.expose_public guardrail, which holds for confirmation
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
	if req.TLS && req.Issuer == "" {
		return ExposeResult{}, fmt.Errorf("expose %s: TLS requires an issuer: %w", req.App, ErrInvalid)
	}

	ns, err := e.resolveMutatingNamespace(ctx, req.Env)
	if err != nil {
		return ExposeResult{}, fmt.Errorf("expose %s: %w", req.App, err)
	}
	k := e.k8s.WithNamespace(ns)
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return ExposeResult{}, fmt.Errorf("expose %s: loading guardrail policy: %w", req.App, err)
	}
	args := map[string]string{"host": req.Host, "port": strconv.Itoa(int(req.Port)), "tls": strconv.FormatBool(req.TLS), "env": envName(req.Env)}
	if err := e.recordDecision(ctx, auditOpExpose, req.App, args, GuardrailExposePublic,
		pol.evaluateGuardrail(req.Env, "expose", GuardrailExposePublic, req.Confirm, fmt.Sprintf("exposing %s at %s", req.App, req.Host))); err != nil {
		return ExposeResult{}, err
	}

	// The app must be deployed: exposing a workload that does not exist would create a
	// Service with no backends.
	if _, err := k.WorkloadStatus(ctx, req.App); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ExposeResult{}, fmt.Errorf("expose %s: no running workload — deploy it first: %w", req.App, err)
		}
		return ExposeResult{}, fmt.Errorf("expose %s: reading workload: %w", req.App, err)
	}

	// The cluster must be set up for public reachability (an ingress controller) and, when TLS is
	// asked for, for certificate issuance (cert-manager and a ClusterIssuer). A missing prerequisite
	// would leave a half-working exposure the agent then has to diagnose with raw kubectl, so detect
	// it up front and return a structured checklist naming each gap and its burrow fix (ADR-0006).
	if err := e.exposePrerequisites(ctx, req); err != nil {
		return ExposeResult{}, fmt.Errorf("expose %s: %w", req.App, err)
	}

	if err := k.Expose(ctx, ExposeSpec{App: req.App, Host: req.Host, Port: req.Port, TLS: req.TLS, Issuer: req.Issuer}); err != nil {
		e.recordExecution(ctx, auditOpExpose, req.App, args, err)
		return ExposeResult{}, fmt.Errorf("expose %s: %w", req.App, err)
	}
	e.recordExecution(ctx, auditOpExpose, req.App, args, nil)
	scheme := "http"
	if req.TLS {
		scheme = "https"
	}
	return ExposeResult{App: req.App, Host: req.Host, Port: req.Port, URL: scheme + "://" + req.Host}, nil
}

// exposePrerequisites checks the cluster is set up for the public, optionally TLS-terminated
// reachability an expose request needs, and returns a MissingPrerequisitesError enumerating every
// missing piece and the burrow command that provisions it (ADR-0006, ADR-0034). It reads capabilities
// through the ClusterProber seam and the providers registry — never a raw cluster call — so it stays
// unit-testable against a fake.
//
// It blocks only on prerequisites whose absence leaves the exposure non-functional: an ingress
// controller (without one no Ingress ever gets an external address) and, when TLS is asked for,
// cert-manager (without it the certificate is never issued). When one of those hard gaps is present it
// also folds in the DNS-provider note so the agent gets the full remediation in one shot; a missing
// DNS provider alone never blocks, because pointing DNS at the ingress address by hand is a valid path
// and the reachability surface guides that. When no prober is wired it returns nil: detection is
// best-effort and never blocks an expose on a build that cannot probe the cluster.
func (e *Engine) exposePrerequisites(ctx context.Context, req ExposeRequest) error {
	if e.prober == nil {
		return nil
	}
	caps, err := e.prober.DetectCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("checking cluster prerequisites: %w", err)
	}

	var missing []Prerequisite
	blocking := false
	if !caps.Ingress.Present {
		blocking = true
		missing = append(missing, Prerequisite{
			Name:   "ingress controller",
			Detail: "public reachability needs an ingress controller to route the host and assign an external address",
			Fix:    "run `burrow cluster ingress install`",
		})
	}
	if req.TLS && !caps.CertManager.Present {
		blocking = true
		missing = append(missing, Prerequisite{
			Name:   "cert-manager",
			Detail: "TLS needs cert-manager and a ClusterIssuer to issue the certificate",
			Fix:    "run `burrow cluster ingress install`",
		})
	}
	if !blocking {
		return nil
	}

	// A hard gap is already blocking; fold in the DNS-provider note when no provider is configured so
	// the agent sees the whole checklist at once. DNS is a control-plane registry fact, not a cluster
	// read (ADR-0023), so it comes from the providers registry rather than the prober.
	providers, err := e.db.Providers(ctx)
	if err != nil {
		return fmt.Errorf("checking DNS provider: %w", err)
	}
	dnsConfigured := false
	for _, p := range providers {
		if p.Serves(CapabilityDNS) {
			dnsConfigured = true
			break
		}
	}
	if !dnsConfigured {
		missing = append(missing, missingDNSProviderPrerequisite(req.Host))
	}

	return &MissingPrerequisitesError{Host: req.Host, TLS: req.TLS, Missing: missing}
}

// Reachability reports, link by link, whether an app is reachable at its hostname (ADR-0018):
// deployed and ready, exposed, given an external address by an ingress controller, and DNS
// pointing the host at that address. It returns a structured chain plus a one-line plain
// summary for a non-expert; it never errors on a missing link — that is the answer.
func (e *Engine) Reachability(ctx context.Context, app, env string) (ReachabilityResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return ReachabilityResult{}, fmt.Errorf("reachability: %w: %w", ErrInvalid, err)
	}
	ns, err := e.resolveNamespace(ctx, env)
	if err != nil {
		return ReachabilityResult{}, fmt.Errorf("reachability %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)
	res := ReachabilityResult{App: app}

	ws, err := k.WorkloadStatus(ctx, app)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			res.BlockedOn = "deployment"
			res.Summary = fmt.Sprintf("%s is not deployed yet — deploy it first.", app)
			return res, nil
		}
		return ReachabilityResult{}, fmt.Errorf("reachability %s: reading workload: %w", app, err)
	}
	res.Deployed = true
	res.Ready = ws.Available

	exp, err := k.ExposureStatus(ctx, app)
	if err != nil {
		return ReachabilityResult{}, fmt.Errorf("reachability %s: reading exposure: %w", app, err)
	}
	res.Exposed = exp.Exposed
	res.Host = exp.Host
	res.Address = exp.Address
	res.TLS = exp.TLS
	res.CertReady = exp.CertReady

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

	// Converged verdict: a pure, point-in-time read of the chain. BlockedOn names the first
	// unready link; Reachable means every link is green and URL is set to the live address.
	res.BlockedOn = reachabilityBlockedOn(res)
	res.Reachable = res.BlockedOn == ""
	if res.Reachable {
		res.URL = "http://" + res.Host
		if res.TLS {
			res.URL = "https://" + res.Host
		}
	}
	res.Summary = reachabilitySummary(res)
	return res, nil
}

// reachabilityBlockedOn returns the first unready link in the reachability chain, or "" when
// every link is green. The order follows the chain controller -> routing -> TLS -> DNS
// (ADR-0018): each link depends on the ones before it, so the first gap is the one to fix.
func reachabilityBlockedOn(r ReachabilityResult) string {
	switch {
	case !r.Deployed:
		return "deployment"
	case !r.Ready:
		return "workload"
	case !r.Exposed:
		return "ingress"
	case r.Address == "":
		return "ingress controller"
	case r.TLS && !r.CertReady:
		return "tls certificate"
	case !r.DNSPointsAtCluster:
		return "dns"
	default:
		return ""
	}
}

// reachabilitySummary turns the chain into a one-line, plain-English verdict naming the
// first unsatisfied link and the next action (ADR-0022's novice altitude).
func reachabilitySummary(r ReachabilityResult) string {
	switch {
	case !r.Ready:
		return fmt.Sprintf("%s is deployed but not ready yet — check `burrow app logs %s`.", r.App, r.App)
	case !r.Exposed:
		return fmt.Sprintf("%s is running but not exposed — run `burrow app publish %s --host <name> --port <n>`.", r.App, r.App)
	case r.Address == "":
		return fmt.Sprintf("%s is exposed at %s but no external address is assigned yet — is an ingress controller installed and running?", r.App, r.Host)
	case r.TLS && !r.CertReady:
		return fmt.Sprintf("%s is exposed at %s with an external address, but its TLS certificate is not ready yet; cert-manager is still issuing it.", r.App, r.Host)
	case !r.DNSPointsAtCluster:
		return fmt.Sprintf("%s is exposed at %s, but DNS for %s doesn't point at the cluster yet — add a DNS record pointing %s at %s.", r.App, r.Host, r.Host, r.Host, r.Address)
	case r.TLS:
		return fmt.Sprintf("%s is reachable at https://%s.", r.App, r.Host)
	default:
		return fmt.Sprintf("%s is reachable at http://%s.", r.App, r.Host)
	}
}

// Unexpose removes an app's exposure (its Service and Ingress). It does not affect the
// workload. Unexposing an app that was never exposed returns ErrNotFound.
func (e *Engine) Unexpose(ctx context.Context, app, env string) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("unexpose: %w: %w", ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return fmt.Errorf("unexpose %s: %w", app, err)
	}
	if err := e.k8s.WithNamespace(ns).Unexpose(ctx, app); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("unexpose %s: not exposed: %w", app, err)
		}
		return fmt.Errorf("unexpose %s: %w", app, err)
	}
	return nil
}

// Guardrails returns the guardrail policy as a list for inspection (ADR-0020). With an empty or
// "default" env it returns the global policy; with a named environment it returns that
// environment's effective policy under the env to global to default fallback, each entry marking
// whether its disposition is env-specific or inherited (ADR-0035 phase 2c). A named environment
// must be registered; an unknown one is a clear ErrNotFound.
func (e *Engine) Guardrails(ctx context.Context, env string) ([]GuardrailInfo, error) {
	if env != "" && env != DefaultEnvironment {
		if _, err := e.db.GetEnvironment(ctx, env); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("guardrails: unknown environment %q: %w", env, ErrNotFound)
			}
			return nil, fmt.Errorf("guardrails: resolving environment %q: %w", env, err)
		}
	}
	p, err := e.db.Policy(ctx)
	if err != nil {
		return nil, fmt.Errorf("guardrails: loading policy: %w", err)
	}
	return p.GuardrailsFor(env), nil
}

// SetGuardrail sets one guardrail's disposition (ADR-0020). It rejects an unknown guardrail
// or an invalid disposition as ErrInvalid. This is the operator's lever — exposed via the
// CLI, never as an MCP tool, so the agent cannot change its own guardrails.
//
// With an empty or "default" env it sets the global disposition for code (today's behavior). With a
// named environment it stores the env-prefixed code (e.g. prod.app.delete) so the environment's
// policy can diverge from the global one (ADR-0035 phase 2c). A named environment must be registered
// (an unknown one is ErrNotFound, catching typos), and only the app-level guardrails are
// env-scopable: a cluster-level guardrail (addon.*, dns.*) gates a cluster-wide operation and can
// only be set globally, so env-scoping one is rejected as ErrInvalid.
func (e *Engine) SetGuardrail(ctx context.Context, env string, code GuardrailCode, d Disposition) error {
	if !KnownGuardrail(code) {
		return fmt.Errorf("set guardrail: unknown guardrail %q: %w", code, ErrInvalid)
	}
	if !d.Valid() {
		return fmt.Errorf("set guardrail: invalid disposition %q (want allow, confirm, or deny): %w", d, ErrInvalid)
	}
	stored := code
	if env != "" && env != DefaultEnvironment {
		if !EnvScopable(code) {
			return fmt.Errorf("set guardrail: %q is a cluster-level guardrail and cannot be scoped to an environment; set it globally without --env: %w", code, ErrInvalid)
		}
		if _, err := e.db.GetEnvironment(ctx, env); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("set guardrail: unknown environment %q: %w", env, ErrNotFound)
			}
			return fmt.Errorf("set guardrail: resolving environment %q: %w", env, err)
		}
		stored = GuardrailCode(env + "." + string(code))
	}
	return e.db.SetGuardrail(ctx, stored, d)
}

// AutoDeploy returns the auto-deploy level configured for app in env (ADR-0052 §2). A missing
// configuration resolves to the built-in default (DefaultAutoDeployLevel, off): auto-deploy is
// opt-in, so an app with no stored level is off and is never polled (ADR-0054). The environment is
// resolved so an unknown name is a clear error, and the level is keyed by the canonical environment
// name. This is a read: the agent may observe it over burrow-agent, but only a human sets it (§6).
func (e *Engine) AutoDeploy(ctx context.Context, app, env string) (AutoDeployLevel, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return "", fmt.Errorf("auto-deploy: %w: %w", ErrInvalid, err)
	}
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return "", fmt.Errorf("auto-deploy %s: %w", app, err)
	}
	level, err := e.db.AutoDeployLevel(ctx, app, envName(env))
	if err != nil {
		return "", fmt.Errorf("auto-deploy %s: %w", app, err)
	}
	return level, nil
}

// SetAutoDeploy sets the auto-deploy level for app in env (ADR-0052 §2, §6). Choosing the level is a
// governance decision, so it is a human operator action exposed only through the `burrow` CLI and
// never to the agent — what deploys unattended stays a human decision (ADR-0038). It rejects an
// invalid level as ErrInvalid and an unknown or ambiguous environment like every other per-app
// mutation, and stores the level under the canonical environment name.
func (e *Engine) SetAutoDeploy(ctx context.Context, app, env string, level AutoDeployLevel) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("set auto-deploy: %w: %w", ErrInvalid, err)
	}
	if !level.Valid() {
		return fmt.Errorf("set auto-deploy %s: invalid level %q (want off, patch, minor, or major): %w", app, level, ErrInvalid)
	}
	if _, err := e.resolveMutatingNamespace(ctx, env); err != nil {
		return fmt.Errorf("set auto-deploy %s: %w", app, err)
	}
	return e.db.SetAutoDeployLevel(ctx, app, envName(env), level)
}

// Rollback restores the app's previously running release by redeploying its reference
// (ADR-0007). It finds the current running release, re-applies the release that one
// superseded, and records the rollback as a new release. It returns ErrNotFound when
// there is nothing to roll back from or to.
//
// A rollback disables auto-deploy in the target environment (ADR-0052 §5): once landed it sets the
// app's level to off with the reason "disabled by rollback", so the pull-based watcher does not fight
// the deliberate downgrade by re-applying the version just backed away from. Re-enabling is a
// deliberate human action (`burrow app auto-deploy <app> <level>`).
func (e *Engine) Rollback(ctx context.Context, app, env string, confirm bool) (RollbackResult, error) {
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: %w", app, err)
	}
	releases, err := e.db.Releases(ctx, app, envName(env))
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

	// Guardrail check only after the rollback is known to be valid, so "nothing to roll back to"
	// reads as ErrNotFound rather than a spurious confirmation prompt (mirrors DeleteApp). The
	// rollback guardrail defaults to allow — a recovery action — but an operator may set it to
	// confirm or deny (ADR-0020).
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: loading guardrail policy: %w", app, err)
	}
	args := map[string]string{"image": target.Image, "to_release": target.ID, "env": envName(env)}
	if err := e.recordDecision(ctx, auditOpRollback, app, args, GuardrailRollback,
		pol.evaluateGuardrail(env, "rollback", GuardrailRollback, confirm,
			fmt.Sprintf("rolling %q back to its previous release %s (image %s)", app, target.ID, target.Image))); err != nil {
		return RollbackResult{}, err
	}

	// Env is app-global current state, not snapshotted per release (ADR-0028): a rollback
	// restores the prior image and command but renders the env the app currently has set, not
	// whatever was in effect when the target was first deployed.
	cfg, err := e.db.AppEnv(ctx, app)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: reading env: %w", app, err)
	}

	// A rollback restores the prior image and command but must not reset the replica count to the
	// target release's: resolve with no explicit request so the running count is preserved (or the
	// HPA left to own it), exactly as a redeploy does.
	replicas, err := e.resolveReplicas(ctx, e.k8s.WithNamespace(ns), app, 0)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: %w", app, err)
	}

	rel := Release{
		ID:          e.ids.NewID(),
		App:         app,
		Environment: envName(env),
		Image:       target.Image,
		Env:         cfg,
		Command:     target.Command,
		MetricsPort: target.MetricsPort,
		Replicas:    replicas,
		Status:      ReleasePending,
		Trigger:     TriggerManual,
		Supersedes:  cur.ID,
		CreatedAt:   e.clock.Now(),
	}
	if err := e.db.SaveRelease(ctx, rel); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: recording release: %w", app, err)
	}

	args["env_keys"] = auditKeys(cfg) // KEY NAMES only — never values (ADR-0027)

	spec := WorkloadSpec{App: app, Kind: WorkloadDeployment, Image: target.Image, Env: cfg, Command: target.Command, MetricsPort: target.MetricsPort, Replicas: replicas, ReleaseID: rel.ID}
	if err := e.k8s.WithNamespace(ns).ApplyWorkload(ctx, spec); err != nil {
		rel.Status = ReleaseFailed
		_ = e.db.SaveRelease(ctx, rel)
		e.recordExecution(ctx, auditOpRollback, app, args, err)
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
	e.recordExecution(ctx, auditOpRollback, app, args, nil)

	// The rollback has landed and is recorded; now disable auto-deploy so the watcher does not
	// re-apply the version just backed away from (ADR-0052 §5). Surfacing a disable failure still
	// matters, so return it wrapped even though the rollback itself succeeded.
	if err := e.db.DisableAutoDeploy(ctx, app, envName(env), reasonDisabledByRollback); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: disabling auto-deploy after rollback: %w", app, err)
	}
	return RollbackResult{Release: rel, RolledBackToReleaseID: target.ID, SupersededReleaseID: cur.ID}, nil
}

// AddEnvironment registers a named environment mapping name to namespace (ADR-0035 phase 2). It
// validates name as a DNS-1123-label-safe lowercase token and rejects the reserved `default`
// (which is implicit), then records it. The namespace and burrowd's Role there are created
// kubeconfig-side by `burrow env add` before this call — burrowd holds only namespaced Roles and
// cannot create namespaces or RBAC itself (least privilege), so the engine only records the
// registry entry. A duplicate name is rejected by the store.
func (e *Engine) AddEnvironment(ctx context.Context, name, namespace string) (Environment, error) {
	if err := validateEnvironmentName(name); err != nil {
		return Environment{}, fmt.Errorf("add environment: %w: %w", ErrInvalid, err)
	}
	if namespace == "" {
		return Environment{}, fmt.Errorf("add environment %s: namespace is empty: %w", name, ErrInvalid)
	}
	if err := e.db.CreateEnvironment(ctx, name, namespace); err != nil {
		return Environment{}, fmt.Errorf("add environment %s: %w", name, err)
	}
	return Environment{Name: name, Namespace: namespace, CreatedAt: e.clock.Now()}, nil
}

// ListEnvironments returns the environments the cluster's burrowd knows about (ADR-0035 phase 2):
// the implicit `default` environment first (the app namespace burrowd runs against, behaving like
// today), followed by the registered environments in name order. The default is synthesized rather
// than stored, so multi-environment is opt-in with no regression.
func (e *Engine) ListEnvironments(ctx context.Context) ([]Environment, error) {
	registered, err := e.db.ListEnvironments(ctx)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	out := make([]Environment, 0, len(registered)+1)
	out = append(out, Environment{Name: DefaultEnvironment, Namespace: e.appNamespace, Default: true})
	out = append(out, registered...)
	return out, nil
}

// resolveMutatingNamespace maps a mutating operation's environment name to its namespace, first
// applying the ADR-0047 forcing function: when the operation names no environment (an empty env) and
// more than one environment is registered — the implicit `default` plus at least one named
// environment — it refuses with a structured AmbiguousEnvironmentError that lists the environments
// and tells the caller to name one, rather than silently defaulting to `default`. With only the
// implicit default registered there is no ambiguity, so it resolves exactly like resolveNamespace and
// the common single-environment self-hoster is unaffected (ADR-0047 §2). The check is on
// registration, not reachability (ADR-0047 §1). Every env-scoped mutating engine method routes its
// namespace through this; read-only methods call resolveNamespace directly and are not guarded
// (ADR-0047 §3).
func (e *Engine) resolveMutatingNamespace(ctx context.Context, env string) (string, error) {
	if env == "" {
		envs, err := e.ListEnvironments(ctx)
		if err != nil {
			return "", err
		}
		if len(envs) > 1 {
			return "", &AmbiguousEnvironmentError{Environments: envs}
		}
	}
	return e.resolveNamespace(ctx, env)
}

// resolveNamespace maps an environment name to the namespace its apps operate in (ADR-0035 phase
// 2b). An empty name or the reserved "default" resolves to the engine's app namespace — the
// implicit default environment, behaving exactly like before environments existed. Any other name
// must be a registered environment; an unregistered name is a clear ErrNotFound. Guardrail policy is
// not consulted here: it stays global until phase 2c, so resolution only routes the namespace.
func (e *Engine) resolveNamespace(ctx context.Context, env string) (string, error) {
	if env == "" || env == DefaultEnvironment {
		return e.appNamespace, nil
	}
	got, err := e.db.GetEnvironment(ctx, env)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("unknown environment %q is not registered; ask the user to create it by running: burrow env add %s: %w", env, env, ErrNotFound)
		}
		return "", fmt.Errorf("resolving environment %q: %w", env, err)
	}
	return got.Namespace, nil
}

// envName canonicalizes an environment name for the audit trail: an empty name reads as the
// reserved "default" environment, any other name passes through. The environment is salient,
// non-secret metadata, so it is recorded in the redacted audit args of a guarded operation
// (ADR-0027). A dedicated audit column can follow when phase 2c makes the environment a
// guardrail-code prefix.
func envName(env string) string {
	if env == "" {
		return DefaultEnvironment
	}
	return env
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
