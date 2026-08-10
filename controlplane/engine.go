// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// authoritative resolves a host at its zone's own nameservers for the publish pre-flight
	// (ADR-0041 §3). Optional: nil falls back to resolver, which answers the same question against
	// a recursive resolver.
	authoritative AuthoritativeResolver
	// probe makes the publish pre-flight's plain-HTTP request to the ACME challenge path
	// (ADR-0041 §3). Optional: nil means the pre-flight rests on its DNS half alone.
	probe HTTPProbe
	// after is the timer seam a publish waits on between polls. Nil applies time.After; a test
	// supplies its own so the waits are driven without real time (ADR-0010).
	after func(d time.Duration) <-chan time.Time
	// objectStore builds an ObjectStore for an S3-compatible endpoint and credential pair
	// (ADR-0063 §1). Optional: nil is allowed, and registering an object-storage provider errors
	// cleanly (ErrNotImplemented) when it is not wired.
	objectStore ObjectStoreFactory
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
	// capacity reads the cluster's scheduling-capacity facts (node allocatable, pod requests) for
	// the headroom surface (issue #275). Optional: a capacity read errors cleanly (ErrNotImplemented)
	// when it is nil.
	capacity CapacityProber
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
	// buildLedger reads back the builds that succeeded and were never deployed, so a build whose
	// caller went away is finished rather than discarded (issue #504). Optional: nil is allowed, and
	// a stranded build is simply not recovered — the behaviour every build had before this existed.
	buildLedger BuildLedger
	// buildRegistry is the in-cluster registry reference host:port that the optional in-cluster
	// build defaults its push target to when the caller supplies none (ADR-0053 §5). Optional: when
	// empty, an in-cluster build requires an explicit target and a missing one errors. A
	// caller-supplied target always overrides it, so external registries stay fully supported.
	buildRegistry string
	// buildPublicRegistry is the PUBLIC registry host the in-cluster build's resulting deploy
	// references, distinct from buildRegistry (the internal push endpoint) — the build pushes to the
	// internal Service in-cluster but the node pulls the public host through the ingress over TLS
	// (ADR-0054 §5). Optional: when empty, the deploy falls back to referencing the internal push
	// endpoint (an in-cluster registry installed without a public ingress).
	buildPublicRegistry string
	// appNamespace is the namespace burrowd deploys apps into (BURROW_NAMESPACE) — the namespace
	// the default environment `prod` maps to (ADR-0067 §3: the environment's NAME and its NAMESPACE
	// are separate values, which is what lets an install predating the change gain `prod` without
	// anything moving). It mirrors the kube Adapter's namespace, and it is what
	// EnsureDefaultEnvironment registers `prod` against.
	appNamespace string
	// tokens mints the secret half of a credential (ADR-0084 §2). Optional: nil means this control
	// plane cannot issue credentials, and an issue errors cleanly (ErrNotImplemented). Everything
	// else on the identity path — authenticating a presented token, revoking one — works without it,
	// because only issuance needs new randomness.
	tokens TokenSource
	// authz answers "may this caller mint a credential for that principal" (ADR-0084 §2). It is
	// never nil: New defaults it to the local implementation, which reads the admin column, so a
	// build that wires nothing still authorizes rather than allowing everything. An SSO or SAML
	// integration replaces the value and no call site moves.
	authz CredentialAuthorizer
	// hookLock serializes the lifecycle hooks of one (app, environment) pair (ADR-0072 §9), so two
	// pushes in quick succession never run two migration Jobs against one database. It is state, but
	// not GLOBAL state: it belongs to this engine and is created in New, so two engines in one test
	// binary do not contend.
	hookLock *hookLock
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
	// AuthoritativeResolver resolves a host at its zone's own nameservers for the publish
	// pre-flight (ADR-0041 §3). Optional — nil is allowed, and the pre-flight falls back to
	// Resolver, verifying the same thing against a recursive resolver's answer.
	AuthoritativeResolver AuthoritativeResolver
	// HTTPProbe makes the publish pre-flight's plain-HTTP request to the ACME challenge path
	// (ADR-0041 §3). Optional — nil is allowed, and the pre-flight rests on its DNS half alone
	// rather than blocking every publish on a seam a build did not wire.
	HTTPProbe HTTPProbe
	// After is the timer seam a publish waits on between polls. Optional — nil applies time.After;
	// a test supplies its own so a publish's waits run without real time (ADR-0010).
	After func(d time.Duration) <-chan time.Time
	// ObjectStore builds an ObjectStore for an S3-compatible endpoint and credential pair, so a
	// backup can be written outside the cluster it came from (ADR-0063). Optional — nil is allowed,
	// and registering an object-storage provider errors cleanly (ErrNotImplemented) when it is not
	// wired, exactly as the build path does without a Builder.
	ObjectStore ObjectStoreFactory
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
	// CapacityProber reads the cluster's scheduling-capacity facts (node allocatable, pod requests)
	// for the headroom surface (issue #275). Optional — nil is allowed, and the engine errors cleanly
	// (ErrNotImplemented) on a capacity read when it is not wired.
	CapacityProber CapacityProber
	// RegistryClient lists an image repository's tags for the auto-deploy read/watch (ADR-0052).
	// Optional — nil is allowed, and the auto-deploy show degrades to reporting the level alone
	// when it is not wired. It is OUTBOUND-only and never used on the core deploy path (ADR-0040).
	RegistryClient RegistryClient
	// Builder builds an image from a git source reference inside the cluster for the optional
	// in-cluster build path (ADR-0053). Optional — nil is allowed, and the engine errors cleanly
	// (ErrNotImplemented) on a build when it is not wired.
	Builder Builder
	// BuildLedger reads back successful builds whose deploy never ran, so the build reconciler can
	// finish them (issue #504). Optional — nil is allowed and the reconciler does nothing. In
	// production it is the same *kube.BuildAdapter wired as Builder, which records what each build
	// was for on the build Job itself.
	BuildLedger BuildLedger
	// BuildRegistry is the in-cluster registry reference host:port the in-cluster build defaults its
	// push target to when the caller supplies none — the zero-config default push target for a build
	// (ADR-0053 §5). Optional — an empty value means a build with no explicit target errors, and a
	// caller-supplied target always overrides it (external registries stay fully supported). burrowd
	// sets it from BURROW_BUILD_REGISTRY, which `burrow cluster registry install` wires to the
	// in-cluster registry it deploys.
	BuildRegistry string
	// BuildPublicRegistry is the PUBLIC registry host the in-cluster build's resulting deploy
	// references so the node pulls through the ingress over TLS, distinct from BuildRegistry (the
	// internal push endpoint) — ADR-0054 §5. Optional — an empty value falls back to referencing the
	// internal push endpoint. burrowd sets it from BURROW_BUILD_PUBLIC_REGISTRY, which
	// `burrow cluster registry install --host` wires to the registry's public ingress hostname.
	BuildPublicRegistry string
	// TokenSource mints the secret half of a credential (ADR-0084 §2). Optional — nil is allowed,
	// and issuing a credential errors cleanly (ErrNotImplemented) rather than inventing randomness
	// the engine is not supposed to read. Authenticating and revoking work without it.
	TokenSource TokenSource
	// CredentialAuthorizer answers who may mint a credential for whom (ADR-0084 §2). Optional — nil
	// defaults to the local implementation over the principals table (NewDatabaseAuthorizer), which
	// is what an SSO or SAML integration later replaces. It never defaults to allowing everything:
	// the point of the seam is that the answer exists in exactly one place from the first day.
	CredentialAuthorizer CredentialAuthorizer
	// AppNamespace is the namespace burrowd deploys apps into (BURROW_NAMESPACE) — the namespace the
	// default environment `prod` maps to (ADR-0067 §3). Optional — an empty value defaults to the
	// Kubernetes namespace "default", matching the kube Adapter. That is a NAMESPACE name and has
	// nothing to do with the retired environment name of the same spelling.
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
	authz := d.CredentialAuthorizer
	if authz == nil {
		authz = NewDatabaseAuthorizer(d.Database)
	}
	return &Engine{
		k8s:                 d.Kubernetes,
		db:                  d.Database,
		clock:               d.Clock,
		ids:                 d.IDs,
		resolver:            d.Resolver,
		credentials:         d.Credentials,
		dns:                 d.DNS,
		authoritative:       d.AuthoritativeResolver,
		probe:               d.HTTPProbe,
		after:               d.After,
		objectStore:         d.ObjectStore,
		logs:                d.Logs,
		metrics:             d.Metrics,
		dbProvisioner:       d.DatabaseProvisioner,
		prober:              d.ClusterProber,
		capacity:            d.CapacityProber,
		registry:            d.RegistryClient,
		builder:             d.Builder,
		buildLedger:         d.BuildLedger,
		buildRegistry:       d.BuildRegistry,
		buildPublicRegistry: d.BuildPublicRegistry,
		appNamespace:        appNamespace,
		tokens:              d.TokenSource,
		authz:               authz,
		hookLock:            newHookLock(),
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
	// recovered marks a deploy the control plane drove on behalf of a caller who had already gone —
	// finishing a build that succeeded after its client disconnected (issue #504). The TRIGGER stays
	// manual, because the operation was manually triggered and only its completion was not, and
	// because everything a manual deploy does (the downgrade safety stop above all) is what an
	// attended run of the same build would have done. What it changes is the audit trail: a row that
	// says a human was present when they were not is a row that misleads a reviewer.
	recovered bool
}

// manualProvenance is the provenance of an explicit CLI or agent deploy — the default for every
// deploy today (ADR-0052 §5).
func manualProvenance() deployProvenance { return deployProvenance{trigger: TriggerManual} }

// recoveredProvenance is the provenance of a deploy the control plane finished on behalf of a caller
// who had already gone — a build that succeeded after its client disconnected (issue #504). It is a
// MANUAL deploy that nobody was present for, not an unattended update: the operation was triggered
// by a person or an agent, and every manual-deploy consequence (the downgrade safety stop) is what
// an attended run of the same build would have had. Only the audit trail tells them apart.
func recoveredProvenance() deployProvenance {
	return deployProvenance{trigger: TriggerManual, recovered: true}
}

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
	// Normalise the caller's reporter into a no-op ONCE, here, so no emit site below carries a nil
	// check (issue #480). Nothing is emitted before recordDecision returns: everything above it —
	// validation, the environment, the replica ceiling, and above all the guardrail decision — is a
	// refusal the caller must receive status-coded, and a stage event is what commits a transport to
	// having succeeded.
	progress := deployProgress(noProgress)
	if req.Progress != nil {
		progress = req.Progress
	}
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
	// The replica ceiling is an operational limit, so it is checked here, ahead of the guardrails
	// and of the audit row they write: exceeding it is a validation failure, not a decision anyone
	// held or denied (ADR-0068 §2).
	if err := e.checkReplicaCeiling(ctx, req.Env, "deploy", fmt.Sprintf("%d replicas", replicas), replicas); err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: %w", req.App, err)
	}
	args := map[string]string{"image": req.Image, "replicas": strconv.Itoa(int(replicas)), "env": envName(req.Env), "trigger": string(prov.trigger)}
	// An auto deploy records the level that applied and the tag the watcher took, so the audit trail
	// distinguishes an unattended update from an explicit one (ADR-0052 §5).
	if prov.trigger == TriggerAuto {
		args["auto_level"] = string(prov.level)
		args["auto_tag"] = prov.tag
	}
	// A deploy the control plane finished for a caller who had already gone says so, so a reviewer
	// reading the trail is never told a human was present when nobody was (issue #504).
	if prov.recovered {
		args["recovered"] = "build"
	}
	if err := e.recordDecision(ctx, auditOpDeploy, req.App, args, GuardrailAppDeploy,
		pol.evaluateDeploy(ctx, GuardrailScope{Env: req.Env, Name: req.App}, replicas, req.Confirm)); err != nil {
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

	// The readiness probe is resolved from what the user declared and what Burrow knows about the
	// app's published port (ADR-0076 §3). An app that declared nothing and is not published resolves
	// to no probe, which is exactly the behaviour it had before probes existed. It is read here,
	// before the release record is written, so a store failure fails the deploy cleanly rather than
	// leaving a Pending release behind.
	readiness, ep, _, err := e.resolveHealth(ctx, req.App, envName(req.Env))
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: %w", req.App, err)
	}
	declared := ep.Declared()

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

	// The pre-deploy hook runs here, from the image BEING DEPLOYED, before anything reaches the
	// cluster (ADR-0072 §2). This is the shared rollout path, so it fires on EVERY deploy path — an
	// explicit deploy, a build that ends in one, and an unattended auto-deploy alike: a hook that
	// fires only sometimes is worse than one that always fires, because the point is that schema and
	// code move together. A rollback does not come through here and fires `pre-rollback` instead (§8).
	//
	// Its failure ABORTS the deploy (§3): the new image does not roll out, the running version keeps
	// serving on the old schema, and the failure is reported as the deploy's failure with the
	// command's output. The release is recorded failed so the history shows the attempt; a failed
	// release is not a rollback target, so the rollback handle is unchanged.
	if err := e.runHook(ctx, k, HookPreDeploy, req.App, req.Env, req.Image, env, nil, progress); err != nil {
		rel.Status = ReleaseFailed
		_ = e.db.SaveRelease(ctx, rel) // best effort: record the failure
		e.recordExecution(ctx, auditOpDeploy, req.App, args, auditableHookError(err))
		return DeployResult{}, fmt.Errorf("deploy %s: %w", req.App, err)
	}

	// The secret projection is read here for the same reason the env and the probe above it are: it is
	// current app configuration rather than a property of this release (ADR-0089 §5), so a deploy
	// renders whatever the app has mounted now.
	mounts, secretEnv, err := e.secretProjectionFor(ctx, k, req.App, envName(req.Env))
	if err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: %w", req.App, err)
	}

	spec := WorkloadSpec{App: req.App, Kind: WorkloadDeployment, Image: req.Image, Env: env, Command: req.Command, MetricsPort: req.MetricsPort, Readiness: readiness, Replicas: replicas, SecretFiles: mounts, SecretEnvKeys: secretEnv, ReleaseID: rel.ID}
	progress.started(StageApply)
	if err := k.ApplyWorkload(ctx, spec); err != nil {
		progress.failed(StageApply)
		rel.Status = ReleaseFailed
		_ = e.db.SaveRelease(ctx, rel) // best effort: record the failure
		e.recordExecution(ctx, auditOpDeploy, req.App, args, err)
		return DeployResult{}, fmt.Errorf("deploy %s: applying to cluster: %w", req.App, err)
	}
	progress.done(StageApply)

	// THE WRITE WAS ACCEPTED. That is not the deploy's outcome, and reporting it as one is what
	// issue #546 was filed about: a rollout whose new pod never passed its readiness probe was
	// reported as `deployed`, with the release that was still serving every request reported as
	// `superseded`. Kubernetes had behaved correctly and kept the old ReplicaSet up; only the report
	// was wrong, and it is the report an agent has instead of a pod list.
	//
	// So the deploy waits here, before it says anything (ADR-0092 §1). The observation is the one
	// settleOnce already made for the `post-deploy` hook and the dependency check — the same
	// sync.OnceValue handed to both further down, so an app with either still settles exactly once
	// (issue #407) and the three cannot report differently on one rollout. What changed is that
	// nothing has to ASK any more: the deploy itself is a consumer, so every deploy waits.
	//
	// A caller may decline the wait with NoWait, and then the report says the outcome is unknown
	// rather than saying it went well.
	settle := e.settleOnce(ctx, k, req.App, envName(req.Env), progress)
	var rollout *RolloutReport
	if !req.NoWait {
		rollout = e.rolloutReport(ctx, k, req.App, settle())
	}

	// The cluster is updated. From here a SaveRelease failure leaves the record behind
	// the cluster (the release stays Pending though the new image is live) — a drift
	// the reconcile loop closes in a later phase. v0.1 surfaces the error honestly.
	//
	// THE STATUS STILL SAYS `deployed` WHEN THE ROLLOUT DID NOT SETTLE, and the rollout columns
	// beside it say what actually happened (ADR-0092 §4). Status is the registry's record of which
	// release Burrow applied and which one a rollback returns to: `burrow app rollback` takes the
	// newest `deployed` release and re-applies what THAT superseded, so demoting a wedged release to
	// `failed` would silently move the rollback handle one release further back — landing an image
	// nobody asked for at the moment somebody is recovering from a bad deploy.
	rel.Status = ReleaseDeployed
	rel.Rollout, rel.RolloutReason = recordedRollout(rollout)
	if err := e.db.SaveRelease(ctx, rel); err != nil {
		return DeployResult{}, fmt.Errorf("deploy %s: recording successful release: %w", req.App, err)
	}
	// The audit row records the rollout too, so the trail a reviewer reads afterwards says whether
	// the image this row is about ever served (ADR-0027). args is the same map the decision row was
	// written from, extended before the execution row the way env_keys already is.
	args["rollout"] = string(rel.Rollout)
	if rel.RolloutReason != "" {
		args["rollout_reason"] = rel.RolloutReason
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
	res := DeployResult{Release: rel, SupersededReleaseID: superseded, Rollout: rollout}
	// Nudge toward semver when the deployed tag cannot be classified for auto-update (ADR-0052 §8).
	// This is a non-blocking hint on an otherwise-successful deploy, not a gate: the deploy has
	// already landed. An auto deploy always carries a semver tag, so only a manual non-semver deploy
	// ever trips it.
	if stableSemver(imageTag(req.Image)) == "" {
		res.Hints = append(res.Hints, nonSemverDeployHint)
	}
	// ADR-0076 §5, stated where the agent meets it. An app with no declared health endpoint is one
	// whose broken deploys look successful, and the agent — which has the user's source — is the
	// only party that can fix that. The hint says so, says adding one is a few lines, and says the
	// endpoint must check the app's OWN readiness and never its dependencies, because that last part
	// is what an agent copying the internet's most common example gets wrong (§2).
	if !declared {
		res.Hints = append(res.Hints, NoHealthEndpointHint)
	}
	// The deploy-time dependency check (ADR-0076 §4), then the post-deploy hook (ADR-0072 §4). Both
	// run LAST, after the release is recorded and the deploy is in every sense done, and neither can
	// fail it: the image is live and the record is written, so there is nothing left to abort.
	//
	// The CHECK goes first so the hook, which is the party that acts on the outcome, runs after
	// everything Burrow has to say about this deploy is known. What it checks is DERIVED from what
	// Burrow provisioned for this app — the database it attached, the port it published — so there is
	// nothing to configure and nothing that can drift, and an app it provisioned nothing for runs no
	// check pod at all. runDependencyChecks returns no error, deliberately: a check that failed, or a
	// check pod that could not be scheduled, must not turn a live deploy into a reported failure
	// (ADR-0076 §6). It waits for the rollout to settle first, which is what ADR-0072 §4's
	// `post-deploy` phase means by when it fires.
	//
	// BOTH TAKE THE SAME settle THE DEPLOY'S OWN REPORT TOOK. It is one observation of this rollout,
	// made by whichever party asks first and handed unchanged to the rest, so the deploy waits out
	// the settle bound at most once however many things want to know how it went (issue #407) — and
	// the report, the check, and the hook cannot say different things about one rollout. Since
	// ADR-0092 the deploy itself asks, so on the ordinary path the observation is already made and
	// these two read it; a deploy that declined the wait (NoWait) still settles here if it has a
	// dependency to check or a hook to tell, because those are defined in terms of the outcome.
	res.Dependencies = e.runDependencyChecks(ctx, k, req.App, envName(req.Env), ns, req.Image, env, settle, progress)
	for _, d := range res.Dependencies {
		if d.Failed() {
			res.Hints = append(res.Hints, DependencyFailureHint)
			break
		}
	}
	// The post-deploy hook tells the hook how the rollout went — succeeded or failed, and on failure
	// the reason from ADR-0074 §2's closed vocabulary — from the settle above. With no hook set it
	// does nothing at all and waits for nothing, so a deploy nobody asked to be told about is
	// unchanged. Whatever it and the rollout report comes back as hints (ADR-0072 §6 — Burrow
	// reports, the hook decides).
	res.Hints = append(res.Hints, e.runPostDeployHook(ctx, k, req.App, req.Env, req.Image, rel.ID, DeployKindDeploy, env, settle, progress)...)
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
// Deployment (ADR-0028).
func (e *Engine) reapplyEnv(ctx context.Context, k Kubernetes, app, env string) error {
	_, err := e.reapplyWorkload(ctx, k, "set env", app, env)
	return err
}

// reapplyWorkload re-renders the running workload from the app's currently running release and the
// current store state, so an out-of-band change (a config var, a declared health endpoint, a new
// exposure) rolls the Deployment and reaches the running pods. op names the calling operation for
// the error messages. It reports whether a workload was actually re-applied: with no running release
// there is nothing to roll, so the change is persisted and lands on the next deploy — a no-op, not
// an error, and the false return is what lets a caller say so on its surface.
func (e *Engine) reapplyWorkload(ctx context.Context, k Kubernetes, op, app, env string) (bool, error) {
	releases, err := e.db.Releases(ctx, app, env)
	if err != nil {
		return false, fmt.Errorf("%s %s: reading release history: %w", op, app, err)
	}
	cur, ok := lastDeployed(releases)
	if !ok {
		return false, nil // no running workload yet; the change lands on the next deploy
	}
	cfg, err := e.db.AppEnv(ctx, app)
	if err != nil {
		return false, fmt.Errorf("%s %s: reading env: %w", op, app, err)
	}
	// A reapply re-renders the running workload; it must not rescale it. Resolve with no explicit
	// request so the current count is preserved (or the HPA left to own it).
	replicas, err := e.resolveReplicas(ctx, k, app, 0)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", op, app, err)
	}
	// The readiness probe is resolved on EVERY apply, not snapshotted onto the release, so a
	// declared endpoint or a new exposure reaches the workload through this path as well as through
	// a deploy (ADR-0076 §3).
	readiness, err := e.readinessFor(ctx, app, env)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", op, app, err)
	}
	// Like the env and the probe, the secret projection is read on every apply rather than snapshotted
	// (ADR-0089 §5), so this is also the path a mount, an unmount, or a `secret set` on an app that
	// enumerates its secret environment reaches the running pods through.
	mounts, secretEnv, err := e.secretProjectionFor(ctx, k, app, env)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", op, app, err)
	}
	spec := WorkloadSpec{App: app, Kind: WorkloadDeployment, Image: cur.Image, Env: cfg, Command: cur.Command, MetricsPort: cur.MetricsPort, Readiness: readiness, Replicas: replicas, SecretFiles: mounts, SecretEnvKeys: secretEnv, ReleaseID: cur.ID}
	if err := k.ApplyWorkload(ctx, spec); err != nil {
		return false, fmt.Errorf("%s %s: applying to cluster: %w", op, app, err)
	}
	return true, nil
}

// ListSecrets returns the env-var KEYS in an app's per-app Secret, sorted, never the values
// (ADR-0028/0004). Secret values live only in the Kubernetes Secret and never cross the API or the
// agent control channel, so this read returns keys only. An app with no secrets yields an empty slice.
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
// logged, never audited, never stored in Postgres, and never carried over the agent control channel
// — only its KEY name appears in any error (the value is never formatted into one). Setting a value
// stays a human operation: `burrow-agent` has no secret-set command, so the agent cannot supply a
// value. An app with no running workload just writes the Secret; the change lands on the next deploy.
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
	return e.rollForSecretChange(ctx, k, "set secret", app, env)
}

// rollForSecretChange makes a running workload see a change to its per-app Secret, and it is THE ONE
// PLACE that knows how (ADR-0089 §4). Every path that writes or removes a key out of band goes
// through it — `secret set`, `secret unset`, an add-on attach or detach, a restore cutover.
//
// TWO WAYS, AND WHICH IS CORRECT IS A PROPERTY OF THE APP RATHER THAN OF THE CALLER. An app that
// sources its Secret wholesale through envFrom needs only a RESTART: envFrom is read at pod start and
// picks up whatever the Secret holds by then, so even a key that did not exist when the pod template
// was written arrives. An app with a file-only key ENUMERATES its secret environment, and an
// enumerated template names each key — so a new key is not in it, a restart rolls the pod without the
// value that was just written, and only a REAPPLY carries it.
//
// It is a function rather than a rule to remember because the callers are not all `secret set`, and
// the failure is silent for the ones that are not: an `addon attach` writes DATABASE_URL straight
// through the Kubernetes seam, and the app would come back with the attach reported successful, the
// connection string in the Secret, and no DATABASE_URL in its environment.
//
// A missing workload is not an error on either path: nothing is running yet, and the change lands on
// the next deploy.
func (e *Engine) rollForSecretChange(ctx context.Context, k Kubernetes, op, app, env string) error {
	mounts, err := e.secretMountsFor(ctx, app, envName(env))
	if err != nil {
		return fmt.Errorf("%s %s: %w", op, app, err)
	}
	if mounts.AnyFileOnly() {
		_, err := e.reapplyWorkload(ctx, k, op, app, envName(env))
		return err
	}
	if err := k.RestartWorkload(ctx, app, e.clock.Now()); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%s %s: rolling workload: %w", op, app, err)
	}
	return nil
}

// UnsetSecret removes one key from an app's per-app Secret and, unless noRestart, rolls the
// running workload so it drops the value (ADR-0028). Removing a key carries no value, so it is on
// the agent surface (`burrow-agent secret unset`). An app with no running workload just updates the
// Secret; the change lands on the next deploy. Removing an absent key succeeds.
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
	return e.rollForSecretChange(ctx, k, "unset secret", app, env)
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

// InstallAddon deploys the vetted backing service for the named add-on type INTO ONE ENVIRONMENT and
// registers it as a queryable capability (ADR-0025/0026). It is guarded by addon.install.
//
// Each environment gets its own instance (ADR-0067 §1), and one per environment is the DEFAULT
// rather than the maximum (ADR-0091 §1): with no opts.Name the install lands on the environment's own
// instance under exactly the names an existing install already has — nothing migrates — and with one
// it stands a SECOND instance up beside it, under a generated cluster name the operator never types.
// The environment is resolved the same way a deploy's is, so an install that names none while several
// environments are registered is refused rather than landing on whichever one happens to be first
// (ADR-0047 §1).
//
// INSTALLING A LABEL THAT ALREADY EXISTS IS A RE-INSTALL OF THAT INSTANCE, not a second one. An
// instance's identity is its label, `addon install` has always been idempotent, and the alternative —
// a second `Cluster` for a label somebody typed twice — is a pod and a volume nobody asked for.
func (e *Engine) InstallAddon(ctx context.Context, t AddonType, env string, opts InstallAddonOptions) (AddonInfo, error) {
	spec, ok := LookupAddon(t)
	if !ok {
		return AddonInfo{}, fmt.Errorf("install addon: unknown type %q: %w", t, ErrInvalid)
	}
	targetEnv, _, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return AddonInfo{}, fmt.Errorf("install addon %s: %w", t, err)
	}
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return AddonInfo{}, fmt.Errorf("install addon %s: loading guardrail policy: %w", t, err)
	}
	// The INSTANCE this install would act on, settled before the guardrail because its LABEL is what
	// scopes the disposition (ADR-0085 §1, ADR-0091 §4): an operator can deny standing up one
	// particular instance without denying every add-on on the cluster.
	label, instance, err := e.installTarget(ctx, t, targetEnv, opts.Name)
	if err != nil {
		return AddonInfo{}, fmt.Errorf("install addon %s: %w", t, err)
	}
	args := map[string]string{"type": string(t), "image": spec.Image, "env": targetEnv, "instance": label}
	if err := e.recordDecision(ctx, auditOpAddonInstall, string(t), args, GuardrailAddonInstall,
		// addon.* is not EnvScopable (ADR-0035 phase 2c), so an environment on its own cannot relax
		// or tighten it — the instance carries the environment instead. The environment still
		// appears in the message and the audit args, because which environment an add-on operation
		// lands in is exactly what the operator is being asked to approve (ADR-0067 §1).
		//
		// The scope name is the LABEL, never the generated cluster name: a key nobody can read is a
		// key nobody will write, and because an environment's first instance is labelled with its own
		// name, every disposition already written keeps matching (ADR-0091 §4).
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: targetEnv, Name: label}, "addon install", GuardrailAddonInstall, opts.Confirm,
			installConsequence(t, spec.Image, targetEnv, label))); err != nil {
		return AddonInfo{}, err
	}
	// Resolved AFTER the guardrail and before the first object is written. It is the pgBackRest
	// repository the instance will archive to (ADR-0066 §3), and a nil one is the ordinary state of a
	// cluster with no object storage: the instance is then exactly the `Cluster` it was before this
	// existed, with no plugin and no archiving, and physical backups of it are refused by name.
	//
	// Only Postgres has one. Asking for a destination on behalf of the metrics add-on would read the
	// provider registry, and possibly refuse for ambiguity, on an operation that has no repository.
	var archive *ArchiveDestination
	if t == AddonPostgres {
		archive, err = e.resolveArchiveDestination(ctx, opts.ArchiveDestination, targetEnv)
		if err != nil {
			e.recordExecution(ctx, auditOpAddonInstall, string(t), args, err)
			return AddonInfo{}, fmt.Errorf("install addon %s: %w", t, err)
		}
		if archive != nil {
			// The provider NAME on the audit row, never a credential or an endpoint secret.
			args["archive"] = archive.Provider
		}
	}
	info, err := e.k8s.DeployAddon(ctx, spec, targetEnv, instance, archive)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonInstall, string(t), args, err)
		return AddonInfo{}, fmt.Errorf("install addon %s: %w", t, err)
	}
	// Record the add-on in the registry — the DB is the source of truth for what add-ons exist
	// (ADR-0025), like the provider registry. Readiness is never stored; it is probed live.
	//
	// THE ROW IS THE MAPPING between the label and the cluster name, and it is the only one
	// (ADR-0091 §2). Written here, after the objects exist, so a registry that says an instance is
	// called something is a registry describing something that is there.
	info.Label = label
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
// never carried over the agent control channel — only the key is recorded in the registry
// (ADR-0004/0023). A token without a secretKey is invalid. The registry entry that crosses the API
// holds only the key.
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

// RemoveAddon removes the named add-on instance. It tears the add-on's WORKLOAD down and, by
// default, LEAVES ITS DATA VOLUME IN PLACE: for Postgres that volume holds every attached app's
// database (ADR-0031), so a removal meant as "stop this and reinstall it cleanly" must not destroy
// it. Passing DeleteData is the explicit, separate ask that destroys the volume.
//
// It is guarded by addon.remove (ADR-0025), held for confirmation by default. The guardrail's
// message carries the CONCRETE consequence — which volume goes or stays, which apps are affected by
// name, and whether a final backup will be taken first — because a confirmation prompt that does not
// say what is destroyed is not informed consent (ADR-0006: the gate hands back a reason the human
// and the agent can act on). A hard refusal is deliberately NOT used for the attached-apps case:
// this system expresses "never" as a policy disposition (`guard set addon.remove deny`, ADR-0020),
// not as a bespoke override flag, and the destructive path already requires the caller to have typed
// DeleteData.
//
// THE MECHANISM DOES NOT CHANGE THE CONTRACT. A Postgres instance is a CloudNativePG `Cluster`
// (ADR-0066 §1) and keeps its data on removal exactly as a Deployment-backed add-on does; the
// difference is entirely below this line, because the operator owns those claims and keeping them
// means disowning them before its `Cluster` is deleted rather than simply not deleting them. What
// that costs here is a single argument — the add-on's TYPE is read from the registry row and handed
// to the seam, because a removal that inferred the shape from the cluster would read a refused probe
// as an absent add-on.
//
// WHERE AN OBJECT STORE IS REGISTERED, DeleteData TAKES A FINAL BACKUP FIRST AND ABORTS IF IT FAILS
// (ADR-0064 §5). The ordering is the whole of the safety: nothing is destroyed until a copy is known
// to exist off the cluster. planFinalBackup decides that before the guardrail is evaluated, so the
// human confirming the removal is told whether a copy will be made; finalBackupBeforeDataDeletion
// takes it, and any failure returns before the first destructive call.
func (e *Engine) RemoveAddon(ctx context.Context, name string, opts RemoveAddonOptions) (RemoveAddonResult, error) {
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return RemoveAddonResult{}, fmt.Errorf("remove addon %s: loading guardrail policy: %w", name, err)
	}
	// The registry is the source of truth for what add-ons exist (ADR-0025): load it BEFORE the
	// guardrail so an unknown name is a plain ErrNotFound rather than a held confirmation for an
	// add-on that does not exist, and so the confirmation message can describe what is actually
	// there — its type, its volume, and who is attached to it.
	info, err := e.removalTarget(ctx, name, opts.Environment)
	if err != nil {
		return RemoveAddonResult{}, fmt.Errorf("remove addon %s: %w", name, err)
	}
	// From here the removal acts on the instance the registry named, never on the caller's string:
	// a label and a cluster name are the same value only for an environment's first instance.
	name = info.Name
	apps, appsKnown := e.attachedApps(ctx, info)

	// Planned before the guardrail so the held confirmation says whether a copy will be made, and
	// NOT best-effort: a registry read that fails must not be read as "no object store is
	// registered", because that reading destroys the volume while reporting that nothing was
	// available to back it up to.
	plan, err := e.planFinalBackup(ctx, info, opts)
	if err != nil {
		return RemoveAddonResult{}, fmt.Errorf("remove addon %s: %w", name, err)
	}

	// The audit row records the destructive INTENT alongside the operation, so the log distinguishes
	// "stopped the add-on" from "destroyed its data", names who was attached, and says whether the
	// data was destroyed without a final backup (ADR-0027). App names are not secrets; no credential
	// or connection string goes near this.
	args := map[string]string{"type": string(info.Type), "delete_data": strconv.FormatBool(opts.DeleteData)}
	if opts.DeleteData {
		args["skip_final_backup"] = strconv.FormatBool(opts.SkipFinalBackup)
	}
	if len(apps) > 0 {
		args["attached_apps"] = strings.Join(apps, ",")
	}
	if err := e.recordDecision(ctx, auditOpAddonRemove, name, args, GuardrailAddonRemove,
		// Scoped by the instance's LABEL, not by its cluster name (ADR-0091 §4). For every instance
		// that existed before that record the two are the same string, so a disposition already
		// written keeps matching; for a later one the label is the only readable half, and a key
		// nobody can read is a key nobody will write.
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: info.Environment, Name: instanceLabel(info)}, "addon remove", GuardrailAddonRemove, opts.Confirm,
			removalConsequence(info, opts.DeleteData, apps, plan))); err != nil {
		return RemoveAddonResult{}, err
	}

	res := RemoveAddonResult{Name: name, Type: info.Type, Instance: instanceLabel(info), AttachedApps: apps}
	// Everything below this point destroys something, so the final backup goes ABOVE it. A failure
	// returns here with the add-on, its claim and its registry row all untouched — which is also what
	// makes a retry safe: there is no partial state to resume from (ADR-0064 §5).
	if opts.DeleteData {
		backups, ferr := e.finalBackupBeforeDataDeletion(ctx, info, apps, appsKnown, plan)
		if ferr != nil {
			e.recordExecution(ctx, auditOpAddonRemove, name, args, ferr)
			return RemoveAddonResult{}, fmt.Errorf("remove addon %s: %w", name, ferr)
		}
		res.FinalBackups = backups
		res.FinalBackupSkipped, res.FinalBackupNote = plan.skipped, plan.note
	}
	if info.Mode == "installed" {
		// The TYPE comes from the registry row, and it is what decides the shape of the teardown
		// (ADR-0066 §1). Removal is the one operation that must not work that out from the cluster: a
		// Postgres instance has no Deployment to find, and a probe that finds nothing is
		// indistinguishable from a probe that was refused.
		removal, derr := e.k8s.DeleteAddon(ctx, name, info.Type, opts.DeleteData)
		if derr != nil {
			e.recordExecution(ctx, auditOpAddonRemove, name, args, derr)
			return RemoveAddonResult{}, fmt.Errorf("remove addon %s: %w", name, derr)
		}
		res.AddonRemoval = removal
	}
	if err := e.db.DeleteAddon(ctx, name); err != nil {
		e.recordExecution(ctx, auditOpAddonRemove, name, args, err)
		return RemoveAddonResult{}, fmt.Errorf("remove addon %s: %w", name, err)
	}
	e.recordExecution(ctx, auditOpAddonRemove, name, args, nil)
	return res, nil
}

// attachedApps enumerates the apps holding a Burrow-provisioned database on an installed Postgres
// add-on (ADR-0031) — the set whose data a data-deleting removal destroys, and whose DATABASE_URL a
// data-keeping removal leaves pointing at a stopped instance. It asks the instance being removed,
// which is the one serving info.Environment (ADR-0067 §1), so removing staging's Postgres never
// names production's attached apps. Only the Postgres add-on has per-app attachments; every other
// type returns none.
//
// It is BEST-EFFORT by contract: an unwired provisioner, a provisioner that does not implement
// AppDatabaseLister, or an instance that will not answer all yield no apps rather than an error. An
// add-on is often removed precisely because it is broken, and being unable to ask it who is attached
// must never make it unremovable. The message the caller sees then falls back to the generic
// consequence, which is still concrete about the volume.
//
// The second return says WHICH KIND OF EMPTY the first one is, and it exists because collapsing the
// two is a data-loss bug on the final-backup path (ADR-0064 §5). "No app is attached" means there is
// nothing to back up and the volume is safe to destroy; "the instance would not answer" means we do
// not know what is in there. A caller that reads the second as the first destroys every database on
// a wedged instance and reports that the backup succeeded, which is precisely the false assurance
// §5 exists to prevent. It is true (known) for an add-on type that has no per-app attachments at
// all, because that is a fact about the type rather than an unanswered question.
func (e *Engine) attachedApps(ctx context.Context, info AddonInfo) (apps []string, known bool) {
	if info.Type != AddonPostgres || info.Mode != "installed" {
		return nil, true
	}
	// A registry row written before add-ons were per-environment carries no environment; it is the
	// default environment's instance by construction, since that is the only one that could exist.
	// The INSTANCE comes off the row rather than being derived from the environment, because an
	// environment may hold more than one and the question is only ever about this one (ADR-0091 §4).
	return e.appDatabases(ctx, envName(info.Environment), info.Name)
}

// appDatabases asks ONE Postgres instance in environment env which Burrow-provisioned app databases
// it holds, sorted, and says whether the question could be put at all. The instance is named rather
// than derived from the environment: an environment may hold several, and "who is attached?" is only
// ever true of one of them (ADR-0091 §4).
//
// It is factored out of attachedApps because a physical restore asks the same instance the same
// question for the opposite purpose — not "whose data does this destroy?" but "did the data actually
// come back?" (verifyRecoveredInstance) — and the two must not drift into two ideas of what a
// provisioned database is.
//
// known=false means the question was never answered: no provisioner implements AppDatabaseLister, or
// the instance would not answer. It is deliberately distinct from an empty answer, because an
// instance that says "I hold nothing" and one that says nothing at all are the difference between a
// fact and a gap, and every caller of this makes a decision that turns on which one it has.
func (e *Engine) appDatabases(ctx context.Context, env, instance string) (apps []string, known bool) {
	lister, ok := e.dbProvisioner.(AppDatabaseLister)
	if !ok {
		return nil, false
	}
	found, err := lister.ListAppDatabases(ctx, env, instance)
	if err != nil {
		return nil, false
	}
	// Sorted here rather than trusted from the provisioner, so the result the caller reports and the
	// order the final backups are taken in are both deterministic.
	sort.Strings(found)
	return found, true
}

// removalConsequence renders what this removal will actually do, for the guardrail's confirmation
// message: which volume is destroyed or kept (by name), how many apps are affected and which, that
// the backups outlive the database either way, and — for a data-deleting removal — whether a final
// off-cluster backup is taken first. It is deliberately concrete — "this is destructive" tells a
// human nothing they can decide on; "this destroys the databases of 2 attached apps (api, web)"
// does.
//
// The final-backup clause is in the message the human APPROVES rather than only in the output they
// read afterwards, because the two answers lead to different decisions: "a copy is made first, and
// the removal is abandoned if it fails" and "nothing is copied, this is the last moment the data
// exists" are not the same question being confirmed (ADR-0064 §5).
func removalConsequence(info AddonInfo, deleteData bool, apps []string, plan finalBackupPlan) string {
	spec, known := LookupAddon(info.Type)
	hasVolume := info.Mode == "installed" && known && spec.StorageGi > 0
	what := fmt.Sprintf("removing the add-on %q", info.Name)
	// The volume the removal ACTS on, which for Postgres is CloudNativePG's claim rather than the
	// instance's own name (ADR-0066 §1). A confirmation naming a volume that does not exist is worse
	// than one naming none: it reads as precise.
	volume := AddonDataVolumeName(info.Type, info.Name)

	switch {
	case hasVolume && deleteData:
		what += fmt.Sprintf(" AND DESTROYING its data volume %q", volume)
		if info.Type == AddonPostgres {
			what += ", " + destroyedDatabases(apps)
			// This environment's backup claim, not the compiled-in one: with a claim per environment
			// (ADR-0067 §1) naming the wrong one would tell the operator that a volume they are not
			// touching is what survives.
			if claim, err := BackupVolumeName(AddonPostgres, info.Name); err == nil {
				what += fmt.Sprintf(" (the backup volume %q is kept)", claim)
			}
		}
		what += "; " + plan.consequence()
	case hasVolume:
		what += fmt.Sprintf(" — its data volume %q is KEPT and reinstalling the add-on reuses it", volume)
		if info.Type == AddonPostgres && len(apps) > 0 {
			what += fmt.Sprintf("; %s cannot reach a database until it is reinstalled", pluralApps(apps))
		}
	default:
		// A stateless add-on (cache) has no volume, so there is nothing for deleteData to destroy.
		what += " (it holds no data volume)"
	}
	return what
}

// destroyedDatabases phrases the per-app data loss a data-deleting Postgres removal causes. An empty
// app list is stated as such rather than silently omitted: "no app is attached" is exactly the fact
// that makes the removal safe to approve, and the caller should see it.
func destroyedDatabases(apps []string) string {
	if len(apps) == 0 {
		return "which holds every attached app's database (no app is currently attached)"
	}
	return "destroying the database of " + pluralApps(apps)
}

// pluralApps renders an app list as "N attached apps (api, web)" / "1 attached app (web)".
func pluralApps(apps []string) string {
	noun := "attached apps"
	if len(apps) == 1 {
		noun = "attached app"
	}
	return fmt.Sprintf("%d %s (%s)", len(apps), noun, strings.Join(apps, ", "))
}

// AttachResult is the outcome of attaching an app to an add-on (ADR-0031). It carries the KEY
// NAME the connection string was written under (e.g. "DATABASE_URL") — never the value, which
// lives only in the app's Kubernetes Secret.
type AttachResult struct {
	App   string    `json:"app"`
	Addon AddonType `json:"addon"`
	// Environment is the environment whose instance the database was provisioned on (the reserved
	// "default" for the implicit one). It is reported because it is the thing that decides WHICH
	// database the app just got (ADR-0067 §1) — an attach result that named only the app would read
	// identically for two environments holding entirely separate data.
	Environment string `json:"environment,omitempty"`
	// Instance is the LABEL of the instance the database was provisioned on (ADR-0091 §1). An
	// environment may hold more than one, so the environment on its own no longer says which server
	// the app was given — and an app may hold several attachments, which read identically without it.
	Instance string `json:"instance,omitempty"`
	// SecretKey is the env-var name under which the generated connection string was written into
	// the app's per-app Secret. The value is never returned (ADR-0029/0031).
	SecretKey string `json:"secret_key"`
	// PreviousSecretKey is the name this attachment used BEFORE this call, set only when the attach
	// renamed it. One app has one database per INSTANCE, so a rename MOVES that attachment's variable
	// rather than adding a second: the connection string is written under the new name and the old name is
	// removed, because the old one would otherwise hold a password this attach has already rotated —
	// a variable the app still reads and that no longer connects. It is reported so the move is
	// stated rather than inferred from a variable quietly disappearing (issue #462).
	PreviousSecretKey string `json:"previous_secret_key,omitempty"`
	// ReadSecretKey is the env-var name the READ ADDRESS was written under, and it is set only when
	// the instance actually has a standby (ADR-0081 §2). An instance with none has no `-ro` endpoint
	// for the address to resolve to, so the variable is absent rather than present and dead: an
	// address that is always there reads as a thing to use, and a developer who wires reads to one
	// that resolves to nothing finds out at the first query.
	ReadSecretKey string `json:"read_secret_key,omitempty"`
}

// AttachAddonOptions is everything an attach carries beyond the add-on, the app and the environment.
// It is a struct rather than two trailing strings for DetachAddonOptions' reason: `AttachAddon(ctx,
// t, app, env, "", "analytics")` is not a call anyone can review.
type AttachAddonOptions struct {
	// Instance is the LABEL of the instance to attach to (ADR-0091 §3). Empty is the environment's
	// default instance, so today's command means today's thing; a label selects one of the several an
	// environment may hold.
	Instance string
	// EnvKey NAMES THE VARIABLE the connection string is written under, and EMPTY IS NOT A SYNONYM
	// FOR "DATABASE_URL" (issue #462). Empty means "whatever this attachment already uses", which is
	// DATABASE_URL for every app that never chose otherwise and the chosen name for one that did.
	//
	// A SECOND ATTACHMENT HAS TO NAME ONE. An app attached to a second instance has no name it
	// already uses, and the name it would fall back to belongs to the first attachment — so the
	// attach is refused with the conflict named rather than handed a derived `DATABASE_URL_2` the
	// application does not read (ADR-0091 §3).
	EnvKey string
	// Confirm satisfies the addon.attach guardrail's confirmation hold (ADR-0095 §1). It is the
	// caller asserting that a human approved the operation the held message described; nothing here
	// verifies that, which is what makes a confirm a raised floor rather than a boundary.
	Confirm bool
}

// attachConsequence is the sentence the addon.attach hold prints, and it says which of the two
// things this call is doing (ADR-0095 §4). A first attach creates; a re-attach ROTATES, which is the
// one part of an attach nothing can undo — the connection string is generated server-side and never
// returned, so the previous password is gone the moment provisioning runs, and any holder of it
// other than the app itself simply stops connecting.
//
// Following what the call does rather than what the verb is called is ADR-0090 §5's rule: a
// confirmation describing a consequence the operation does not have trains the reader to discount
// the words.
//
// BURROW TELLS THE TWO APART BY THE RECORDED ATTACHMENT, the same row detach reads, so an attachment
// made before the variable name was recorded (issue #462) reads as a first attach and gets the
// understating message. That gap is left deliberately: the alternative is a live query against the
// instance's catalogue on the path of a prompt, and a first attach wrongly described as a rotation
// would train the reader to discount the words in the other direction.
func attachConsequence(t AddonType, app, env, instance, key, current string, recorded bool) string {
	if !recorded {
		return fmt.Sprintf("attaching %q to the %s instance %q in environment %s (creates a database and a login role on it, writes the connection string into %s, and restarts the app)",
			app, t, instance, env, key)
	}
	what := fmt.Sprintf("re-attaching %q to the %s instance %q in environment %s ROTATES ITS PASSWORD: %s is given the new connection string in %s, and any other holder of the old one stops connecting",
		app, t, instance, env, app, key)
	if key != current {
		what += fmt.Sprintf(", and %s is removed, so anything reading that name finds nothing", current)
	}
	return what
}

// AttachAddon gives app its own database on one of ENVIRONMENT env's Postgres instances and wires it
// into the app (ADR-0031). burrowd provisions an isolated database + login role on that environment's
// instance, generates the DATABASE_URL server-side, writes it into the app's per-app Secret in that
// environment's namespace via the SetSecretValue path (ADR-0029), and restarts the app so envFrom
// picks it up. No secret value crosses the agent control channel — the agent supplies only the app
// name; burrowd generates the value and never returns it. The audit row records
// {addon, app, env, instance, key} only — never the URL.
//
// It is HELD FOR CONFIRMATION by the addon.attach guardrail by default (ADR-0095 §1), scoped to the
// add-on instance the database lands on and to the environment. Attach was ungated until that record
// on the grounds that it destroys nothing; what that reading missed is that it provisions on a shared
// server, restarts the app, and on a re-attach rotates a password nothing can restore.
//
// The environment is not optional and is resolved before anything is provisioned (ADR-0067 §1). It
// is the whole of the fix for issue #339: databases keep their simple names, so with one instance
// per cluster an attach of `web` in staging found production's `web`, and because provisioning is
// idempotent it did not fail — it rotated the role password and handed staging a URL pointing at
// production's data. Resolving the environment first sends the provisioning at staging's own
// instance, and sends the Secret to staging's own namespace, so the two attaches have nothing in
// common to collide over.
//
// envKey NAMES THE VARIABLE the connection string is written under, and EMPTY IS NOT A SYNONYM FOR
// "DATABASE_URL" (issue #462). Empty means "whatever this attachment already uses", which is
// DATABASE_URL for every app that never chose otherwise and the chosen name for one that did — so
// omitting it keeps today's behaviour exactly, and a re-attach of a renamed attachment rotates the
// password into the name it is actually read under instead of quietly moving it back to the default.
func (e *Engine) AttachAddon(ctx context.Context, t AddonType, app, env string, opts AttachAddonOptions) (AttachResult, error) {
	envKey := opts.EnvKey
	if err := (App{Name: app}).Validate(); err != nil {
		return AttachResult{}, fmt.Errorf("attach addon: %w: %w", ErrInvalid, err)
	}
	if t != AddonPostgres {
		return AttachResult{}, fmt.Errorf("attach addon %s: only the postgres add-on supports attach: %w", t, ErrInvalid)
	}
	if e.dbProvisioner == nil {
		return AttachResult{}, fmt.Errorf("attach addon %s: database provisioning is not configured: %w", t, ErrNotImplemented)
	}
	if envKey != "" {
		if err := validateEnvKey(envKey); err != nil {
			return AttachResult{}, fmt.Errorf("attach addon %s for %s: %w: %w", t, app, ErrInvalid, err)
		}
	}
	targetEnv, ns, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: %w", t, app, err)
	}
	// WHICH INSTANCE, resolved from the registry before anything is provisioned (ADR-0091 §3). No
	// opts.Instance is the environment's default instance, so today's command means today's thing;
	// naming one selects it, and naming one that does not exist is a refusal rather than a database
	// on the wrong server.
	inst, err := e.resolveInstance(ctx, t, targetEnv, opts.Instance)
	if err != nil {
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: %w", t, app, err)
	}
	// The name THIS attachment uses today, which is the recorded one — or DATABASE_URL for an
	// attachment made before the name was a choice, and only on the environment's default instance,
	// because that is the only instance such an attachment can be against. It is both the default for
	// this call and the key a rename has to clean up afterwards.
	//
	// AN UNRECORDED ATTACHMENT TO A SECOND INSTANCE OWNS NOTHING, and that distinction is what keeps
	// a second attach from overwriting the first one's connection string: without it the second
	// attach would believe it already holds `DATABASE_URL` and skip the check that refuses it
	// (ADR-0091 §3).
	current, recorded, err := e.db.AddonEnvKey(ctx, string(t), app, targetEnv, inst.Name)
	if err != nil {
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: reading the recorded variable name: %w", t, app, err)
	}
	if !recorded && isDefaultInstance(inst) {
		current = AppDatabaseURLKey
	}
	key := current
	if envKey != "" {
		key = envKey
	}
	if key == "" {
		key = AppDatabaseURLKey
	}
	k := e.k8s.WithNamespace(ns)
	// A NAME THIS ATTACHMENT DOES NOT ALREADY OWN MUST BE FREE. Attach writes a value nobody can read
	// back, so writing over an app's existing API token, a config var, or ANOTHER ATTACHMENT'S
	// connection string would destroy it with no way to recover it and no way to notice.
	//
	// The attachment's own name is its to overwrite, which is what a re-attach rotating the password
	// does; everything else is checked. That is also how a second attachment is refused a variable
	// rather than handed a derived one: the first attachment holds `DATABASE_URL`, so the second one
	// is told the name is taken and asked to name its own (ADR-0091 §3). Burrow does not invent
	// `DATABASE_URL_2` — a generated name is a name the application was never told to read, and the
	// attach would report success while the app found nothing.
	if key != current {
		if err := e.refuseOccupiedEnvKey(ctx, k, app, targetEnv, key); err != nil {
			return AttachResult{}, fmt.Errorf("attach addon %s for %s: %w", t, app, err)
		}
	}
	// The redacted audit args carry the add-on, app, environment and KEY names only — never the
	// generated URL (ADR-0031). The environment is salient, non-secret metadata: it is what says
	// which database the app was given (ADR-0027); the key name says where the app reads it, and a
	// key NAME is not a secret (ADR-0028) — it is already in every error on this path.
	args := map[string]string{"addon": string(t), "app": app, "env": targetEnv, "instance": instanceLabel(inst), "key": key}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return AttachResult{}, fmt.Errorf("attach addon %s: loading guardrail policy: %w", t, err)
	}
	// THE GUARDRAIL RUNS LAST AMONG THE CHECKS AND FIRST AMONG THE EFFECTS. Everything above it reads
	// and validates — the environment, the instance, the variable name, whether that name is free —
	// so the held message can name the exact instance and the exact key, and nothing has been
	// provisioned when it holds.
	//
	// The disposition is scoped by the INSTANCE, as every other two-target add-on verb's is
	// (ADR-0085 §1, ADR-0095 §2): the database and the role land on that one server, and protecting
	// one app by name would leave the identical verb free to put a database on the same instance for
	// the next, which reads as protection and is not. Unlike the other addon.* codes this one is also
	// env-scopable, so the environment reaches the lookup as a tier of its own as well as through the
	// instance (ADR-0095 §2).
	if err := e.recordDecision(ctx, auditOpAddonAttach, app, args, GuardrailAddonAttach,
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: targetEnv, Name: instanceLabel(inst)}, "addon attach", GuardrailAddonAttach, opts.Confirm,
			attachConsequence(t, app, targetEnv, instanceLabel(inst), key, current, recorded))); err != nil {
		return AttachResult{}, err
	}

	// Provision the database/role on THIS instance and compose the connection string. The returned
	// url is a SECRET value: from here it is handed only to SetSecretValue and never logged, audited,
	// or returned.
	url, err := e.dbProvisioner.EnsureAppDatabase(ctx, app, targetEnv, inst.Name)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonAttach, app, args, err)
		// EnsureAppDatabase's error names the app/environment identifier only, never the URL.
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: %w", t, app, err)
	}

	// Write the connection string into the app's per-app Secret IN THIS ENVIRONMENT'S NAMESPACE and
	// roll the app there to pick it up — the ADR-0029 secret path, the same one `secret set` uses.
	// The value never crosses the audit log, the agent control channel, or Postgres.
	if err := k.SetSecretValue(ctx, app, key, url); err != nil {
		e.recordExecution(ctx, auditOpAddonAttach, app, args, err)
		// SetSecretValue's error names the app and key only — never the value.
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: writing %s: %w", t, app, key, err)
	}
	// RECORD THE NAME IMMEDIATELY AFTER WRITING IT, before anything else can fail. Detach, the
	// dependency check and the restore cutover all read this row to find the variable, so the window
	// where the Secret holds a name the record does not know is the window where a detach would leave
	// a live credential behind. A failure here is reported rather than swallowed, and re-running the
	// same attach closes it.
	if err := e.db.SetAddonEnvKey(ctx, string(t), app, targetEnv, inst.Name, key, e.clock.Now()); err != nil {
		e.recordExecution(ctx, auditOpAddonAttach, app, args, err)
		return AttachResult{}, fmt.Errorf("attach addon %s for %s: the connection string was written into %s but the name could not be recorded, so re-run this attach: %w", t, app, key, err)
	}
	// A rename MOVES the variable. EnsureAppDatabase has just rotated the role's password, so the old
	// name now holds a connection string that no longer authenticates: leaving it would give the app
	// two database variables, one of them silently dead, which is worse than the single name this
	// replaces. Removing an absent key is a no-op.
	previous := ""
	if current != "" && key != current {
		if err := k.UnsetSecretKey(ctx, app, current); err != nil {
			e.recordExecution(ctx, auditOpAddonAttach, app, args, err)
			return AttachResult{}, fmt.Errorf("attach addon %s for %s: the connection string was written into %s but the previous %s could not be removed, so the app still holds a stale one: %w", t, app, key, current, err)
		}
		previous = current
	}
	// The read address, when this instance has a standby for it to point at (ADR-0081 §2). It is
	// settled BEFORE the roll below so one restart carries both variables, and it is best-effort by
	// design — see attachReadAddress.
	readKey := e.attachReadAddress(ctx, k, t, app, targetEnv, inst.Name, key, previous)
	if readKey != "" {
		args["read_key"] = readKey
	}
	// An attach writes a key the app may never have held, so it rolls through the one helper that
	// knows whether this app's pod template names its secret keys (ADR-0089 §4). A restart alone
	// would bring an enumerated app back with the connection string in its Secret, absent from its
	// environment, and the attach reported successful.
	if err := e.rollForSecretChange(ctx, k, "attach addon "+string(t), app, targetEnv); err != nil {
		e.recordExecution(ctx, auditOpAddonAttach, app, args, err)
		return AttachResult{}, err
	}
	e.recordExecution(ctx, auditOpAddonAttach, app, args, nil)
	return AttachResult{App: app, Addon: t, Environment: targetEnv, Instance: instanceLabel(inst), SecretKey: key, PreviousSecretKey: previous, ReadSecretKey: readKey}, nil
}

// attachReadAddress writes app's read address beside its connection string when the instance has a
// standby, and takes it away when it does not — so an attach leaves the app in the state the
// instance's CURRENT shape calls for rather than in the state it called for when the app last
// attached (ADR-0081 §2). It returns the key it wrote, or "" for an instance with no standby.
//
// IT IS BEST-EFFORT, AND DELIBERATELY SO. The connection string is already written and recorded by
// the time this runs; failing the attach because a second, optional variable could not be composed
// would turn a working attachment into a reported failure, and the operator's next move — re-run the
// attach — is the same either way. The address is also re-derived by `addon config <type> standbys`,
// which is the command that makes one relevant in the first place.
//
// A RENAME MOVES BOTH NAMES. The read key is derived from the attachment's own key, so a rename that
// left the old one behind would leave a `PG_DSN` beside a stale `DATABASE_URL_READ` — a variable
// naming a connection string that is no longer read.
func (e *Engine) attachReadAddress(ctx context.Context, k Kubernetes, t AddonType, app, targetEnv, instance, key, previous string) string {
	if previous != "" {
		_ = k.UnsetSecretKey(ctx, app, readAddressKey(previous))
	}
	shape, err := e.k8s.AddonInstanceShape(ctx, t, targetEnv, instance)
	if err != nil || shape.Standbys == 0 {
		// No standby, or no answer about whether there is one. Either way the address is removed
		// rather than left: `-ro` resolves to nothing at a standby-less instance, and an attach that
		// kept a stale one would hand the app a variable that used to work.
		_ = k.UnsetSecretKey(ctx, app, readAddressKey(key))
		return ""
	}
	reader, ok := e.dbProvisioner.(AppReadAddresser)
	if !ok {
		return ""
	}
	// The returned url is a SECRET value: from here it is handed only to SetSecretValue and never
	// logged, audited, or returned.
	url, err := reader.AppReadURL(ctx, app, targetEnv, instance)
	if err != nil {
		return ""
	}
	readKey := readAddressKey(key)
	if err := k.SetSecretValue(ctx, app, readKey, url); err != nil {
		return ""
	}
	return readKey
}

// refuseOccupiedEnvKey refuses a requested attachment variable that something else in the app's
// environment already answers to, NAMING WHAT HOLDS IT (issue #462's "refused with a message naming
// the conflict").
//
// Two sources can hold it, and both are checked because the app cannot tell them apart at runtime —
// its environment is the config store and the per-app Secret merged. A Secret key is the destructive
// case: the value is unreadable, so overwriting it destroys a credential permanently. A config key
// is the confusing one: both would render into the workload under one name, and which wins is not
// something a user should have to know.
//
// A cluster that will not answer is a REFUSAL rather than an assumption of "free". The whole point of
// the check is that the write is irreversible; proceeding on an unreadable Secret would be deciding
// the dangerous way on no evidence. A missing Secret is not that — an app that has never had one has
// nothing to overwrite, and that is the ordinary state of a first attach.
func (e *Engine) refuseOccupiedEnvKey(ctx context.Context, k Kubernetes, app, env, key string) error {
	keys, err := k.SecretKeys(ctx, app)
	switch {
	case err == nil:
		for _, existing := range keys {
			if existing == key {
				return fmt.Errorf("%s is already a secret in %s's environment %s, and attaching would overwrite a value that cannot be read back; remove it with `burrow secret unset %s %s` or attach under another name: %w", key, app, env, app, key, ErrInvalid)
			}
		}
	case errors.Is(err, ErrNotFound):
		// No Secret yet: nothing to overwrite. The ordinary state of a first attach.
	default:
		return fmt.Errorf("%s could not be checked against %s's existing secrets, and attaching would overwrite a value that cannot be read back: %w", key, app, err)
	}
	cfg, err := e.db.AppEnv(ctx, app)
	if err != nil {
		return fmt.Errorf("%s could not be checked against %s's config: %w", key, app, err)
	}
	if _, taken := cfg[key]; taken {
		return fmt.Errorf("%s is already a config var of %s, and the app would see two values under one name; remove it with `burrow config unset %s %s` or attach under another name: %w", key, app, app, key, ErrInvalid)
	}
	return nil
}

// DetachAddonOptions is everything a detach carries beyond the add-on and the app. It is a struct
// rather than a second positional boolean for RemoveAddonOptions' reason: one of these two fields
// destroys an application's data and the other does not, and `DetachAddon(ctx, t, app, env, true,
// true)` is not a call anyone can review.
type DetachAddonOptions struct {
	// Instance is the LABEL of the instance to detach from (ADR-0091 §3). Empty is the environment's
	// default instance. Detaching one attachment leaves every other attachment of the same app — its
	// variable, its role and its database — untouched.
	Instance string
	// DeleteData destroys the app's database as well as its access to it (ADR-0090 §2). Its absence
	// is the safe default and the ordinary detach: the credential goes, the rows stay, and a
	// re-attach gets them back.
	DeleteData bool
	// Confirm satisfies the addon.detach guardrail's confirmation hold (ADR-0020). It is NOT the
	// data-loss acknowledgement DeleteData requires: that gate lives on the operator CLI, because a
	// flag people already reach for reflexively is exactly the habit it exists to interrupt
	// (ADR-0064 §2, ADR-0090 §4).
	Confirm bool
}

// DetachAddon removes the variable app's connection string was written under and, behind the
// addon.detach confirm guardrail, ends app's access to its database on ENVIRONMENT env's Postgres
// instance (ADR-0090 §1, ADR-0067 §1). The app's login role is dropped and THE DATABASE IS KEPT, so
// a later attach of the same app adopts it and the data comes back. With opts.DeleteData the
// database is destroyed too, which is the only way to ask for that (ADR-0090 §2).
//
// The audit row records {addon, app, env, key, delete_data} — names only, never the value, and the
// disposition alongside the operation so the log distinguishes "took an app's access away" from
// "destroyed its database" (ADR-0027).
//
// The environment is required for the same reason attach requires it, and it still matters most
// here: without it, detaching `web` in staging would reach production's `web` — the same collision as
// issue #339, on the verb that can also destroy it.
func (e *Engine) DetachAddon(ctx context.Context, t AddonType, app, env string, opts DetachAddonOptions) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("detach addon: %w: %w", ErrInvalid, err)
	}
	if t != AddonPostgres {
		return fmt.Errorf("detach addon %s: only the postgres add-on supports detach: %w", t, ErrInvalid)
	}
	if e.dbProvisioner == nil {
		return fmt.Errorf("detach addon %s: database provisioning is not configured: %w", t, ErrNotImplemented)
	}
	targetEnv, ns, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return fmt.Errorf("detach addon %s for %s: %w", t, app, err)
	}
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return fmt.Errorf("detach addon %s: loading guardrail policy: %w", t, err)
	}
	// A detach names two things — the instance and the app — and the disposition is scoped by the
	// INSTANCE (ADR-0085 §1). That is where the data lives and where the reach stops: every database
	// this verb can drop sits on this one instance, so protecting one app by name would leave the
	// same verb free to wipe the next, which reads as protection and is not.
	//
	// WHICH instance is a registry lookup rather than a derivation (ADR-0091 §2): no opts.Instance is
	// the environment's default, which is what this command has always meant.
	inst, err := e.resolveInstance(ctx, t, targetEnv, opts.Instance)
	if err != nil {
		return fmt.Errorf("detach addon %s for %s: %w", t, app, err)
	}
	// THE KEY IS READ, NOT ASSUMED. Detach removes the variable this attachment was written under,
	// which is the recorded name — DATABASE_URL only for an attachment that never chose another
	// (issue #462), and only on the environment's default instance, since that is the only instance
	// an unrecorded attachment can be against. Reading it here, before the guardrail, means a detach
	// that cannot find out what to remove refuses rather than removing the default and leaving a live
	// credential in the app's environment pointing at a database this call is about to drop.
	key, recorded, err := e.db.AddonEnvKey(ctx, string(t), app, targetEnv, inst.Name)
	if err != nil {
		return fmt.Errorf("detach addon %s for %s: reading the recorded variable name: %w", t, app, err)
	}
	if !recorded {
		if !isDefaultInstance(inst) {
			return fmt.Errorf("detach addon %s for %s: %q holds no attachment for %s in environment %s: %w",
				t, app, instanceLabel(inst), app, targetEnv, ErrNotFound)
		}
		key = AppDatabaseURLKey
	}
	args := map[string]string{"addon": string(t), "app": app, "env": targetEnv, "instance": instanceLabel(inst), "key": key, "delete_data": strconv.FormatBool(opts.DeleteData)}
	// WHAT THE HOLD SAYS FOLLOWS WHAT THE CALL DOES. The guardrail guards losing an app's access to
	// its data, which is disruptive and worth a confirmation; it is not the gate on destroying the
	// data, which has its own and reaches this far only when someone asked for it in those words
	// (ADR-0090 §4). A confirmation describing a consequence the operation no longer has is worse
	// than no text: it trains an operator to discount the words.
	consequence := fmt.Sprintf("detaching %q from the %s instance %q in environment %s (removes its credential and drops its role; the database and its rows are kept, and re-attaching gets them back)", app, t, instanceLabel(inst), targetEnv)
	if opts.DeleteData {
		consequence = fmt.Sprintf("detaching %q from the %s instance %q in environment %s AND DESTROYING its database, with every row in it; this cannot be undone", app, t, instanceLabel(inst), targetEnv)
	}
	if err := e.recordDecision(ctx, auditOpAddonDetach, app, args, GuardrailAddonDetach,
		// addon.* is not EnvScopable, so the environment reaches the lookup through the instance
		// name rather than as a tier of its own; it is named in the message and the audit args
		// (ADR-0035 phase 2c, ADR-0067 §1).
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: targetEnv, Name: instanceLabel(inst)}, "addon detach", GuardrailAddonDetach, opts.Confirm, consequence)); err != nil {
		return err
	}

	// Remove the connection-string key first (the app stops seeing the credential), then ROLL THE APP
	// OFF IT, and only then touch the instance. Both act in this environment only. A missing key, and
	// a missing workload, are each a no-op.
	//
	// THE ROLL COMES BEFORE THE INSTANCE IS TOUCHED, and that ordering is load-bearing rather than
	// tidy, on both dispositions for different reasons. Destroying the database needs it because
	// PostgreSQL refuses to drop a database anything is still connected to, and an app holding a pool
	// is exactly that. Keeping it needs it because the credential is about to stop working: an app
	// left running against a role that no longer exists fails on its next connection rather than at a
	// moment anyone chose. It also fails in the better direction — a workload that cannot be rolled
	// aborts the detach with the attachment intact, rather than taking it apart and then reporting
	// that the app was left running against nothing.
	k := e.k8s.WithNamespace(ns)
	if err := k.UnsetSecretKey(ctx, app, key); err != nil {
		e.recordExecution(ctx, auditOpAddonDetach, app, args, err)
		return fmt.Errorf("detach addon %s for %s: removing %s: %w", t, app, key, err)
	}
	if err := e.rollForSecretChange(ctx, k, "detach addon "+string(t), app, targetEnv); err != nil {
		e.recordExecution(ctx, auditOpAddonDetach, app, args, err)
		return err
	}
	// The one place the two dispositions diverge, and they are separate calls rather than one call
	// with a flag: the seam that destroys an app's data should not be reachable by passing `false`
	// somewhere.
	teardown := e.dbProvisioner.RevokeAppDatabase
	if opts.DeleteData {
		teardown = e.dbProvisioner.DropAppDatabase
	}
	if err := teardown(ctx, app, targetEnv, inst.Name); err != nil {
		e.recordExecution(ctx, auditOpAddonDetach, app, args, err)
		return fmt.Errorf("detach addon %s for %s: %w", t, app, err)
	}
	// The attachment is gone, so the recorded name describes nothing. Leaving it would have a later
	// attach of the same app default to a name the app no longer reads. Best-effort: the variable and
	// the attachment are already gone, and failing the detach afterwards would report a completed
	// teardown as broken — and a stale row only ever names a key a re-attach would then write.
	if err := e.db.DeleteAddonEnvKey(ctx, string(t), app, targetEnv, inst.Name); err != nil {
		slog.WarnContext(ctx, "forgetting the attachment's recorded variable name failed", "app", app, "env", targetEnv, "instance", inst.Name, "error", err)
	}
	e.recordExecution(ctx, auditOpAddonDetach, app, args, nil)
	return nil
}

// BackupAddon backs up app's database on the installed Postgres add-on (ADR-0032, ADR-0063 §7):
// burrowd resolves where the backup is to go, records a pending backup, runs an in-cluster Job that
// pg_dumps the database to the backup PVC and — when an object-storage destination is registered —
// writes it to the store and reads it back, and only then marks the backup completed. The backup is
// recorded in the control-plane database: burrowd is not mounted to the backup PVC, so the database,
// not the volume, is the index of backups.
//
// THE ROW IS NOT ALLOWED TO SAY SUCCEEDED FOR BYTES THAT DID NOT ARRIVE. Every path off the Job that
// is not "the Job succeeded" writes BackupFailed with the closed reason the Job reported, because a
// backup recorded as completed when the store never got it is worse than no row at all: it converts a
// missing backup into a false assurance, and the assurance is only tested at restore time. Nothing
// in this function has a path that leaves the row pending on a known failure, and pending is never
// read as success — the age of the last SUCCESSFUL backup counts completed rows only, so a row
// stranded pending by a burrowd that died mid-Job keeps the age growing rather than resetting it.
//
// destination names which object-storage provider holds this backup; empty resolves it, which
// succeeds when exactly one is registered and refuses when several are (ADR-0063 §6). No object
// storage at all is not a failure — the dump still lands on the PVC, and the row records that it
// only ever reached the cluster.
//
// It moves no secret value: the Job reads the superuser password only via secretKeyRef, the
// object-storage credential reaches the pod only through a Job-owned Secret, and the audit row and
// the returned result name the add-on, app, backup id, path, destination, and size — never a
// credential. Backup is allowed by default (it destroys nothing) and safe over the agent control
// channel.
func (e *Engine) BackupAddon(ctx context.Context, t AddonType, app, env, instance, destination string) (BackupResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return BackupResult{}, fmt.Errorf("backup addon: %w: %w", ErrInvalid, err)
	}
	if t != AddonPostgres {
		return BackupResult{}, fmt.Errorf("backup addon %s: only the postgres add-on supports backup: %w", t, ErrInvalid)
	}
	// A dump is taken FROM one server, so the environment names which instance to read and is
	// recorded on the row (ADR-0067 §1). Without it, backing up `web` in staging would have dumped
	// production's `web` — and the resulting row would have looked entirely ordinary.
	targetEnv, _, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup addon %s for %s: %w", t, app, err)
	}
	// WHICH instance holds the database being dumped. An environment may hold more than one, and two
	// of them may each hold a database called `web` (ADR-0091 §4), so the instance is what selects the
	// server and the claim the dump lands on.
	inst, err := e.resolveInstance(ctx, t, targetEnv, instance)
	if err != nil {
		return BackupResult{}, fmt.Errorf("backup addon %s for %s: %w", t, app, err)
	}
	backup, _, err := e.backupApp(ctx, app, targetEnv, inst, destination)
	if err != nil {
		return BackupResult{}, err
	}
	return BackupResult{Backup: backup}, nil
}

// backupApp is the body of a single backup, with the environment already resolved: it resolves the
// destination, writes the pending row, runs the Job, and settles the row to completed or failed.
//
// It exists apart from BackupAddon because a data-deleting `addon remove` takes a final backup of
// every attached database before it destroys anything (ADR-0064 §5), and that caller needs the
// closed FAILURE REASON the Job reported, not just an error — it has to tell the operator why their
// removal was refused. Returning the BackupJobOutcome alongside the row is what carries the reason
// out; BackupAddon discards it because its own caller reads the row.
//
// The invariant BackupAddon documents is this function's: no path leaves a row that says completed
// for bytes that did not arrive, and no known failure leaves a row pending.
func (e *Engine) backupApp(ctx context.Context, app, targetEnv string, inst AddonInfo, destination string) (Backup, BackupJobOutcome, error) {
	const t = AddonPostgres
	backupID := e.ids.NewID()
	// The redacted audit args carry the add-on, app, environment, instance, backup, and destination
	// NAMES only — never a credential (ADR-0032, ADR-0063 §1).
	args := map[string]string{"addon": string(t), "app": app, "env": targetEnv, "instance": instanceLabel(inst), "backup": backupID}

	// Resolved BEFORE the row is written, so an ambiguous or misnamed destination fails without
	// leaving a pending backup behind that nothing will ever finish.
	dest, err := e.resolveBackupDestination(ctx, destination, app, targetEnv, backupID)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonBackup, app, args, err)
		return Backup{}, BackupJobOutcome{}, fmt.Errorf("backup addon %s for %s: %w", t, app, err)
	}

	// The claim is resolved before the row is written, from the SAME instance the dump is taken from,
	// so a row can never say a dump is on a volume the Job did not mount (ADR-0067 §1, ADR-0091 §4).
	claim, err := BackupVolumeName(t, inst.Name)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonBackup, app, args, err)
		return Backup{}, BackupJobOutcome{}, fmt.Errorf("backup addon %s for %s: %w", t, app, err)
	}
	backup := Backup{
		ID: backupID,
		// Stated rather than left to the store's default, so the row says which of the two mechanisms
		// took it even on an install that also takes physical ones (ADR-0066 §4).
		Kind:        BackupKindLogical,
		App:         app,
		Environment: targetEnv,
		CreatedAt:   e.clock.Now(),
		Volume:      claim,
		Path:        BackupPath(app, backupID),
		Status:      BackupPending,
		Destination: BackupDestinationCluster,
	}
	if dest != nil {
		backup.Destination = BackupDestinationObjectStore
		backup.Provider = dest.Provider
		backup.ObjectKey = dest.Key
		args["destination"] = dest.Provider
	}
	if err := e.db.RecordBackup(ctx, backup); err != nil {
		e.recordExecution(ctx, auditOpAddonBackup, app, args, err)
		return Backup{}, BackupJobOutcome{}, fmt.Errorf("backup addon %s for %s: recording backup: %w", t, app, err)
	}

	outcome, err := e.k8s.RunBackupJob(ctx, app, targetEnv, inst.Name, backupID, dest)
	if err != nil {
		// Best-effort, and deliberately not gated on succeeding: a failed status write leaves the row
		// pending, which still does not read as a successful backup anywhere.
		//
		// An empty reason is left empty rather than filled in with a guess. It means the backup never
		// got as far as a running Job — a rejected identifier, an unresolvable instance, a claim that
		// could not be created — and inventing "the store refused it" for that would send the next
		// reader somewhere the failure is not.
		_ = e.db.FailBackup(ctx, backupID, outcome.Reason, outcome.Detail)
		e.recordExecution(ctx, auditOpAddonBackup, app, args, err)
		return Backup{}, outcome, fmt.Errorf("backup addon %s for %s: %w", t, app, err)
	}
	if err := e.db.SetBackupStatus(ctx, backupID, BackupCompleted, outcome.SizeBytes); err != nil {
		// The bytes reached the destination and the registry did not hear about it. Say so on the row
		// if we can, so the failure is legible rather than being an unexplained pending; the operator
		// has a backup they cannot see, which is the harmless direction of this failure.
		_ = e.db.FailBackup(ctx, backupID, BackupReasonNotRecorded, "the backup reached its destination but its completion could not be written to the registry")
		e.recordExecution(ctx, auditOpAddonBackup, app, args, err)
		return Backup{}, BackupJobOutcome{Reason: BackupReasonNotRecorded}, fmt.Errorf("backup addon %s for %s: recording completion: %w", t, app, err)
	}
	backup.Status = BackupCompleted
	backup.SizeBytes = outcome.SizeBytes
	e.recordExecution(ctx, auditOpAddonBackup, app, args, nil)
	return backup, outcome, nil
}

// ListBackups returns recorded backups, newest first, from the control-plane database (ADR-0032).
// An empty app lists every app's backups and an empty env every environment's; a non-empty value
// restricts to that app or environment. Read-only and safe over the agent control channel — it names
// the app, environment, size, time, and on-PVC path, never a credential.
//
// An unfiltered listing deliberately spans environments rather than defaulting to one: a listing
// answers "what dumps exist", and each row now says which environment it came from, so the answer is
// legible without a filter (ADR-0067 §1). Backup and restore, which act on exactly one instance, do
// not get that latitude — they take the environment as a required target.
func (e *Engine) ListBackups(ctx context.Context, t AddonType, app, env string) ([]Backup, error) {
	if t != AddonPostgres {
		return nil, fmt.Errorf("list backups %s: only the postgres add-on supports backups: %w", t, ErrInvalid)
	}
	if app != "" {
		if err := (App{Name: app}).Validate(); err != nil {
			return nil, fmt.Errorf("list backups: %w: %w", ErrInvalid, err)
		}
	}
	backups, err := e.db.ListBackups(ctx, app, env)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	return backups, nil
}

// checkBackupVolume reports whether b's dump is on the claim environment targetEnv actually mounts.
//
// It is a SECOND check, beside the environment one, because the row and the bytes are two facts and
// the gap between them is what issue #349 was. `environment` says which instance a dump was taken
// from; `volume` says which disk it is on. They agree for every backup taken since backups became
// per-environment, and they disagree for exactly one population: a dump taken in a non-default
// environment while a single shared claim still existed. Deriving the claim from the environment
// instead of reading it would silently send the restore Job looking for that dump on a claim that
// was created empty.
//
// A row with no volume recorded is read as the shared claim, which is the only one it can be: the
// column was backfilled to it and nothing else has ever held a dump.
func checkBackupVolume(t AddonType, b Backup, targetEnv, instance string) error {
	want, err := BackupVolumeName(t, instance)
	if err != nil {
		return err
	}
	got := b.Volume
	if got == "" {
		got = PostgresBackupVolume
	}
	if got == want {
		return nil
	}
	return fmt.Errorf("backup %q is on the volume %q, and environment %q restores from %q: it was taken before backups were held per environment, when every environment shared one volume. "+
		"The dump is still there — copy it into %q, or take a fresh backup of this environment: %w",
		b.ID, got, targetEnv, want, want, ErrInvalid)
}

// RestoreAddon restores app's database from a recorded backup, overwriting its live contents
// (ADR-0032). It is behind the addon.restore confirm guardrail (it destroys live data), runs an
// in-cluster Job that pg_restores the named dump, and records the restore in the audit log. The Job
// reads the superuser password only via secretKeyRef; the audit row records {addon, app, env, backup}
// only — never a credential.
//
// The backup must belong to the app AND to the environment being restored into (ADR-0067 §1).
// Environments have separate instances holding separate data, so a dump taken from one is not a
// valid source for another: restoring staging's dump into production would overwrite live production
// data with staging's, which is the same class of incident as issue #339 pointed the other way.
func (e *Engine) RestoreAddon(ctx context.Context, t AddonType, app, backupID, env, instance string, confirm bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("restore addon: %w: %w", ErrInvalid, err)
	}
	if t != AddonPostgres {
		return fmt.Errorf("restore addon %s: only the postgres add-on supports restore: %w", t, ErrInvalid)
	}
	if backupID == "" {
		return fmt.Errorf("restore addon %s: a backup id is required: %w", t, ErrInvalid)
	}
	targetEnv, _, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return fmt.Errorf("restore addon %s for %s: %w", t, app, err)
	}
	// WHICH instance the dump goes back into. An environment may hold more than one, and restoring
	// into the wrong one overwrites a live database with another server's data — issue #339's shape
	// one level down (ADR-0091 §4).
	inst, err := e.resolveInstance(ctx, t, targetEnv, instance)
	if err != nil {
		return fmt.Errorf("restore addon %s for %s: %w", t, app, err)
	}

	// The backup must exist and belong to the app — resolve it before evaluating the guardrail so a
	// bad id reads as ErrNotFound rather than a spurious confirmation prompt (mirrors Rollback).
	backup, err := e.db.GetBackup(ctx, backupID)
	if err != nil {
		return fmt.Errorf("restore addon %s for %s: backup %q: %w", t, app, backupID, err)
	}
	// A PHYSICAL backup is refused here rather than attempted, and the refusal is the point of the
	// two paths being named differently (ADR-0066 §4). Physical recovery rewinds the WHOLE instance,
	// so honouring `restore <app>` against one would roll back every other app sharing it — a
	// cross-app data loss triggered by a single-app operation, asked for by somebody who picked the
	// most recent id off a listing. It is checked before the app comparison so the message says what
	// the backup IS, rather than that it belongs to an app named "".
	if backup.Kind == BackupKindPhysical {
		return fmt.Errorf("restore addon %s for %s: backup %q is a PHYSICAL backup of environment %s's whole instance, not a dump of %s's database. Restoring it would rewind every app sharing that instance, so it cannot be reached from a single-app restore; pick a backup taken with `burrow addon backup postgres %s`: %w",
			t, app, backupID, envName(backup.Environment), app, app, ErrInvalid)
	}
	if backup.App != app {
		return fmt.Errorf("restore addon %s for %s: backup %q belongs to app %q: %w", t, app, backupID, backup.App, ErrInvalid)
	}
	// A row recorded before backups carried an environment is the default environment's by
	// construction — it is the only instance that could have existed when it was written.
	if bEnv := envName(backup.Environment); bEnv != targetEnv {
		return fmt.Errorf("restore addon %s for %s: backup %q was taken from environment %q, not %q; a dump from another environment's instance is not a valid source: %w",
			t, app, backupID, bEnv, targetEnv, ErrInvalid)
	}
	// And the BYTES have to be on the claim this environment mounts, which is a second question the
	// row above cannot answer. Each environment's dumps live on its own claim (ADR-0067 §1), so the
	// restore Job mounts one volume and one only; a row whose recorded claim is a different one is a
	// dump taken before backups were per-environment, still sitting on the shared claim. Reaching it
	// would mean mounting a volume holding every other environment's dumps into this environment's
	// Job, which is the isolation this closed — so it is refused, naming where the dump actually is
	// rather than letting the Job fail on a file that is not there.
	if err := checkBackupVolume(t, backup, targetEnv, inst.Name); err != nil {
		return fmt.Errorf("restore addon %s for %s: %w", t, app, err)
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return fmt.Errorf("restore addon %s: loading guardrail policy: %w", t, err)
	}
	args := map[string]string{"addon": string(t), "app": app, "env": targetEnv, "instance": instanceLabel(inst), "backup": backupID}
	if err := e.recordDecision(ctx, auditOpAddonRestore, app, args, GuardrailAddonRestore,
		// Scoped by the INSTANCE, for the reason a detach is: the databases this verb overwrites all
		// live on one instance (ADR-0085 §1), and by its LABEL, which is the half an operator can read
		// and write a disposition against (ADR-0091 §4). addon.* is not EnvScopable, so the
		// environment reaches the lookup through the instance rather than as a tier of its own
		// (ADR-0035 phase 2c, ADR-0067 §1).
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: targetEnv, Name: instanceLabel(inst)}, "addon restore", GuardrailAddonRestore, confirm,
			fmt.Sprintf("restoring %q on the %s instance %q in environment %s from backup %s (overwrites its live database)", app, t, instanceLabel(inst), targetEnv, backupID))); err != nil {
		return err
	}

	if err := e.k8s.RunRestoreJob(ctx, app, targetEnv, inst.Name, backupID); err != nil {
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
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: env, Name: app}, "app delete", GuardrailAppDelete, confirm, fmt.Sprintf("deleting the app %q (its workload, routing, and release history)", app))); err != nil {
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
	// The app is gone, so its lifecycle hooks are commands for an image nobody will deploy. Leaving
	// them would have an app of the same name created later inherit a stranger's pre-deploy command
	// (ADR-0072 §1: unset means today's behaviour exactly, and a new app is unset).
	if err := e.db.DeleteAppHooks(ctx, app); err != nil {
		e.recordExecution(ctx, auditOpAppDelete, app, args, err)
		return fmt.Errorf("delete app %s: removing lifecycle hooks: %w", app, err)
	}
	// The app is gone, so its recorded exposure is intent about nothing. Leaving it would have the
	// observer report a missing Ingress for an app that was deliberately deleted (ADR-0074 §6).
	e.forgetExposure(ctx, app, env)
	// Same reasoning for the declared health endpoint: it is intent about an app that no longer
	// exists, and a redeploy under the same name should start from the conservative default rather
	// than inherit a path the previous occupant served (ADR-0076 §5). Best-effort — the app is
	// already gone, and failing the delete afterwards would report a completed teardown as broken.
	if err := e.db.DeleteHealthEndpoints(ctx, app); err != nil {
		slog.WarnContext(ctx, "removing the app's declared health endpoints failed", "app", app, "error", err)
	}
	// And the same for the keys it projected as files (ADR-0089 §5): the record is key names and
	// filenames about an app that no longer exists, and an app created later under the same name must
	// start with nothing on disk rather than inherit a previous occupant's file layout. Best-effort
	// for the same reason.
	if err := e.db.DeleteSecretMounts(ctx, app); err != nil {
		slog.WarnContext(ctx, "removing the app's secret mounts failed", "app", app, "error", err)
	}
	// And the same for a decision to turn the deploy-time dependency check off (ADR-0076 §4): an app
	// created later under the same name must start checked, which is Burrow's default, rather than
	// silently inherit a previous occupant's opt-out. Best-effort for the same reason.
	if err := e.db.DeleteDependencyCheckSettings(ctx, app); err != nil {
		slog.WarnContext(ctx, "removing the app's dependency-check setting failed", "app", app, "error", err)
	}
	// And the same for the variable name its attachments were written under (issue #462): an app
	// created later under the same name gets Burrow's default rather than a previous occupant's
	// choice, which would otherwise decide where a fresh attach writes. Best-effort for the same
	// reason. The database itself is not dropped here — deleting an app has never dropped its data,
	// which is what `addon detach` is for.
	if err := e.db.DeleteAppAttachments(ctx, app); err != nil {
		slog.WarnContext(ctx, "removing the app's recorded attachment variable names failed", "app", app, "error", err)
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

// QueryMetricsRange runs a PromQL range query against the connected metrics add-on and returns the
// matching time series over [start, end] sampled every step. It is the time-series sibling of
// QueryMetrics — the read path behind a sparkline or area chart — and resolves and authenticates the
// add-on identically (ADR-0026): it locates the "metrics" add-on, reads its token when the backend is
// authenticated, and dispatches through the querier keyed by that backend. The window is caller-supplied
// (no ambient time in the engine, ADR-0010). The selected querier must additionally implement the
// optional MetricsRangeQuerier seam; a backend that offers only instant queries returns a clean
// ErrNotImplemented-wrapped error. An empty backend picks the first metrics add-on; a non-empty backend
// targets a specific one (by its concrete backend or its registry name).
func (e *Engine) QueryMetricsRange(ctx context.Context, query string, backend string, start, end time.Time, step time.Duration) ([]MetricSeries, error) {
	if step <= 0 {
		return nil, fmt.Errorf("query metrics range: step must be positive: %w", ErrInvalid)
	}
	if end.Before(start) {
		return nil, fmt.Errorf("query metrics range: end %s is before start %s: %w", end.Format(time.RFC3339), start.Format(time.RFC3339), ErrInvalid)
	}
	if len(e.metrics) == 0 {
		return nil, fmt.Errorf("query metrics range: metrics querying is not configured: %w", ErrNotImplemented)
	}
	addons, err := e.db.Addons(ctx)
	if err != nil {
		return nil, fmt.Errorf("query metrics range: %w", err)
	}
	addon, found := selectAddon(addons, "metrics", backend)
	if !found {
		if backend != "" {
			return nil, fmt.Errorf("query metrics range: no metrics add-on with backend %q (have: %s): %w", backend, strings.Join(availableBackends(addons, "metrics"), ", "), ErrNotFound)
		}
		return nil, fmt.Errorf("query metrics range: no metrics add-on is connected — run `burrow addon connect prometheus`: %w", ErrNotFound)
	}
	q := e.metrics[addon.Backend]
	if q == nil {
		return nil, fmt.Errorf("query metrics range: no metrics querier for backend %q: %w", addon.Backend, ErrNotImplemented)
	}
	rq, ok := q.(MetricsRangeQuerier)
	if !ok {
		return nil, fmt.Errorf("query metrics range: range metrics querying is not supported by backend %q: %w", addon.Backend, ErrNotImplemented)
	}
	// An authenticated backend records the key under which its bearer token lives in the
	// burrow-credentials Secret; read it at query time so a rotation is picked up with no restart
	// (ADR-0023). An empty SecretKey means the backend is unauthenticated — pass no token.
	token := ""
	if addon.SecretKey != "" {
		token, err = e.credentials.Token(ctx, addon.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("query metrics range: reading token for add-on %q under key %q: %w", addon.Name, addon.SecretKey, err)
		}
	}
	series, err := rq.QueryMetricsRange(ctx, addon.Endpoint, query, token, start, end, step)
	if err != nil {
		return nil, fmt.Errorf("query metrics range: %w", err)
	}
	return series, nil
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

	// The recent-failure history and the coverage behind it (ADR-0074 §8). The live read above says
	// what is wrong now; this says whether it has been wrong before and when it started, which is
	// the pair of questions every diagnosis opens with and the pair no live read can answer.
	failures, cov, errF := e.appFailures(ctx, app, envName(env))
	if errF != nil {
		return StatusResult{}, fmt.Errorf("status %s: %w", app, errF)
	}
	res.Failures, res.Coverage = failures, cov
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

// Scale changes an app's replica count. It is bounded by the replica ceiling — an operational
// limit whose breach is a validation failure (ADR-0068 §2) — and guarded against scale-to-zero
// (ADR-0006). It does not create a new release: scaling adjusts the running workload, while a
// release records a deploy.
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
	// The ceiling is a bound, checked before the guardrails for the reason it is on the deploy path
	// (ADR-0068 §2): no disposition opens it and no confirmation satisfies it.
	if err := e.checkReplicaCeiling(ctx, env, "scale", fmt.Sprintf("%d replicas", replicas), replicas); err != nil {
		return ScaleResult{}, fmt.Errorf("scale %s: %w", app, err)
	}
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return ScaleResult{}, fmt.Errorf("scale %s: loading guardrail policy: %w", app, err)
	}
	args := map[string]string{"replicas": strconv.Itoa(int(replicas)), "env": envName(env)}
	if err := e.recordDecision(ctx, auditOpScale, app, args, "", pol.evaluateReplicas(ctx, GuardrailScope{Env: env, Name: app}, "scale", replicas, confirm)); err != nil {
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
// bounded twice — the replica ceiling bounds the requested max the same way it bounds a manual
// scale, so a max above the ceiling is refused exactly like scaling above it (ADR-0068 §2), and the
// app.autoscale guardrail gates the operation itself (allow by default). The HPA is applied even when
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
	// The autoscaler's maximum is bounded by the same ceiling a manual scale is, and checked the
	// same way: ahead of the guardrails, as a validation failure (ADR-0068 §2).
	if err := e.checkReplicaCeiling(ctx, env, "autoscale", fmt.Sprintf("a maximum of %d replicas", spec.MaxReplicas), spec.MaxReplicas); err != nil {
		return AutoscaleResult{}, fmt.Errorf("autoscale %s: %w", app, err)
	}
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
	if err := e.recordDecision(ctx, auditOpAutoscale, app, args, GuardrailAutoscale, pol.evaluateAutoscale(ctx, GuardrailScope{Env: env, Name: app}, confirm)); err != nil {
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
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: env, Name: app}, "autoscale", GuardrailAutoscale, confirm, "disabling autoscaling")); err != nil {
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
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: req.Env, Name: req.App}, "expose", GuardrailExposePublic, req.Confirm, fmt.Sprintf("exposing %s at %s", req.App, req.Host))); err != nil {
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
	// What the readiness probe resolved to BEFORE this exposure was recorded — publishing an app is
	// one of the two things that can change it (ADR-0076 §3), so it is read here, on the near side
	// of the write.
	before, _, _, herr := e.resolveHealth(ctx, req.App, envName(req.Env))
	// Record what was asked for, so an Ingress that later disappears is a failure Burrow can see
	// rather than an app that looks like it was never exposed (ADR-0074 §6).
	e.recordExposure(ctx, Exposure{App: req.App, Environment: envName(req.Env), Host: req.Host, Port: req.Port, TLS: req.TLS})
	if herr == nil {
		e.rollForReadiness(ctx, k, "expose", req.App, envName(req.Env), before)
	}
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
	k := e.k8s.WithNamespace(ns)
	if err := k.Unexpose(ctx, app); err != nil {
		if errors.Is(err, ErrNotFound) {
			// The routing is already gone, but a stale intent row would have the observer report a
			// missing Ingress forever, so it is dropped on this path too.
			e.forgetExposure(ctx, app, env)
			return fmt.Errorf("unexpose %s: not exposed: %w", app, err)
		}
		return fmt.Errorf("unexpose %s: %w", app, err)
	}
	before, _, _, herr := e.resolveHealth(ctx, app, envName(env))
	e.forgetExposure(ctx, app, env)
	// Unpublishing withdraws the port §3's default probe was checking, so the probe goes away with
	// it. Rolling that out is a RELAXATION — the pods stop being gated on a check that no longer has
	// a port behind it — which is the safe direction to move (§6).
	if herr == nil {
		e.rollForReadiness(ctx, k, "unexpose", app, envName(env), before)
	}
	return nil
}

// Guardrails returns the guardrail policy as a list for inspection (ADR-0020). With an empty env, or
// with `prod` — the environment install created, whose policy IS the global policy (ADR-0067 §2) —
// it returns the global policy; with an environment added later it returns that environment's
// effective policy under the env to global to default fallback, each entry marking whether its
// disposition is env-specific or inherited (ADR-0035 phase 2c). That environment must be registered;
// an unknown one is a clear ErrNotFound.
//
// With a name it returns the effective policy for that one app or add-on instance, each entry
// marking whether the disposition was set for the name, for the environment, globally, or is the
// built-in default (ADR-0085 §4) — which is what makes "why is this denied for this app" answerable
// without walking the fallback chain by hand. A name is only meaningful inside an environment, so
// it requires one, for the reason SetGuardrail does.
func (e *Engine) Guardrails(ctx context.Context, scope GuardrailScope) ([]GuardrailInfo, error) {
	if scope.Name != "" && scope.Env == "" {
		return nil, fmt.Errorf("guardrails: %q names something without saying which environment it is in; add --env: %w", scope.Name, ErrInvalid)
	}
	if scope.Env != "" && scope.Env != DefaultEnvironment {
		if _, err := e.db.GetEnvironment(ctx, scope.Env); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("guardrails: unknown environment %q: %w", scope.Env, ErrNotFound)
			}
			return nil, fmt.Errorf("guardrails: resolving environment %q: %w", scope.Env, err)
		}
	}
	p, err := e.db.Policy(ctx)
	if err != nil {
		return nil, fmt.Errorf("guardrails: loading policy: %w", err)
	}
	return p.GuardrailsFor(ctx, scope), nil
}

// SetGuardrail sets one guardrail's disposition (ADR-0020). It rejects an unknown guardrail
// or an invalid disposition as ErrInvalid. This is the operator's lever — exposed via the
// `burrow` CLI's `guard set`; `burrow-agent` can only list guardrails, so the agent cannot
// change its own.
//
// With an empty env, or with `prod`, it sets the GLOBAL disposition for code: `prod` is the
// environment install created and the baseline every other environment inherits, so `guard set --env
// prod app.delete deny` and `guard set app.delete deny` are deliberately the same write (ADR-0067
// §2). An environment added later stores the env-prefixed code (e.g. staging.app.delete) so its
// policy can diverge from that baseline (ADR-0035 phase 2c) — which is the gradient ADR-0065 §3
// expects, written as an opt-OUT of production's setting rather than a set of unrelated ones. Such
// an environment must be registered (an unknown one is ErrNotFound, catching typos), and only the
// app-level guardrails are env-scopable: a cluster-level guardrail (addon.*, dns.*) gates a
// cluster-wide operation and can only be set globally, so env-scoping one is rejected as
// ErrInvalid.
//
// With a NAME it sets the disposition for that one app or add-on instance, stored under
// env.name.code (ADR-0085 §1). Three rules govern it, and each is a refusal rather than a
// best-effort guess:
//
//   - The guardrail must DECLARE that one name bounds its effect (ADR-0085 §3). `guard set
//     dns.write --name web` is refused saying how far dns.write actually reaches, because a
//     guardrail settable as though it were narrower than it is describes its own blast radius
//     falsely.
//   - The name must come with an ENVIRONMENT. Without one the key would have to be name.code,
//     which is byte-identical to the key an ENVIRONMENT of that name produces — app and
//     environment names are both DNS labels, so nothing tells `website.app.run` apart from
//     "everything in the `website` environment". Quietly widening it to the environment tier
//     instead would be worse than an error: it would relax or tighten every app in an environment
//     nobody named. Note this is the one place `--env prod` is required and does not mean the
//     global policy — `prod` is a segment of the key here, not a synonym for the baseline.
//   - The name is checked for SHAPE but not for existence. A disposition may legitimately be set
//     before the thing it names exists, and arming a deny ahead of an install is a reasonable order
//     to work in; a name that never matches anything shows up as inherited dispositions under
//     `guard list --name`.
//
// With a BINDS it binds the disposition to one kind of credential (ADR-0094 §2), storing the key the
// same write would have used with the kind and a colon in front of it — `agent:app.delete`,
// `agent:prod.burrowd-cloud.app.deploy`. The disposition itself is unchanged: still one word, still
// allow, confirm or deny. Binding narrows on the CALLER axis and composes with the target axes rather
// than replacing them, so an operator can bind a global disposition, an environment's, or one app's.
//
// The kind must be one of the three ADR-0084 §3 defines, and it must be a kind this install actually
// issues — see requireCredentialsForBinding, which is the refusal §4 asks for.
func (e *Engine) SetGuardrail(ctx context.Context, scope GuardrailScope, binds CredentialKind, code GuardrailCode, d Disposition) error {
	if !KnownGuardrail(code) {
		// A limit code arriving here is the ADR-0068 §2 correction landing on someone who learned
		// the old shape — `guard set app.replica_ceiling allow` was how the ceiling was turned off.
		// Naming the surface that now carries it is the whole point of removing the code rather
		// than leaving it settable and inert.
		if KnownLimit(LimitCode(code)) {
			return fmt.Errorf("set guardrail: %q is an operational limit, not a guardrail: it is a bound a human sets, and exceeding it is refused rather than dispositioned. Set its value with `burrow cluster config set [--env <name>] %s <value>`: %w", code, code, ErrInvalid)
		}
		return fmt.Errorf("set guardrail: unknown guardrail %q: %w", code, ErrInvalid)
	}
	if !d.Valid() {
		return fmt.Errorf("set guardrail: invalid disposition %q (want allow, confirm, or deny): %w", d, ErrInvalid)
	}
	if err := e.checkBinding(ctx, binds); err != nil {
		return err
	}
	if scope.Name != "" {
		return e.setNamedGuardrail(ctx, scope, binds, code, d)
	}
	stored := code
	if scope.Env != "" && scope.Env != DefaultEnvironment {
		if !EnvScopable(code) {
			// A code that names one thing gets pointed at that lever rather than at a flat
			// refusal: --env alone does nothing for it, --env with --name is how it is narrowed.
			if NameScopable(code) {
				return fmt.Errorf("set guardrail: %q is a cluster-level guardrail and cannot be scoped to an environment on its own; set it globally without --env, or for one %s with `--env %s --name <name>`: %w", code, GuardrailTarget(code), scope.Env, ErrInvalid)
			}
			return fmt.Errorf("set guardrail: %q is a cluster-level guardrail and cannot be scoped to an environment; set it globally without --env: %w", code, ErrInvalid)
		}
		if err := e.requireEnvironment(ctx, scope.Env); err != nil {
			return err
		}
		stored = envPolicyKey(scope.Env, code)
	}
	return e.db.SetGuardrail(ctx, bindKey(binds, stored), d)
}

// checkBinding validates a `--binds` before anything is written (ADR-0094 §4). An empty binding is
// the ordinary, unbound case and passes straight through.
//
// The kind must be one of the three ADR-0084 §3 defines: the set is closed, recorded at issuance, and
// nothing else can ever be on the other side of a credential row, so a fourth value would be a key
// nothing will ever match.
//
// AND THE INSTALL MUST ISSUE CREDENTIALS AT ALL. On an install nobody has signed in to, every request
// carries the shared token, no request has a kind, and a kind-bound disposition would therefore bind
// NOTHING. A protection that silently protects nothing is the worst available outcome, so it is
// refused where it is asked for rather than discovered later, during whatever the deny existed to
// prevent. The unbound disposition remains available and remains the honest answer for a shared-token
// install: blunt, and it holds.
//
// Principals are the signal because a principal is what a credential belongs to and the two are
// recorded together — ClaimFirstPrincipal writes the first principal and its first credential in one
// transaction precisely so neither can exist without the other. An install with a principal has
// issued a credential; one with none has issued nothing.
func (e *Engine) checkBinding(ctx context.Context, binds CredentialKind) error {
	if binds == "" {
		return nil
	}
	if !binds.Valid() {
		return fmt.Errorf("set guardrail: %q is not a credential kind (want user, agent, or machine): %w", binds, ErrInvalid)
	}
	ps, err := e.db.Principals(ctx)
	if err != nil {
		return fmt.Errorf("set guardrail: checking whether this install issues credentials: %w", err)
	}
	if len(ps) == 0 {
		return fmt.Errorf("set guardrail: this install has no per-caller credentials, so a disposition bound to %q would bind nobody: every request carries the shared install token, which has no kind. Run `burrow auth login` first, or set the disposition without --binds so it binds every caller: %w", binds, ErrInvalid)
	}
	return nil
}

// bindKey applies a binding to a composed policy key, and is the one place the caller axis meets the
// target axes. An unbound write stores the key it always stored, which is what makes an install that
// never uses `--binds` byte-identical to one from before this existed.
func bindKey(binds CredentialKind, key GuardrailCode) GuardrailCode {
	if binds == "" {
		return key
	}
	return boundPolicyKey(binds, key)
}

// setNamedGuardrail writes the disposition for one app or add-on instance (ADR-0085 §1). The
// refusals it can produce are the point of it: each says what the guardrail actually targets, so an
// operator learns the shape of the policy from the message rather than from the source.
func (e *Engine) setNamedGuardrail(ctx context.Context, scope GuardrailScope, binds CredentialKind, code GuardrailCode, d Disposition) error {
	if !NameScopable(code) {
		return fmt.Errorf("set guardrail: %q cannot be set for one thing by name: %s. Set it for the whole cluster with `burrow guard set %s %s`: %w",
			code, GuardrailReach(code), code, d, ErrInvalid)
	}
	if scope.Env == "" {
		return fmt.Errorf("set guardrail: %q names the %s %q but no environment; add --env, because a name on its own is indistinguishable from an environment of the same name: %w",
			code, GuardrailTarget(code), scope.Name, ErrInvalid)
	}
	// A name is a DNS-1123 label wherever it comes from — an app name, or the instance name
	// AddonInstanceName produces. Checking the shape here is what keeps the composed key
	// unambiguous: a name carrying a dot would be a name that could pose as another tier.
	if err := (App{Name: scope.Name}).Validate(); err != nil {
		return fmt.Errorf("set guardrail: %s name %q is not usable as a policy key: %v: %w", GuardrailTarget(code), scope.Name, err, ErrInvalid)
	}
	if err := e.requireEnvironment(ctx, scope.Env); err != nil {
		return err
	}
	return e.db.SetGuardrail(ctx, bindKey(binds, namePolicyKey(scope.Env, scope.Name, code)), d)
}

// requireEnvironment refuses an environment that is not registered, so a typo lands as a clear
// ErrNotFound rather than as a policy row nothing will ever read. The default environment always
// exists and is not looked up.
func (e *Engine) requireEnvironment(ctx context.Context, env string) error {
	if env == "" || env == DefaultEnvironment {
		return nil
	}
	if _, err := e.db.GetEnvironment(ctx, env); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("set guardrail: unknown environment %q: %w", env, ErrNotFound)
		}
		return fmt.Errorf("set guardrail: resolving environment %q: %w", env, err)
	}
	return nil
}

// Limits returns every operational limit with its effective value for env, each marking the tier
// that value came from (ADR-0068 §3). With an empty env, or with `prod` — the environment install
// created, whose configuration IS the cluster configuration (ADR-0067 §2) — it reports the cluster
// tier and the built-in defaults; an environment added later reports its own values where it has
// them. That environment must be registered; an unknown one is a clear ErrNotFound.
//
// This is a read of what the operator set, and reading a limit is harmless. SETTING one is the
// operator's alone (ADR-0068 §4).
func (e *Engine) Limits(ctx context.Context, env string) ([]LimitInfo, error) {
	if env != "" && env != DefaultEnvironment {
		if _, err := e.db.GetEnvironment(ctx, env); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("limits: unknown environment %q: %w", env, ErrNotFound)
			}
			return nil, fmt.Errorf("limits: resolving environment %q: %w", env, err)
		}
	}
	cfg, err := e.db.OperationalConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("limits: loading operational configuration: %w", err)
	}
	return cfg.Limits(env), nil
}

// SetLimit sets one operational limit's value (ADR-0068). It rejects an unknown limit, or a value
// that is not of the limit's kind or lies outside its permitted range, as ErrInvalid. This is the
// operator's lever and it is on the operator CLI only — `burrow-agent` carries no `cluster config`,
// because a bound the agent can raise is not a bound (ADR-0068 §4).
//
// With an empty env, or with `prod`, it sets the CLUSTER value, for the reason SetGuardrail does
// the same: `prod` is the environment install created and the baseline every other environment
// inherits (ADR-0067 §2). An environment added later stores the env-prefixed code (e.g.
// staging.app.replica_ceiling) so its bound can diverge from that baseline; such an environment
// must be registered, and only a limit that DECLARES itself environment-scoped may be set that way
// (ADR-0068 §5).
//
// The value is normalized to the limit's canonical text form before it is stored, so `72h0m0s` and
// `72h` do not read back as two different settings.
func (e *Engine) SetLimit(ctx context.Context, env string, code LimitCode, value string) error {
	d, ok := lookupLimit(code)
	if !ok {
		// A guardrail code arriving here is the ADR-0068 §2 correction landing on someone who
		// learned the old shape, so name the surface that does carry it rather than only refusing.
		if KnownGuardrail(GuardrailCode(code)) {
			return fmt.Errorf("set limit: %q is a guardrail, not an operational limit; set its disposition with `burrow guard set %s <allow|confirm|deny>`: %w", code, code, ErrInvalid)
		}
		return fmt.Errorf("set limit: unknown limit %q (known limits: %s): %w", code, joinLimitCodes(), ErrInvalid)
	}
	n, err := d.parse(value)
	if err != nil {
		return fmt.Errorf("set limit %s: %w: %w", code, err, ErrInvalid)
	}
	stored := code
	if env != "" && env != DefaultEnvironment {
		if !d.envScoped {
			return fmt.Errorf("set limit: %q is cluster-wide and cannot be set for one environment; set it without --env: %w", code, ErrInvalid)
		}
		if _, err := e.db.GetEnvironment(ctx, env); err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("set limit: unknown environment %q: %w", env, ErrNotFound)
			}
			return fmt.Errorf("set limit: resolving environment %q: %w", env, err)
		}
		stored = LimitCode(env + "." + string(code))
	}
	return e.db.SetLimit(ctx, stored, d.format(n))
}

// joinLimitCodes lists the known limit codes for the "unknown limit" refusal, so an operator who
// mistyped one sees the set rather than being told only that theirs is not in it.
func joinLimitCodes() string {
	codes := LimitCodes()
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = string(c)
	}
	return strings.Join(out, ", ")
}

// checkReplicaCeiling refuses op when requested exceeds the effective replica ceiling for env
// (ADR-0068 §2). It runs BEFORE the guardrail decision on every path that names a replica count,
// because exceeding a bound is a validation failure rather than a policy decision: there is nothing
// to confirm, nothing to audit as held, and no disposition that opens it.
func (e *Engine) checkReplicaCeiling(ctx context.Context, env, op, what string, requested int32) error {
	cfg, err := e.db.OperationalConfig(ctx)
	if err != nil {
		return fmt.Errorf("loading operational configuration: %w", err)
	}
	return cfg.checkReplicaCeiling(env, op, what, requested)
}

// AutoDeploy returns the auto-deploy level configured for app in env (ADR-0052 §2). A missing
// configuration resolves to the built-in default (DefaultAutoDeployLevel, off): auto-deploy is
// opt-in, so an app with no stored level is off and is never polled (ADR-0058). The environment is
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
//
// opts.SkipHooks is the operator-only override that rolls back WITHOUT running the app's
// `pre-rollback` hook (ADR-0080 §2). It changes nothing about the default: a hook that is not skipped
// runs, and its failure still aborts.
//
// LIKE A DEPLOY, IT WAITS FOR THE ROLLOUT AND REPORTS WHAT HAPPENED (ADR-0093 §1). It used to answer
// at submission, and the sentence it answered with named the release being rolled back FROM as
// superseded — which, when the older image never became ready, is the release Kubernetes is still
// running. opts.NoWait declines the wait and reports the outcome as unknown.
func (e *Engine) Rollback(ctx context.Context, app, env string, opts RollbackOptions) (RollbackResult, error) {
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)
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
	// A skipped hook is resolved BEFORE the guardrail decision so the FIRST row of the trail already
	// says one was skipped and which (ADR-0080 §4). "We rolled back around a broken hook" is the fact
	// somebody needs afterwards, and a reader should find it on the rollback rather than infer it from
	// the absence of a hook row.
	var skipped []string
	if opts.SkipHooks {
		skipped = e.skippedRollbackHook(ctx, app, env)
		if len(skipped) > 0 {
			args["hooks_skipped"] = string(HookPreRollback)
			args["skipped_command"] = strings.Join(skipped, " ")
		}
	}
	if err := e.recordDecision(ctx, auditOpRollback, app, args, GuardrailRollback,
		pol.evaluateGuardrail(ctx, GuardrailScope{Env: env, Name: app}, "rollback", GuardrailRollback, opts.Confirm,
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
	replicas, err := e.resolveReplicas(ctx, k, app, 0)
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

	// Like the env, the readiness probe is current state rather than a per-release snapshot: a
	// rollback restores the prior image and command but keeps the probe the app has declared now
	// (ADR-0076 §3), so rolling back never reinstates a probe the operator has since changed.
	readiness, err := e.readinessFor(ctx, app, envName(env))
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: %w", app, err)
	}

	// A rollback fires `pre-rollback` and NEVER `pre-deploy` (ADR-0072 §8). A rollback is mechanically
	// a deploy of an older image, so §2's "every deploy path" would otherwise reach it — and running
	// the pre-deploy hook here would run A's migration tool while returning to A, which does not know
	// B's migration exists and would step back one of A's OWN migrations instead. The exclusion is
	// structural: a rollback does not go through the shared deploy path, and this is the only hook it
	// runs.
	//
	// It runs from `cur.Image` — the image being rolled back FROM, not the one being rolled back to —
	// because the code that knows how to undo B's migration is in B. It runs BEFORE traffic moves
	// back, so the schema is stepped back before the older code serves; a failure therefore aborts the
	// rollback, the same rule §3 gives the other pre phase, because letting the older code serve
	// against a half-stepped-back schema is the outcome the ordering exists to prevent. With no
	// pre-rollback hook set nothing runs at all, which is the safe forward-only default.
	//
	// AN OPERATOR CAN SKIP IT, AND ONLY HERE (ADR-0080 §2). Two different failures arrive as the same
	// abort — the revert failed, or the hook could not run at all — and only the first is a reason to
	// stay on the newer image. Since a rollback is what somebody reaches for when a deploy has gone
	// wrong, an abort it takes a second operation to get past is an extra step on the path that must
	// not have one. The skip is not "run it and ignore the result": the case it exists for is a hook
	// that cannot start, where running it again only spends the timeout again.
	//
	// What it deliberately is not is silent, lossy, or the agent's to make. It leaves the hook
	// configured (the escape it replaces was `hook unset`, which deletes it), it says so on the result
	// and in the audit row, and `--skip-hooks` is absent from `burrow-agent` — a rollback the agent
	// runs still aborts, with a message naming the command a human runs (ADR-0080 §3).
	if !opts.SkipHooks {
		if err := e.runHook(ctx, k, HookPreRollback, app, env, cur.Image, cfg, nil, noProgress); err != nil {
			rel.Status = ReleaseFailed
			_ = e.db.SaveRelease(ctx, rel) // best effort: record the failure
			e.recordExecution(ctx, auditOpRollback, app, args, auditableHookError(err))
			return RollbackResult{}, fmt.Errorf("rollback %s: %w", app, err)
		}
	}

	// And the same for the secret projection, where it matters most: a mount records how a credential
	// is delivered to whatever code is running, so a rollback to a release cut BEFORE the mount
	// existed keeps the file (ADR-0089 §5). Were this a release property, the incident escape hatch
	// would take the credential with it.
	mounts, secretEnv, err := e.secretProjectionFor(ctx, k, app, envName(env))
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: %w", app, err)
	}

	spec := WorkloadSpec{App: app, Kind: WorkloadDeployment, Image: target.Image, Env: cfg, Command: target.Command, MetricsPort: target.MetricsPort, Readiness: readiness, Replicas: replicas, SecretFiles: mounts, SecretEnvKeys: secretEnv, ReleaseID: rel.ID}
	if err := k.ApplyWorkload(ctx, spec); err != nil {
		rel.Status = ReleaseFailed
		_ = e.db.SaveRelease(ctx, rel)
		e.recordExecution(ctx, auditOpRollback, app, args, err)
		return RollbackResult{}, fmt.Errorf("rollback %s: applying to cluster: %w", app, err)
	}

	// THE OLDER IMAGE IS APPLIED. Whether it is SERVING is the question a rollback exists to answer,
	// and until ADR-0093 §1 nothing here asked it: the sentence below was written the moment the API
	// server took the object, and it named the release being rolled back FROM as superseded. When the
	// older image does not come up, that release is the one Kubernetes is still running — the broken
	// one the operator was fleeing — so the report was pointing away from the only pod worth looking
	// at, in the middle of an incident.
	//
	// So the rollback waits here, before it records or reports anything. The observation is the same
	// sync.OnceValue the `post-deploy` hook below takes, so the settle bound is spent at most once
	// however many parties want to know how it went (issue #407) and the report and the hook cannot
	// disagree about one rollout. What changed is that the rollback is now a consumer itself: it used
	// to observe only when a hook had been configured to be told.
	settle := e.settleOnce(ctx, k, app, envName(env), noProgress)
	var rollout *RolloutReport
	if !opts.NoWait {
		rollout = e.rolloutReport(ctx, k, app, settle())
	}

	// THE STATUS STILL SAYS `deployed` WHEN THE ROLLOUT DID NOT SETTLE, for the reason ADR-0092 §4
	// gives and one more this record adds (ADR-0093 §3). Status is what a rollback walks back from, so
	// demoting this row moves the handle; and marking it `failed` while superseding the release it
	// replaces would leave the app with no `deployed` release at all, which is the state in which
	// rollback stops working — reached by the operator who is already recovering.
	rel.Status = ReleaseDeployed
	rel.Rollout, rel.RolloutReason = recordedRollout(rollout)
	if err := e.db.SaveRelease(ctx, rel); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: recording successful release: %w", app, err)
	}
	cur.Status = ReleaseSuperseded
	if err := e.db.SaveRelease(ctx, cur); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: superseding release %s: %w", app, cur.ID, err)
	}
	// The audit row records the rollout beside the operation, the way a deploy's does (ADR-0027), so
	// a reviewer reading the trail afterwards is told whether the recovery this row is about ever
	// served — the question that matters most about a rollback and least about anything else.
	args["rollout"] = string(rel.Rollout)
	if rel.RolloutReason != "" {
		args["rollout_reason"] = rel.RolloutReason
	}
	e.recordExecution(ctx, auditOpRollback, app, args, nil)

	// The rollback has landed and is recorded; now disable auto-deploy so the watcher does not
	// re-apply the version just backed away from (ADR-0052 §5). Surfacing a disable failure still
	// matters, so return it wrapped even though the rollback itself succeeded.
	if err := e.db.DisableAutoDeploy(ctx, app, envName(env), reasonDisabledByRollback); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: disabling auto-deploy after rollback: %w", app, err)
	}
	res := RollbackResult{Release: rel, RolledBackToReleaseID: target.ID, SupersededReleaseID: cur.ID, Rollout: rollout}
	// The skip is stated on the result, plainly, at the moment it happens (ADR-0080 §4). It leads the
	// hints because it is the one thing about this rollback that is not ordinary.
	if len(skipped) > 0 {
		res.Hints = append(res.Hints, skippedHookNote(app, env, skipped))
	}
	// A rollback fires `post-deploy` too, told that the deploy it is reporting on was a rollback
	// (ADR-0072 §4). "Did this settle and is it serving?" is the same question whichever direction
	// the image moved, so a separate `post-rollback` phase would be a fourth name for an identical
	// answer — and the case that most needs reporting is a rollback that did not fix things.
	//
	// It runs from the image now serving, target.Image: the post phase reports on what IS running,
	// where `pre-rollback` ran from the image being left behind (§8).
	//
	// IT TAKES THE SETTLE THE ROLLBACK'S OWN REPORT TOOK. A rollback runs no dependency check, so the
	// hook is the only other consumer, and both read one observation of one rollout (issue #407). On
	// the ordinary path it is already made by the time the hook asks; a rollback that declined the
	// wait (NoWait) still settles here if a hook is set, because the phase is defined in terms of the
	// outcome.
	//
	// It reports NO PROGRESS. Issue #480 is about the silence of a deploy; a rollback reaches the
	// same two functions, and widening the reported surface to a second operation is a separate
	// change with its own client and CLI half.
	res.Hints = append(res.Hints, e.runPostDeployHook(ctx, k, app, env, target.Image, rel.ID, DeployKindRollback, cfg, settle, noProgress)...)
	return res, nil
}

// skippedRollbackHook reads the `pre-rollback` hook a `--skip-hooks` rollback is about to not run, so
// the skip can be reported and recorded with the command it skipped (ADR-0080 §4).
//
// It returns nil when no hook is set, because skipping nothing is not a skip: an operator who passes
// the flag on an app with no hook has changed nothing, and a hint or an audit row saying otherwise
// would be a record of something that did not happen.
//
// A READ FAILURE DOES NOT FAIL THE ROLLBACK. This is the incident path, and the flag's whole purpose
// is to remove a prerequisite from it; refusing to recover because a settings read was briefly
// unavailable would reinstate exactly the failure mode ADR-0080 exists to remove. The rollback skips
// the hook either way and the record loses one detail, which is the smaller harm.
func (e *Engine) skippedRollbackHook(ctx context.Context, app, env string) []string {
	command, err := e.db.AppHook(ctx, app, envName(env), HookPreRollback)
	if err != nil {
		slog.WarnContext(ctx, "reading the pre-rollback hook failed; the rollback skips it either way, but the record cannot name the command",
			"app", app, "env", envName(env), "error", err)
		return nil
	}
	return command
}

// AddEnvironment registers a named environment mapping name to namespace (ADR-0035 phase 2). It
// validates name as a DNS-1123-label-safe lowercase token and rejects `prod` (install already
// created it — ADR-0067 §2) and the retired `default`, then records it. The namespace and burrowd's
// Role there are created
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

// EnsureDefaultEnvironment registers the environment install creates — `prod`, mapped to the app
// namespace burrowd runs against — and returns it (ADR-0067 §2–§3). burrowd calls it once at
// startup, after the migrations, so the registry row exists on a FRESH install and is BACKFILLED on
// an existing one by the same code path: an install predating ADR-0067 gains an environment named
// `prod` pointing at the namespace its apps are already in, and nothing in the cluster moves or is
// renamed. Registering it here rather than from `burrow cluster install` is what makes the upgrade
// case free — a re-run, a restart, and an upgrade all arrive at the same one row.
//
// It is idempotent, and deliberately not a plain CreateEnvironment: an existing row is read back
// rather than treated as a duplicate error, so a second burrowd replica racing the first is a no-op
// rather than a failed startup.
//
// A row that already maps `prod` to a DIFFERENT namespace is refused rather than repointed. It can
// only exist on an install that ran `burrow env add prod --namespace …` before this change — an
// install where `prod` already means a namespace of its own, and silently redefining it would send
// every unqualified operation into the wrong namespace. Migration 00018 refuses the same case up
// front, so this is the second net rather than the first.
func (e *Engine) EnsureDefaultEnvironment(ctx context.Context) (Environment, error) {
	got, err := e.db.GetEnvironment(ctx, DefaultEnvironment)
	switch {
	case err == nil:
		if got.Namespace != e.appNamespace {
			return Environment{}, fmt.Errorf(
				"ensure default environment: %q is registered against namespace %q but this control plane deploys apps into %q; the name %q belongs to the environment mapped to the app namespace (ADR-0067 §2), so re-register the other one under a different name: %w",
				DefaultEnvironment, got.Namespace, e.appNamespace, DefaultEnvironment, ErrInvalid)
		}
		got.Default = true
		return got, nil
	case !errors.Is(err, ErrNotFound):
		return Environment{}, fmt.Errorf("ensure default environment: %w", err)
	}
	if err := e.db.CreateEnvironment(ctx, DefaultEnvironment, e.appNamespace); err != nil {
		// A concurrent replica won the race; the row it wrote is the same one, so read it back
		// rather than failing a startup that has nothing left to do.
		if got, gerr := e.db.GetEnvironment(ctx, DefaultEnvironment); gerr == nil {
			got.Default = true
			return got, nil
		}
		return Environment{}, fmt.Errorf("ensure default environment: %w", err)
	}
	return Environment{Name: DefaultEnvironment, Namespace: e.appNamespace, Default: true, CreatedAt: e.clock.Now()}, nil
}

// ListEnvironments returns the environments the cluster's burrowd knows about (ADR-0035 phase 2,
// ADR-0067 §2): the default environment `prod` first, followed by the environments added later in
// name order. Unlike before ADR-0067 the default is a REGISTERED row like any other — install
// creates it (EnsureDefaultEnvironment) — so a fresh install lists exactly one environment and a
// single-environment user never meets the ambiguity refusal.
//
// It is synthesized only when its row is missing, which is a burrowd that has not yet run its
// startup ensure. That fallback is not cosmetic: without it a cluster with `staging` registered and
// no `prod` row would list ONE environment, and ADR-0047's forcing function — which counts
// environments — would let an unqualified mutating operation through instead of refusing it.
func (e *Engine) ListEnvironments(ctx context.Context) ([]Environment, error) {
	registered, err := e.db.ListEnvironments(ctx)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	out := make([]Environment, 0, len(registered)+1)
	rest := make([]Environment, 0, len(registered))
	found := false
	for _, env := range registered {
		if env.Name == DefaultEnvironment {
			env.Default = true
			out = append(out, env)
			found = true
			continue
		}
		rest = append(rest, env)
	}
	if !found {
		out = append(out, Environment{Name: DefaultEnvironment, Namespace: e.appNamespace, Default: true})
	}
	return append(out, rest...), nil
}

// RemoveEnvironment unregisters a named environment (ADR-0035 phase 2), the inverse of
// AddEnvironment. It rejects `prod` — the environment install creates, which every unqualified
// operation resolves to (ADR-0067 §2) — and an empty name, and returns ErrNotFound for an unknown
// name (from the store). Like AddEnvironment it only touches the registry entry: the environment's namespace and any apps
// in it are managed out of band — kubeconfig-side by the operator in the single-tenant install, or
// by the managed control plane's own teardown in the cloud — so removing the mapping does not delete
// the namespace. A removed environment can be re-added later.
func (e *Engine) RemoveEnvironment(ctx context.Context, name string) error {
	switch name {
	case "":
		return fmt.Errorf("remove environment: name is empty: %w", ErrInvalid)
	case DefaultEnvironment:
		return fmt.Errorf("remove environment %q: the environment install created cannot be removed; every operation that names none resolves to it (ADR-0067 §2): %w", name, ErrInvalid)
	}
	if err := e.db.DeleteEnvironment(ctx, name); err != nil {
		return fmt.Errorf("remove environment %s: %w", name, err)
	}
	return nil
}

// resolveMutatingNamespace maps a mutating operation's environment name to its namespace, first
// applying the ADR-0047 forcing function: when the operation names no environment (an empty env) and
// more than one environment is registered — `prod` plus at least one added later — it refuses with a
// structured AmbiguousEnvironmentError that lists the environments and tells the caller to name one,
// rather than silently defaulting to `prod`. With only `prod` registered there is no ambiguity, so
// it resolves exactly like resolveNamespace and the common single-environment self-hoster is
// unaffected (ADR-0047 §2, ADR-0067 §4). The check is on registration, not reachability
// (ADR-0047 §1). Every env-scoped mutating engine method routes its
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

// resolveMutatingEnvironment resolves a mutating operation's target environment to its CANONICAL
// NAME and its namespace in one step, applying ADR-0047's ambiguity refusal exactly as
// resolveMutatingNamespace does (it is the same call). The name is what an operation that acts on an
// environment's own resources — an add-on instance, an app's database on it — needs: the namespace
// alone cannot say which instance to reach, and deriving one from the other is what let a second
// environment's attach land on the first environment's database (ADR-0067 §1, issue #339). An
// unnamed environment comes back as `prod`, the one install created, so a single-environment install
// keeps resolving to the instance and the namespace it already has (ADR-0067 §3).
func (e *Engine) resolveMutatingEnvironment(ctx context.Context, env string) (name, namespace string, err error) {
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return "", "", err
	}
	return envName(env), ns, nil
}

// resolveNamespace maps an environment name to the namespace its apps operate in (ADR-0035 phase
// 2b). An empty name or `prod` resolves to the engine's app namespace WITHOUT a registry read: the
// name and the namespace are separate values, and ADR-0067 §3 fixes the mapping between these two —
// `prod` is the app namespace the install already uses (`burrow-apps`), never `burrow-apps-prod`.
// Short-circuiting is what makes an install predating ADR-0067 resolve correctly in the window
// between its migration and its startup backfill. Any other name must be a registered environment;
// an unregistered name is a clear ErrNotFound. Guardrail policy is not consulted here: resolution
// only routes the namespace.
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

// envName canonicalizes an environment name for storage and the audit trail: an empty name reads as
// `prod`, the environment install created (ADR-0067 §2); any other name passes through. It is the
// key control-plane rows are stored under (releases, add-ons, backups, auto-deploy levels), which is
// why migration 00018 rewrote the rows an older install stored under the retired `default` — the
// canonical name has to be one string, not two read in parallel. The environment is salient,
// non-secret metadata, so it is also recorded in the redacted audit args of a guarded operation
// (ADR-0027).
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
