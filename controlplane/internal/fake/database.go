// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Database = (*Database)(nil)

// Database is an in-memory controlplane.Database. It stores releases by ID and tracks
// per-app save order so LatestRelease and Releases are deterministic. Records are deep
// copied in and out, so callers never share Env/Command memory with the store — the
// same isolation a real database gives. Errors can be injected per operation.
type Database struct {
	mu         sync.Mutex
	byID       map[string]controlplane.Release
	order      map[string][]string // app -> release IDs, save order, deduplicated
	providers  map[string]controlplane.Provider
	addons     map[string]controlplane.AddonInfo
	appEnv     map[string]map[string]string                       // app -> key -> value
	hooks      map[string]controlplane.Hook                       // (app, env, phase) -> configured lifecycle hook
	autoDeploy map[string]map[string]controlplane.AutoDeployLevel // app -> env -> level
	reason     map[string]map[string]string                       // app -> env -> disable reason
	audit      []controlplane.AuditEntry                          // append-only, in append order
	backups    map[string]controlplane.Backup
	backupSeq  []string                            // backup IDs in record order, for deterministic newest-first listing
	envs       map[string]controlplane.Environment // registered environments by name
	errs       map[Op]error
	policy     controlplane.Policy
	// The failure ledger and its coverage record (ADR-0074 §4). They are separate from the audit
	// slice above for the same reason they are separate tables in the store: one is what Burrow was
	// asked to do, the other is what happened afterwards, and only the second is pruned (§7).
	failures  []controlplane.Failure
	windows   []controlplane.ObservationWindow
	exposures map[string]controlplane.Exposure // "app\x00env" -> recorded exposure intent
	// The declared health endpoints (ADR-0076 §5), keyed the same way exposures are, because the
	// readiness default reads one against the other.
	health map[string]controlplane.HealthEndpoint // "app\x00env" -> declared health endpoint
	// The secret keys each app projects as FILES (ADR-0089 §5) and the directory they land in, keyed
	// the same way. Key names and filenames only — the values stay in the fake Kubernetes' Secret
	// map, exactly as they stay in the cluster's.
	secretMounts map[string]map[string]controlplane.SecretMount // "app\x00env" -> key -> mount
	secretDirs   map[string]string                              // "app\x00env" -> directory override
	// Whether the deploy-time dependency check runs (ADR-0076 §4), keyed the same way. An absent
	// entry means ENABLED: the check is Burrow's default, so only a decision against it is recorded.
	depChecks map[string]bool // "app\x00env" -> the check runs
	// The variable name each attachment's connection string was written under (issue #462). An
	// absent entry means DATABASE_URL: the default did not move when the name became a choice.
	attachments map[string]string // "addon\x00app\x00env" -> env var name
	// What is locked (cloud ADR-0060), keyed "subject\x00env\x00name". An absent entry means
	// unlocked, which is what everything starts as: the map holds what somebody protected.
	locks map[string]controlplane.Lock

	// The operator-set operational limits (ADR-0068 §1), kept apart from policy above because a
	// limit carries a value rather than a disposition.
	limits controlplane.OperationalConfig

	// Who Burrow knows and what they hold (ADR-0084 §2). credentialSeq keeps issue order so a
	// principal's listing is deterministic when a test clock stamps several at the same instant.
	// The credentials here carry a token HASH, never a token — the fake keeps the same discipline
	// the store does, because a fake that is looser is a place a test can assert the wrong thing.
	principals    map[string]controlplane.Principal
	credentials   map[string]controlplane.Credential
	credentialSeq []string
}

// NewDatabase returns an empty fake database with the default guardrail policy.
func NewDatabase() *Database {
	return &Database{
		byID:         make(map[string]controlplane.Release),
		order:        make(map[string][]string),
		providers:    make(map[string]controlplane.Provider),
		addons:       make(map[string]controlplane.AddonInfo),
		appEnv:       make(map[string]map[string]string),
		hooks:        make(map[string]controlplane.Hook),
		autoDeploy:   make(map[string]map[string]controlplane.AutoDeployLevel),
		reason:       make(map[string]map[string]string),
		backups:      make(map[string]controlplane.Backup),
		envs:         make(map[string]controlplane.Environment),
		errs:         make(map[Op]error),
		policy:       controlplane.DefaultPolicy(),
		exposures:    make(map[string]controlplane.Exposure),
		health:       make(map[string]controlplane.HealthEndpoint),
		secretMounts: make(map[string]map[string]controlplane.SecretMount),
		secretDirs:   make(map[string]string),
		depChecks:    make(map[string]bool),
		attachments:  make(map[string]string),
		locks:        make(map[string]controlplane.Lock),
		limits:       controlplane.OperationalConfig{Values: map[controlplane.LimitCode]string{}},
		principals:   make(map[string]controlplane.Principal),
		credentials:  make(map[string]controlplane.Credential),
	}
}

// SetPolicy replaces the whole guardrail policy. It is a test helper for arranging a
// specific policy before exercising the engine.
func (d *Database) SetPolicy(p controlplane.Policy) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.policy = p
}

// Policy returns the current guardrail policy.
func (d *Database) Policy(ctx context.Context) (controlplane.Policy, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpPolicy]; err != nil {
		return controlplane.Policy{}, err
	}
	return d.policy, nil
}

// SetGuardrail persists one guardrail's disposition, overlaying it on the current policy.
func (d *Database) SetGuardrail(ctx context.Context, code controlplane.GuardrailCode, disp controlplane.Disposition) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetGuardrail]; err != nil {
		return err
	}
	if !disp.Valid() {
		return fmt.Errorf("database: set guardrail: invalid disposition %q", disp)
	}
	d.policy = d.policy.With(code, disp)
	return nil
}

// SetLimits replaces the whole operational configuration. It is a test helper for arranging a
// specific set of limits before exercising the engine, the way SetPolicy is for guardrails.
func (d *Database) SetLimits(c controlplane.OperationalConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.limits = c
}

// OperationalConfig returns the stored operational limit values (ADR-0068 §1).
func (d *Database) OperationalConfig(ctx context.Context) (controlplane.OperationalConfig, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpOperationalConfig]; err != nil {
		return controlplane.OperationalConfig{}, err
	}
	return d.limits, nil
}

// SetLimit persists one operational limit's value, overlaying it on the current configuration.
func (d *Database) SetLimit(ctx context.Context, code controlplane.LimitCode, value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetLimit]; err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("database: set limit %q: empty value", code)
	}
	d.limits = d.limits.With(code, value)
	return nil
}

// AutoDeployLevel returns app's auto-deploy level in env, or DefaultAutoDeployLevel (off) when none
// is set — a missing configuration resolves to the opt-in default, matching the store (ADR-0058).
func (d *Database) AutoDeployLevel(ctx context.Context, app, env string) (controlplane.AutoDeployLevel, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAutoDeployLevel]; err != nil {
		return "", err
	}
	if lvl, ok := d.autoDeploy[app][env]; ok {
		return lvl, nil
	}
	return controlplane.DefaultAutoDeployLevel, nil
}

// SetAutoDeployLevel upserts app's auto-deploy level in env, keyed by (app, env). It clears any
// stored disable reason: setting the level is the deliberate human re-enable action (ADR-0052 §5).
func (d *Database) SetAutoDeployLevel(ctx context.Context, app, env string, level controlplane.AutoDeployLevel) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetAutoDeployLevel]; err != nil {
		return err
	}
	if !level.Valid() {
		return fmt.Errorf("database: set auto-deploy level: invalid level %q", level)
	}
	if d.autoDeploy[app] == nil {
		d.autoDeploy[app] = make(map[string]controlplane.AutoDeployLevel)
	}
	d.autoDeploy[app][env] = level
	if d.reason[app] != nil {
		delete(d.reason[app], env)
	}
	return nil
}

// DisableAutoDeploy sets app's level to off in env and records the reason — the safety stop of
// ADR-0052 §5, keyed by (app, env).
func (d *Database) DisableAutoDeploy(ctx context.Context, app, env, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDisableAutoDeploy]; err != nil {
		return err
	}
	if d.autoDeploy[app] == nil {
		d.autoDeploy[app] = make(map[string]controlplane.AutoDeployLevel)
	}
	d.autoDeploy[app][env] = controlplane.AutoDeployOff
	if d.reason[app] == nil {
		d.reason[app] = make(map[string]string)
	}
	d.reason[app][env] = reason
	return nil
}

// AutoDeployReason returns the stored disable reason for app in env, or "" when none is set.
func (d *Database) AutoDeployReason(ctx context.Context, app, env string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAutoDeployReason]; err != nil {
		return "", err
	}
	return d.reason[app][env], nil
}

// AutoDeployCandidates returns the distinct (app, environment) pairs that have at least one recorded
// release, ordered by app then environment for a deterministic reconcile order — the set the
// pull-based watcher may reconcile (ADR-0052 Phase 4b). An empty stored Environment reads as the
// canonical "default", matching LatestRelease.
func (d *Database) AutoDeployCandidates(ctx context.Context) ([]controlplane.AppEnvRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAutoDeployCandidates]; err != nil {
		return nil, err
	}
	seen := make(map[controlplane.AppEnvRef]bool)
	for _, r := range d.byID {
		env := r.Environment
		if env == "" {
			env = controlplane.DefaultEnvironment
		}
		seen[controlplane.AppEnvRef{App: r.App, Env: env}] = true
	}
	out := make([]controlplane.AppEnvRef, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Env < out[j].Env
	})
	return out, nil
}

// SetError makes op return err until cleared with SetError(op, nil).
func (d *Database) SetError(op Op, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.errs, op)
		return
	}
	d.errs[op] = err
}

func (d *Database) SaveRelease(ctx context.Context, r controlplane.Release) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSaveRelease]; err != nil {
		return err
	}
	if r.ID == "" {
		return fmt.Errorf("database: save release: empty ID")
	}
	if _, exists := d.byID[r.ID]; !exists {
		d.order[r.App] = append(d.order[r.App], r.ID)
	}
	d.byID[r.ID] = cloneRelease(r)
	return nil
}

func (d *Database) Release(ctx context.Context, id string) (controlplane.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpRelease]; err != nil {
		return controlplane.Release{}, err
	}
	r, ok := d.byID[id]
	if !ok {
		return controlplane.Release{}, fmt.Errorf("database: release %q: %w", id, controlplane.ErrNotFound)
	}
	return cloneRelease(r), nil
}

// matchEnv reports whether a release stored with storedEnv belongs to the queried env. An empty
// stored Environment is treated as the canonical "default" so releases pre-set without an env still
// match the default environment (ADR-0052 Phase 4a).
func matchEnv(storedEnv, env string) bool {
	if storedEnv == "" {
		storedEnv = controlplane.DefaultEnvironment
	}
	return storedEnv == env
}

// LatestRelease returns the newest release for app in env — keyed per (app, environment) by filtering
// the app-global save order on the stored release's Environment (ADR-0052 Phase 4a).
func (d *Database) LatestRelease(ctx context.Context, app, env string) (controlplane.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpLatestRelease]; err != nil {
		return controlplane.Release{}, err
	}
	ids := d.order[app]
	for i := len(ids) - 1; i >= 0; i-- {
		if r := d.byID[ids[i]]; matchEnv(r.Environment, env) {
			return cloneRelease(r), nil
		}
	}
	return controlplane.Release{}, fmt.Errorf("database: latest release for app %q in %q: %w", app, env, controlplane.ErrNotFound)
}

// Releases returns every release for app in env, oldest first, keyed per (app, environment).
func (d *Database) Releases(ctx context.Context, app, env string) ([]controlplane.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpReleases]; err != nil {
		return nil, err
	}
	ids := d.order[app]
	out := make([]controlplane.Release, 0, len(ids))
	for _, id := range ids {
		if r := d.byID[id]; matchEnv(r.Environment, env) {
			out = append(out, cloneRelease(r))
		}
	}
	return out, nil
}

// ListReleases returns every release for app in env, newest first (reverse save order) — the deploy
// timeline the history surface reads, keyed per (app, environment). An app with no releases in env
// yields an empty slice and no error.
func (d *Database) ListReleases(ctx context.Context, app, env string) ([]controlplane.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpListReleases]; err != nil {
		return nil, err
	}
	ids := d.order[app]
	out := make([]controlplane.Release, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		if r := d.byID[ids[i]]; matchEnv(r.Environment, env) {
			out = append(out, cloneRelease(r))
		}
	}
	return out, nil
}

// DeleteReleases removes every release record for app, including its save-order tracking.
// Deleting the releases of an app that has none is a no-op, not an error.
func (d *Database) DeleteReleases(ctx context.Context, app string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDeleteReleases]; err != nil {
		return err
	}
	for _, id := range d.order[app] {
		delete(d.byID, id)
	}
	delete(d.order, app)
	return nil
}

// AppEnv returns a copy of the non-secret env store for app. An app with no env yields an
// empty map and no error.
func (d *Database) AppEnv(ctx context.Context, app string) (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAppEnv]; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(d.appEnv[app]))
	for k, v := range d.appEnv[app] {
		out[k] = v
	}
	return out, nil
}

// SetAppEnv upserts one env key for app.
func (d *Database) SetAppEnv(ctx context.Context, app, key, value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetAppEnv]; err != nil {
		return err
	}
	if d.appEnv[app] == nil {
		d.appEnv[app] = make(map[string]string)
	}
	d.appEnv[app][key] = value
	return nil
}

// UnsetAppEnv removes one env key for app. Removing a key that is not set is a no-op.
func (d *Database) UnsetAppEnv(ctx context.Context, app, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpUnsetAppEnv]; err != nil {
		return err
	}
	delete(d.appEnv[app], key)
	return nil
}

// hookKey keys the lifecycle-hook store by (app, environment, phase) — the same key the real
// store's primary key uses, so the fake cannot accidentally be more permissive than Postgres.
func hookKey(app, env string, phase controlplane.HookPhase) string {
	return app + "\x00" + env + "\x00" + string(phase)
}

// AppHook returns the command app runs at phase in env, or nil when no hook is set (ADR-0072 §1).
func (d *Database) AppHook(ctx context.Context, app, env string, phase controlplane.HookPhase) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAppHook]; err != nil {
		return nil, err
	}
	h, ok := d.hooks[hookKey(app, env, phase)]
	if !ok {
		return nil, nil
	}
	return append([]string(nil), h.Command...), nil
}

// AppHooks returns every hook configured for app in env, in phase order. None yields an empty slice.
func (d *Database) AppHooks(ctx context.Context, app, env string) ([]controlplane.Hook, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAppHooks]; err != nil {
		return nil, err
	}
	out := []controlplane.Hook{}
	for _, phase := range controlplane.HookPhases() {
		if h, ok := d.hooks[hookKey(app, env, phase)]; ok {
			h.Command = append([]string(nil), h.Command...)
			out = append(out, h)
		}
	}
	return out, nil
}

// SetAppHook upserts the command app runs at phase in env, replacing any command already there.
func (d *Database) SetAppHook(ctx context.Context, app, env string, phase controlplane.HookPhase, command []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetAppHook]; err != nil {
		return err
	}
	if len(command) == 0 {
		return fmt.Errorf("database: set app hook: empty command")
	}
	d.hooks[hookKey(app, env, phase)] = controlplane.Hook{
		App: app, Environment: env, Phase: phase, Command: append([]string(nil), command...),
	}
	return nil
}

// UnsetAppHook removes app's hook at phase in env. Removing one that is not set is a no-op.
func (d *Database) UnsetAppHook(ctx context.Context, app, env string, phase controlplane.HookPhase) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpUnsetAppHook]; err != nil {
		return err
	}
	delete(d.hooks, hookKey(app, env, phase))
	return nil
}

// DeleteAppHooks removes every hook for app across every environment. Deleting none is a no-op.
func (d *Database) DeleteAppHooks(ctx context.Context, app string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDeleteAppHooks]; err != nil {
		return err
	}
	for key, h := range d.hooks {
		if h.App == app {
			delete(d.hooks, key)
		}
	}
	return nil
}

// SaveProvider upserts a provider by name. It stores only the non-secret registry entry.
func (d *Database) SaveProvider(ctx context.Context, p controlplane.Provider) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSaveProvider]; err != nil {
		return err
	}
	if p.Name == "" {
		return fmt.Errorf("database: save provider: empty name")
	}
	d.providers[p.Name] = cloneProvider(p)
	return nil
}

func (d *Database) Provider(ctx context.Context, name string) (controlplane.Provider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpProvider]; err != nil {
		return controlplane.Provider{}, err
	}
	p, ok := d.providers[name]
	if !ok {
		return controlplane.Provider{}, fmt.Errorf("database: provider %q: %w", name, controlplane.ErrNotFound)
	}
	return cloneProvider(p), nil
}

func (d *Database) Providers(ctx context.Context) ([]controlplane.Provider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpProviders]; err != nil {
		return nil, err
	}
	names := make([]string, 0, len(d.providers))
	for name := range d.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]controlplane.Provider, 0, len(names))
	for _, name := range names {
		out = append(out, cloneProvider(d.providers[name]))
	}
	return out, nil
}

// SaveAddon upserts an add-on by name. It stores only the non-secret registry entry; Ready is
// a live property and is not persisted here.
func (d *Database) SaveAddon(ctx context.Context, a controlplane.AddonInfo) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSaveAddon]; err != nil {
		return err
	}
	if a.Name == "" {
		return fmt.Errorf("database: save addon: empty name")
	}
	a.Ready = false // readiness is never stored
	// A row always carries a label, and a caller that names none is addressing the instance by its
	// own name — which is what an environment's first instance is (ADR-0091 §2), and what every row
	// written before labels existed was backfilled to.
	if a.Label == "" {
		a.Label = a.Name
	}
	d.addons[a.Name] = cloneAddon(a)
	return nil
}

// AddonByLabel returns the instance labelled label in environment env, or ErrNotFound. A label is
// unique within an environment (ADR-0091 §2), which is what makes this a single-row answer.
func (d *Database) AddonByLabel(ctx context.Context, env, label string) (controlplane.AddonInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAddon]; err != nil {
		return controlplane.AddonInfo{}, err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	for _, a := range d.addons {
		if addonEnv(a) == env && addonLabel(a) == label {
			return cloneAddon(a), nil
		}
	}
	return controlplane.AddonInfo{}, fmt.Errorf("database: addon %q in environment %q: %w", label, env, controlplane.ErrNotFound)
}

// AddonsInEnvironment returns the registered instances of type t serving env, label order.
func (d *Database) AddonsInEnvironment(ctx context.Context, t controlplane.AddonType, env string) ([]controlplane.AddonInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAddons]; err != nil {
		return nil, err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	out := []controlplane.AddonInfo{}
	for _, a := range d.addons {
		if a.Type == t && addonEnv(a) == env {
			out = append(out, cloneAddon(a))
		}
	}
	sort.Slice(out, func(i, j int) bool { return addonLabel(out[i]) < addonLabel(out[j]) })
	return out, nil
}

// addonEnv is the environment a stored row serves, with a row written before add-ons were
// per-environment reading as the default one — the only environment it could have been.
func addonEnv(a controlplane.AddonInfo) string {
	if a.Environment == "" {
		return controlplane.DefaultEnvironment
	}
	return a.Environment
}

// addonLabel is what an operator addresses a stored row by, falling back to its cluster name for a
// row written before labels existed.
func addonLabel(a controlplane.AddonInfo) string {
	if a.Label == "" {
		return a.Name
	}
	return a.Label
}

func (d *Database) Addon(ctx context.Context, name string) (controlplane.AddonInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAddon]; err != nil {
		return controlplane.AddonInfo{}, err
	}
	a, ok := d.addons[name]
	if !ok {
		return controlplane.AddonInfo{}, fmt.Errorf("database: addon %q: %w", name, controlplane.ErrNotFound)
	}
	return cloneAddon(a), nil
}

func (d *Database) Addons(ctx context.Context) ([]controlplane.AddonInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAddons]; err != nil {
		return nil, err
	}
	names := make([]string, 0, len(d.addons))
	for name := range d.addons {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]controlplane.AddonInfo, 0, len(names))
	for _, name := range names {
		out = append(out, cloneAddon(d.addons[name]))
	}
	return out, nil
}

func (d *Database) DeleteAddon(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDeleteAddon]; err != nil {
		return err
	}
	if _, ok := d.addons[name]; !ok {
		return fmt.Errorf("database: addon %q: %w", name, controlplane.ErrNotFound)
	}
	delete(d.addons, name)
	return nil
}

// AppendAudit appends one audit row in append order (the append-only log). It deep-copies the
// args map so the store never aliases the caller's map, matching a real database.
func (d *Database) AppendAudit(ctx context.Context, e controlplane.AuditEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAppendAudit]; err != nil {
		return err
	}
	e.ID = int64(len(d.audit) + 1)
	e.Args = cloneStringMap(e.Args)
	d.audit = append(d.audit, e)
	return nil
}

// Audit returns the rows matching filter, newest first, capped by filter.Limit (a default when
// unset). The filter clauses are ANDed.
func (d *Database) Audit(ctx context.Context, filter controlplane.AuditFilter) ([]controlplane.AuditEntry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAudit]; err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	out := make([]controlplane.AuditEntry, 0)
	// Walk newest-first (append order is oldest-first).
	for i := len(d.audit) - 1; i >= 0 && len(out) < limit; i-- {
		e := d.audit[i]
		if filter.App != "" && e.Target != filter.App {
			continue
		}
		if filter.Operation != "" && e.Operation != filter.Operation {
			continue
		}
		if filter.Outcome != "" && e.Outcome != filter.Outcome {
			continue
		}
		e.Args = cloneStringMap(e.Args)
		out = append(out, e)
	}
	return out, nil
}

// AuditRows returns a copy of every appended audit row in append order, for tests asserting on
// what the engine recorded.
func (d *Database) AuditRows() []controlplane.AuditEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]controlplane.AuditEntry, len(d.audit))
	for i, e := range d.audit {
		e.Args = cloneStringMap(e.Args)
		out[i] = e
	}
	return out
}

// RecordBackup persists a new backup row, tracking record order for deterministic listing. An
// existing row with the same ID is overwritten in place.
func (d *Database) RecordBackup(ctx context.Context, b controlplane.Backup) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpRecordBackup]; err != nil {
		return err
	}
	if b.ID == "" {
		return fmt.Errorf("database: record backup: empty ID")
	}
	if _, exists := d.backups[b.ID]; !exists {
		d.backupSeq = append(d.backupSeq, b.ID)
	}
	d.backups[b.ID] = b
	return nil
}

// SetBackupStatus updates a recorded backup's status and size. An unknown id is ErrNotFound.
func (d *Database) SetBackupStatus(ctx context.Context, id string, status controlplane.BackupStatus, sizeBytes int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetBackupStatus]; err != nil {
		return err
	}
	b, ok := d.backups[id]
	if !ok {
		return fmt.Errorf("database: backup %q: %w", id, controlplane.ErrNotFound)
	}
	b.Status = status
	b.SizeBytes = sizeBytes
	// Reaching completed clears any reason left by an earlier attempt, so a successful row never
	// carries a stale explanation of a failure beside it.
	b.FailureReason, b.FailureDetail = "", ""
	d.backups[id] = b
	return nil
}

// FailBackup marks a recorded backup failed and records the closed reason and detail beside it. The
// size is zeroed, so a failed row never carries a length that would read as a partial success. An
// unknown id is ErrNotFound.
func (d *Database) FailBackup(ctx context.Context, id, reason, detail string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpFailBackup]; err != nil {
		return err
	}
	b, ok := d.backups[id]
	if !ok {
		return fmt.Errorf("database: backup %q: %w", id, controlplane.ErrNotFound)
	}
	b.Status = controlplane.BackupFailed
	b.SizeBytes = 0
	b.FailureReason = reason
	b.FailureDetail = detail
	d.backups[id] = b
	return nil
}

// ListBackups returns recorded backups newest first (reverse record order). An empty app lists every
// app's and an empty env every environment's (ADR-0067 §1).
func (d *Database) ListBackups(ctx context.Context, app, env string) ([]controlplane.Backup, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpListBackups]; err != nil {
		return nil, err
	}
	out := make([]controlplane.Backup, 0)
	for i := len(d.backupSeq) - 1; i >= 0; i-- {
		b := d.backups[d.backupSeq[i]]
		if app != "" && b.App != app {
			continue
		}
		if env != "" && b.Environment != env {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// GetBackup returns the backup with the given id, or ErrNotFound.
func (d *Database) GetBackup(ctx context.Context, id string) (controlplane.Backup, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpGetBackup]; err != nil {
		return controlplane.Backup{}, err
	}
	b, ok := d.backups[id]
	if !ok {
		return controlplane.Backup{}, fmt.Errorf("database: backup %q: %w", id, controlplane.ErrNotFound)
	}
	return b, nil
}

// CreateEnvironment registers a named environment, rejecting a duplicate name with an
// ErrInvalid-wrapped error (the name is the primary key), matching the store.
func (d *Database) CreateEnvironment(ctx context.Context, name, namespace string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpCreateEnvironment]; err != nil {
		return err
	}
	if _, exists := d.envs[name]; exists {
		return fmt.Errorf("database: environment %q already exists: %w", name, controlplane.ErrInvalid)
	}
	d.envs[name] = controlplane.Environment{Name: name, Namespace: namespace}
	return nil
}

// ListEnvironments returns the registered environments ordered by name, including the default
// environment `prod` once burrowd's startup ensure has written it (ADR-0067 §2).
func (d *Database) ListEnvironments(ctx context.Context) ([]controlplane.Environment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpListEnvironments]; err != nil {
		return nil, err
	}
	names := make([]string, 0, len(d.envs))
	for name := range d.envs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]controlplane.Environment, 0, len(names))
	for _, name := range names {
		out = append(out, d.envs[name])
	}
	return out, nil
}

// GetEnvironment returns the registered environment with the given name, or ErrNotFound.
func (d *Database) GetEnvironment(ctx context.Context, name string) (controlplane.Environment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpGetEnvironment]; err != nil {
		return controlplane.Environment{}, err
	}
	e, ok := d.envs[name]
	if !ok {
		return controlplane.Environment{}, fmt.Errorf("database: environment %q: %w", name, controlplane.ErrNotFound)
	}
	return e, nil
}

// DeleteEnvironment removes the registered environment with the given name, or ErrNotFound when it
// is not registered, matching the store. The default environment is rejected by the engine first.
func (d *Database) DeleteEnvironment(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDeleteEnvironment]; err != nil {
		return err
	}
	if _, ok := d.envs[name]; !ok {
		return fmt.Errorf("database: environment %q: %w", name, controlplane.ErrNotFound)
	}
	delete(d.envs, name)
	return nil
}

// cloneStringMap deep-copies a string map (nil stays nil) so the fake never aliases a caller's map.
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
