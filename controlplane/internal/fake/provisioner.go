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

var (
	_ controlplane.DatabaseProvisioner = (*Provisioner)(nil)
	_ controlplane.AppDatabaseLister   = (*Provisioner)(nil)
)

// AppDatabase is one provisioning call: the app whose database was ensured or dropped, and the
// ENVIRONMENT whose instance it happened on. The pair is recorded rather than the app alone because
// the app alone is exactly what was not enough: with one instance per cluster, `web` in staging and
// `web` in production were the same call (issue #339, ADR-0067 §1).
type AppDatabase struct {
	App string
	Env string
}

// Provisioner is an in-memory controlplane.DatabaseProvisioner. It models ONE INSTANCE PER
// ENVIRONMENT (ADR-0067 §1): databases are held per environment, so provisioning `web` in two
// environments yields two databases with two distinct connection strings, and the second call cannot
// adopt the first's. It records the (app, environment) pairs it provisioned and returns a
// deterministic connection string per pair, so an attach test can assert the engine threaded the URL
// into the secret path without standing up Postgres. Errors can be injected to exercise the failure
// path.
type Provisioner struct {
	mu sync.Mutex
	// databases maps environment -> app -> present, the fake's stand-in for "this environment's
	// instance holds these databases". Two environments are two maps and can never be one.
	databases map[string]map[string]bool
	ensured   []AppDatabase // calls to EnsureAppDatabase, in order
	dropped   []AppDatabase // calls to DropAppDatabase, in order
	attached  map[string][]string
	ensureErr error
	dropErr   error
	listErr   error
}

// NewProvisioner returns an empty fake provisioner.
func NewProvisioner() *Provisioner {
	return &Provisioner{databases: map[string]map[string]bool{}, attached: map[string][]string{}}
}

// SetEnsureError makes EnsureAppDatabase return err (nil clears it).
func (p *Provisioner) SetEnsureError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureErr = err
}

// SetDropError makes DropAppDatabase return err (nil clears it).
func (p *Provisioner) SetDropError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropErr = err
}

// Ensured returns the (app, environment) pairs EnsureAppDatabase was called with, in order.
func (p *Provisioner) Ensured() []AppDatabase {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]AppDatabase(nil), p.ensured...)
}

// Dropped returns the (app, environment) pairs DropAppDatabase was called with, in order.
func (p *Provisioner) Dropped() []AppDatabase {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]AppDatabase(nil), p.dropped...)
}

// Databases returns the app names holding a database on env's instance, sorted — the fake's view of
// what actually exists, as distinct from what was asked for.
func (p *Provisioner) Databases(env string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.databases[env]))
	for app := range p.databases[env] {
		out = append(out, app)
	}
	sort.Strings(out)
	return out
}

// URLFor is the deterministic connection string the fake returns for app in env — exposed so a test
// can assert the engine wrote exactly this value into the secret without the value leaking
// elsewhere. It embeds the environment's INSTANCE host (controlplane.AddonInstanceName), so two
// environments produce two different strings for the same app: the connection string is the visible
// evidence of which server the app was pointed at.
func URLFor(app, env string) string {
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, env)
	if err != nil {
		// A fake never invents a plausible-looking URL for an environment the real provisioner would
		// have refused; the marker fails a comparison loudly instead.
		return "invalid-environment"
	}
	return fmt.Sprintf("postgres://app_%s:fakepw@%s.burrow-addons.svc:5432/%s?sslmode=disable", app, instance, app)
}

func (p *Provisioner) EnsureAppDatabase(_ context.Context, app, env string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionEnv(env); err != nil {
		return "", err
	}
	if p.ensureErr != nil {
		return "", p.ensureErr
	}
	p.ensured = append(p.ensured, AppDatabase{App: app, Env: env})
	if p.databases[env] == nil {
		p.databases[env] = map[string]bool{}
	}
	p.databases[env][app] = true
	return URLFor(app, env), nil
}

// SetAttachedApps seeds the apps ListAppDatabases reports for env, modelling apps attached to that
// environment's instance — the set whose databases a data-deleting add-on removal would destroy.
func (p *Provisioner) SetAttachedApps(env string, apps ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attached[env] = append([]string(nil), apps...)
}

// SetListError makes ListAppDatabases return err (nil clears it), modelling an instance that is
// wedged or gone — the case where removal must still succeed, just without naming who was attached.
func (p *Provisioner) SetListError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listErr = err
}

func (p *Provisioner) ListAppDatabases(_ context.Context, env string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionEnv(env); err != nil {
		return nil, err
	}
	if p.listErr != nil {
		return nil, p.listErr
	}
	return append([]string(nil), p.attached[env]...), nil
}

func (p *Provisioner) DropAppDatabase(_ context.Context, app, env string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionEnv(env); err != nil {
		return err
	}
	if p.dropErr != nil {
		return p.dropErr
	}
	p.dropped = append(p.dropped, AppDatabase{App: app, Env: env})
	delete(p.databases[env], app)
	return nil
}

// validateProvisionEnv refuses an unnamed environment exactly as the real provisioner does, so a
// caller that forgets the environment fails against the fake too rather than passing in tests and
// landing on another environment's data in production (ADR-0067 §1).
func validateProvisionEnv(env string) error {
	if env == "" {
		return fmt.Errorf("fake: provisioning needs an environment: %w", controlplane.ErrInvalid)
	}
	return nil
}
