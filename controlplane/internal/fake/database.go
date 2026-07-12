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
	autoDeploy map[string]map[string]controlplane.AutoDeployLevel // app -> env -> level
	reason     map[string]map[string]string                       // app -> env -> disable reason
	audit      []controlplane.AuditEntry                          // append-only, in append order
	backups    map[string]controlplane.Backup
	backupSeq  []string                            // backup IDs in record order, for deterministic newest-first listing
	envs       map[string]controlplane.Environment // registered environments by name
	errs       map[Op]error
	policy     controlplane.Policy
}

// NewDatabase returns an empty fake database with the default guardrail policy.
func NewDatabase() *Database {
	return &Database{
		byID:       make(map[string]controlplane.Release),
		order:      make(map[string][]string),
		providers:  make(map[string]controlplane.Provider),
		addons:     make(map[string]controlplane.AddonInfo),
		appEnv:     make(map[string]map[string]string),
		autoDeploy: make(map[string]map[string]controlplane.AutoDeployLevel),
		reason:     make(map[string]map[string]string),
		backups:    make(map[string]controlplane.Backup),
		envs:       make(map[string]controlplane.Environment),
		errs:       make(map[Op]error),
		policy:     controlplane.DefaultPolicy(),
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

// AutoDeployLevel returns app's auto-deploy level in env, or DefaultAutoDeployLevel when none is
// set — a missing configuration resolves to the default, matching the store (ADR-0052 §2).
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
	d.addons[a.Name] = cloneAddon(a)
	return nil
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
	d.backups[id] = b
	return nil
}

// ListBackups returns recorded backups newest first (reverse record order). An empty app lists all.
func (d *Database) ListBackups(ctx context.Context, app string) ([]controlplane.Backup, error) {
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

// ListEnvironments returns the registered environments ordered by name. The synthesized `default`
// environment is not stored here; the engine prepends it.
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
