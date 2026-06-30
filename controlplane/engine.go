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
	registry    Registry
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
	case d.Credentials == nil:
		return nil, fmt.Errorf("controlplane: New: Credentials seam is required")
	case d.DNS == nil:
		return nil, fmt.Errorf("controlplane: New: DNS seam is required")
	}
	return &Engine{
		k8s:           d.Kubernetes,
		registry:      d.Registry,
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
	args := map[string]string{"image": req.Image, "replicas": strconv.Itoa(int(req.Replicas))}
	if err := e.recordDecision(ctx, auditOpDeploy, req.App, args, "", pol.evaluateReplicas("deploy", req.Replicas, req.Confirm)); err != nil {
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
		Image:       req.Image,
		Digest:      info.Digest,
		Env:         env,
		Command:     req.Command,
		MetricsPort: req.MetricsPort,
		Replicas:    req.Replicas,
		Status:      ReleasePending,
		CreatedAt:   e.clock.Now(),
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

	spec := WorkloadSpec{App: req.App, Kind: WorkloadDeployment, Image: req.Image, Env: env, Command: req.Command, MetricsPort: req.MetricsPort, Replicas: req.Replicas}
	if err := e.k8s.ApplyWorkload(ctx, spec); err != nil {
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
	return DeployResult{Release: rel, SupersededReleaseID: superseded}, nil
}

// SetConfig upserts one non-secret config var for an app in the config store (ADR-0028). The store
// is the single source of truth for the app's config. By default the change re-applies the running
// workload so it rolls and the running app picks the value up; with noRestart the value is only
// persisted and lands on the next deploy. An app with no running release simply persists and
// skips the apply — not an error. Config vars are non-secret, so there is no guardrail.
func (e *Engine) SetConfig(ctx context.Context, app, key, value string, noRestart bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("set config: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return fmt.Errorf("set config %s: %w: %w", app, ErrInvalid, err)
	}
	if err := e.db.SetAppEnv(ctx, app, key, value); err != nil {
		return fmt.Errorf("set config %s: persisting %s: %w", app, key, err)
	}
	if noRestart {
		return nil
	}
	return e.reapplyEnv(ctx, app)
}

// UnsetConfig removes one config var for an app from the config store (ADR-0028). Like SetConfig it
// re-applies the running workload by default so the running app drops the value, or only
// persists with noRestart. An app with no running release simply persists and skips the apply.
func (e *Engine) UnsetConfig(ctx context.Context, app, key string, noRestart bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("unset config: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return fmt.Errorf("unset config %s: %w: %w", app, ErrInvalid, err)
	}
	if err := e.db.UnsetAppEnv(ctx, app, key); err != nil {
		return fmt.Errorf("unset config %s: removing %s: %w", app, key, err)
	}
	if noRestart {
		return nil
	}
	return e.reapplyEnv(ctx, app)
}

// ListConfig returns the app's non-secret config store (ADR-0028). An app with no config yields an
// empty map and no error.
func (e *Engine) ListConfig(ctx context.Context, app string) (map[string]string, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return nil, fmt.Errorf("list config: %w: %w", ErrInvalid, err)
	}
	env, err := e.db.AppEnv(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("list config %s: %w", app, err)
	}
	return env, nil
}

// reapplyEnv re-renders the running workload with the current store env so a mutation rolls the
// Deployment (ADR-0028). It reconstructs the WorkloadSpec from the app's currently running release
// and the store. With no running release there is nothing to roll: the change is persisted and
// will land on the next deploy, so this is a no-op, not an error.
func (e *Engine) reapplyEnv(ctx context.Context, app string) error {
	releases, err := e.db.Releases(ctx, app)
	if err != nil {
		return fmt.Errorf("set env %s: reading release history: %w", app, err)
	}
	cur, ok := lastDeployed(releases)
	if !ok {
		return nil // no running workload yet; the change lands on the next deploy
	}
	env, err := e.db.AppEnv(ctx, app)
	if err != nil {
		return fmt.Errorf("set env %s: reading env: %w", app, err)
	}
	spec := WorkloadSpec{App: app, Kind: WorkloadDeployment, Image: cur.Image, Env: env, Command: cur.Command, MetricsPort: cur.MetricsPort, Replicas: cur.Replicas}
	if err := e.k8s.ApplyWorkload(ctx, spec); err != nil {
		return fmt.Errorf("set env %s: applying to cluster: %w", app, err)
	}
	return nil
}

// ListSecrets returns the env-var KEYS in an app's per-app Secret, sorted, never the values
// (ADR-0028/0004). Secret values live only in the Kubernetes Secret and never cross the API or
// MCP, so this read returns keys only. An app with no secrets yields an empty slice.
func (e *Engine) ListSecrets(ctx context.Context, app string) ([]string, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return nil, fmt.Errorf("list secrets: %w: %w", ErrInvalid, err)
	}
	keys, err := e.k8s.SecretKeys(ctx, app)
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
func (e *Engine) SetSecret(ctx context.Context, app, key, value string, noRestart bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("set secret: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return fmt.Errorf("set secret %s: %w: %w", app, ErrInvalid, err)
	}
	if err := e.k8s.SetSecretValue(ctx, app, key, value); err != nil {
		// Wrap with the app and key NAME only — never the value (ADR-0029).
		return fmt.Errorf("set secret %s: writing %s: %w", app, key, err)
	}
	if noRestart {
		return nil
	}
	// envFrom is read only at pod start, so writing a value under an existing key does not roll
	// the Deployment on its own — bump the restart annotation. A missing workload means nothing is
	// running yet: not an error, the change lands on the next deploy.
	if err := e.k8s.RestartWorkload(ctx, app, e.clock.Now()); err != nil {
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
func (e *Engine) UnsetSecret(ctx context.Context, app, key string, noRestart bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("unset secret: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return fmt.Errorf("unset secret %s: %w: %w", app, ErrInvalid, err)
	}
	if err := e.k8s.UnsetSecretKey(ctx, app, key); err != nil {
		return fmt.Errorf("unset secret %s: removing %s: %w", app, key, err)
	}
	if noRestart {
		return nil
	}
	// envFrom is read only at pod start, so removing a key from the Secret does not roll the
	// Deployment on its own — bump the restart annotation. A missing workload means nothing is
	// running yet: not an error, the change lands on the next deploy.
	if err := e.k8s.RestartWorkload(ctx, app, e.clock.Now()); err != nil {
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
func (e *Engine) ListApps(ctx context.Context) ([]WorkloadStatus, error) {
	apps, err := e.k8s.ListWorkloads(ctx)
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
		pol.evaluateGuardrail("addon install", GuardrailAddonInstall, confirm, fmt.Sprintf("installing the %s add-on (%s)", t, spec.Image))); err != nil {
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
		pol.evaluateGuardrail("addon remove", GuardrailAddonRemove, confirm, fmt.Sprintf("removing the add-on %q", name))); err != nil {
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
// destroys data), drops app's database and role from the shared Postgres instance (ADR-0031). The
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
		pol.evaluateGuardrail("addon detach", GuardrailAddonDetach, confirm,
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
		pol.evaluateGuardrail("addon restore", GuardrailAddonRestore, confirm,
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
func (e *Engine) DeleteApp(ctx context.Context, app string, confirm bool) error {
	if err := (App{Name: app}).Validate(); err != nil {
		return fmt.Errorf("delete app: %w: %w", ErrInvalid, err)
	}

	// Existence: an app exists if it has releases OR a live workload. Determine this before
	// evaluating the guardrail so an unknown app is ErrNotFound rather than a confirm prompt.
	releases, err := e.db.Releases(ctx, app)
	if err != nil {
		return fmt.Errorf("delete app %s: reading release history: %w", app, err)
	}
	exists := len(releases) > 0
	if !exists {
		if _, err := e.k8s.WorkloadStatus(ctx, app); err != nil {
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
	if err := e.recordDecision(ctx, auditOpAppDelete, app, nil, GuardrailAppDelete,
		pol.evaluateGuardrail("app delete", GuardrailAppDelete, confirm, fmt.Sprintf("deleting the app %q (its workload, routing, and release history)", app))); err != nil {
		return err
	}

	// Tear down, tolerating already-absent pieces: workload, then routing, then release records.
	if err := e.k8s.DeleteWorkload(ctx, app); err != nil && !errors.Is(err, ErrNotFound) {
		e.recordExecution(ctx, auditOpAppDelete, app, nil, err)
		return fmt.Errorf("delete app %s: removing workload: %w", app, err)
	}
	if err := e.k8s.Unexpose(ctx, app); err != nil && !errors.Is(err, ErrNotFound) {
		e.recordExecution(ctx, auditOpAppDelete, app, nil, err)
		return fmt.Errorf("delete app %s: removing routing: %w", app, err)
	}
	if err := e.db.DeleteReleases(ctx, app); err != nil {
		e.recordExecution(ctx, auditOpAppDelete, app, nil, err)
		return fmt.Errorf("delete app %s: removing release history: %w", app, err)
	}
	e.recordExecution(ctx, auditOpAppDelete, app, nil, nil)
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
	args := map[string]string{"replicas": strconv.Itoa(int(replicas))}
	if err := e.recordDecision(ctx, auditOpScale, app, args, "", pol.evaluateReplicas("scale", replicas, confirm)); err != nil {
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
		e.recordExecution(ctx, auditOpScale, app, args, err)
		return ScaleResult{}, fmt.Errorf("scale %s: %w", app, err)
	}
	e.recordExecution(ctx, auditOpScale, app, args, nil)
	return ScaleResult{App: app, PreviousReplicas: prev, Replicas: replicas}, nil
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

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return ExposeResult{}, fmt.Errorf("expose %s: loading guardrail policy: %w", req.App, err)
	}
	args := map[string]string{"host": req.Host, "port": strconv.Itoa(int(req.Port)), "tls": strconv.FormatBool(req.TLS)}
	if err := e.recordDecision(ctx, auditOpExpose, req.App, args, GuardrailExposePublic,
		pol.evaluateGuardrail("expose", GuardrailExposePublic, req.Confirm, fmt.Sprintf("exposing %s at %s", req.App, req.Host))); err != nil {
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

	if err := e.k8s.Expose(ctx, ExposeSpec{App: req.App, Host: req.Host, Port: req.Port, TLS: req.TLS, Issuer: req.Issuer}); err != nil {
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
			res.BlockedOn = "deployment"
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
func (e *Engine) Rollback(ctx context.Context, app string, confirm bool) (RollbackResult, error) {
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

	// Guardrail check only after the rollback is known to be valid, so "nothing to roll back to"
	// reads as ErrNotFound rather than a spurious confirmation prompt (mirrors DeleteApp). The
	// rollback guardrail defaults to allow — a recovery action — but an operator may set it to
	// confirm or deny (ADR-0020).
	pol, err := e.db.Policy(ctx)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: loading guardrail policy: %w", app, err)
	}
	args := map[string]string{"image": target.Image, "to_release": target.ID}
	if err := e.recordDecision(ctx, auditOpRollback, app, args, GuardrailRollback,
		pol.evaluateGuardrail("rollback", GuardrailRollback, confirm,
			fmt.Sprintf("rolling %q back to its previous release %s (image %s)", app, target.ID, target.Image))); err != nil {
		return RollbackResult{}, err
	}

	// Env is app-global current state, not snapshotted per release (ADR-0028): a rollback
	// restores the prior image and command but renders the env the app currently has set, not
	// whatever was in effect when the target was first deployed.
	env, err := e.db.AppEnv(ctx, app)
	if err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: reading env: %w", app, err)
	}

	rel := Release{
		ID:          e.ids.NewID(),
		App:         app,
		Image:       target.Image,
		Digest:      target.Digest,
		Env:         env,
		Command:     target.Command,
		MetricsPort: target.MetricsPort,
		Replicas:    target.Replicas,
		Status:      ReleasePending,
		Supersedes:  cur.ID,
		CreatedAt:   e.clock.Now(),
	}
	if err := e.db.SaveRelease(ctx, rel); err != nil {
		return RollbackResult{}, fmt.Errorf("rollback %s: recording release: %w", app, err)
	}

	args["env_keys"] = auditKeys(env) // KEY NAMES only — never values (ADR-0027)

	spec := WorkloadSpec{App: app, Kind: WorkloadDeployment, Image: target.Image, Env: env, Command: target.Command, MetricsPort: target.MetricsPort, Replicas: target.Replicas}
	if err := e.k8s.ApplyWorkload(ctx, spec); err != nil {
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
