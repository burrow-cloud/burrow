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
	_ controlplane.DatabaseQuerier     = (*Provisioner)(nil)
)

// AppDatabase is one provisioning call: the app whose database was ensured or dropped, and the
// ENVIRONMENT whose instance it happened on. The pair is recorded rather than the app alone because
// the app alone is exactly what was not enough: with one instance per cluster, `web` in staging and
// `web` in production were the same call (issue #339, ADR-0067 §1).
type AppDatabase struct {
	App string
	Env string
	// Instance is the instance the call named — the second half of the same argument, once an
	// environment may hold more than one (ADR-0091 §4). It is the name in the cluster, which is what
	// the seam takes.
	Instance string
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
	// databases maps instance -> app -> present, the fake's stand-in for "this instance holds these
	// databases". Two instances are two maps and can never be one — which is the property ADR-0091 §4
	// needs modelled, since two instances in ONE environment may each hold a database called `web`.
	databases  map[string]map[string]bool
	ensured    []AppDatabase // calls to EnsureAppDatabase, in order
	revoked    []AppDatabase // calls to RevokeAppDatabase, in order
	dropped    []AppDatabase // calls to DropAppDatabase, in order
	attached   map[string][]string
	ensureErr  error
	revokeErr  error
	dropErr    error
	listErr    error
	readURLErr error
	// statements records every AppStatement the engine handed down, in order, so a test can assert
	// what was run, against which database, and under which bounds — including that the statement
	// text was passed through UNMODIFIED, which ADR-0087 §6 requires (Burrow parses nothing).
	statements  []controlplane.AppStatement
	queryResult controlplane.SQLResult
	queryErr    error
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

// SetRevokeError makes RevokeAppDatabase return err (nil clears it).
func (p *Provisioner) SetRevokeError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revokeErr = err
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

// Revoked returns the (app, environment) pairs RevokeAppDatabase was called with, in order.
func (p *Provisioner) Revoked() []AppDatabase {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]AppDatabase(nil), p.revoked...)
}

// Dropped returns the (app, environment) pairs DropAppDatabase was called with, in order.
func (p *Provisioner) Dropped() []AppDatabase {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]AppDatabase(nil), p.dropped...)
}

// Databases returns the app names holding a database on env's DEFAULT instance, sorted — the fake's
// view of what actually exists, as distinct from what was asked for. DatabasesOn names an instance
// outright.
func (p *Provisioner) Databases(env string) []string {
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, env)
	if err != nil {
		instance = env
	}
	return p.DatabasesOn(instance)
}

// DatabasesOn returns the app names holding a database on one instance named outright, sorted.
func (p *Provisioner) DatabasesOn(instance string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.databases[instance]))
	for app := range p.databases[instance] {
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
	return URLForInstance(app, instance)
}

// URLForInstance is the same string for an instance named outright — the form a test asserting a
// SECOND instance's attachment needs, since the second one's name is not derivable from its
// environment (ADR-0091 §2).
func URLForInstance(app, instance string) string {
	return fmt.Sprintf("postgres://app_%s:fakepw@%s.burrow-addons.svc:5432/%s?sslmode=disable", app, instance, app)
}

func (p *Provisioner) EnsureAppDatabase(_ context.Context, app, env, instance string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionTarget(env, instance); err != nil {
		return "", err
	}
	if p.ensureErr != nil {
		return "", p.ensureErr
	}
	p.ensured = append(p.ensured, AppDatabase{App: app, Env: env, Instance: instance})
	if p.databases[instance] == nil {
		p.databases[instance] = map[string]bool{}
	}
	p.databases[instance][app] = true
	return URLForInstance(app, instance), nil
}

// ReadURLFor is the deterministic READ address the fake returns for app in env — the `-ro` sibling
// of URLFor (ADR-0081 §2). It differs from URLFor only in the host, which is the property worth
// asserting: a read address that named the same endpoint as the write one would be the primary
// wearing a second name, and the record rejects exactly that.
func ReadURLFor(app, env string) string {
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, env)
	if err != nil {
		return "invalid-environment"
	}
	return ReadURLForInstance(app, instance)
}

// ReadURLForInstance is ReadURLFor for an instance named outright.
func ReadURLForInstance(app, instance string) string {
	return fmt.Sprintf("postgres://app_%s:fakepw@%s-ro.burrow-addons.svc:5432/%s?sslmode=disable", app, instance, app)
}

// SetReadURLError makes AppReadURL return err (nil clears it), modelling an app whose credential
// cannot be read — the path where a scale-up succeeds and one app is reported stranded rather than
// the whole change failing.
func (p *Provisioner) SetReadURLError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readURLErr = err
}

// AppReadURL returns app's read address on env's instance. It provisions nothing and rotates
// nothing, exactly as the real one does not: an app that has no database here has no read address
// either, which is ErrNotFound rather than a plausible string.
func (p *Provisioner) AppReadURL(_ context.Context, app, env, instance string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionTarget(env, instance); err != nil {
		return "", err
	}
	if p.readURLErr != nil {
		return "", p.readURLErr
	}
	if !p.databases[instance][app] {
		return "", fmt.Errorf("fake: %s has no database on instance %q: %w", app, instance, controlplane.ErrNotFound)
	}
	return ReadURLForInstance(app, instance), nil
}

// SetAttachedApps seeds the apps ListAppDatabases reports for env's DEFAULT instance, modelling apps
// attached to it — the set whose databases a data-deleting add-on removal would destroy. It takes an
// environment because that is what almost every test means; SetAttachedAppsOn names an instance for
// a test about a second one.
func (p *Provisioner) SetAttachedApps(env string, apps ...string) {
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, env)
	if err != nil {
		instance = env
	}
	p.SetAttachedAppsOn(instance, apps...)
}

// SetAttachedAppsOn seeds the apps ListAppDatabases reports for one instance named outright.
func (p *Provisioner) SetAttachedAppsOn(instance string, apps ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attached[instance] = append([]string(nil), apps...)
}

// SetListError makes ListAppDatabases return err (nil clears it), modelling an instance that is
// wedged or gone — the case where removal must still succeed, just without naming who was attached.
func (p *Provisioner) SetListError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listErr = err
}

func (p *Provisioner) ListAppDatabases(_ context.Context, env, instance string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionTarget(env, instance); err != nil {
		return nil, err
	}
	if p.listErr != nil {
		return nil, p.listErr
	}
	return append([]string(nil), p.attached[instance]...), nil
}

// RevokeAppDatabase records the call and leaves the database in place, which is the property the
// real one exists to have (ADR-0090 §1): a test that detaches and re-attaches sees the same database
// still there, so "the data came back" is asserted against the fake's own state rather than assumed.
func (p *Provisioner) RevokeAppDatabase(_ context.Context, app, env, instance string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionTarget(env, instance); err != nil {
		return err
	}
	if p.revokeErr != nil {
		return p.revokeErr
	}
	p.revoked = append(p.revoked, AppDatabase{App: app, Env: env, Instance: instance})
	return nil
}

func (p *Provisioner) DropAppDatabase(_ context.Context, app, env, instance string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionTarget(env, instance); err != nil {
		return err
	}
	if p.dropErr != nil {
		return p.dropErr
	}
	p.dropped = append(p.dropped, AppDatabase{App: app, Env: env, Instance: instance})
	delete(p.databases[instance], app)
	return nil
}

// SetQueryResult makes QueryAppDatabase return res (with the engine's own identity fields left for
// it to fill), modelling whatever the database answered — rows, a command tag, or a SQLError, which
// is an outcome rather than a failure (ADR-0087 §4).
func (p *Provisioner) SetQueryResult(res controlplane.SQLResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queryResult = res
}

// SetQueryError makes QueryAppDatabase return err (nil clears it) — the statement not running at
// all, as distinct from the database refusing it.
func (p *Provisioner) SetQueryError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queryErr = err
}

// Statements returns the statements QueryAppDatabase was called with, in order.
func (p *Provisioner) Statements() []controlplane.AppStatement {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]controlplane.AppStatement(nil), p.statements...)
}

// QueryAppDatabase records the call and answers with the seeded result. It refuses an app with no
// database on that environment's instance, because the real one does: the credential it would
// connect with is the one attach wrote, and an app that was never attached has none. Modelling that
// is what keeps a test from asserting a statement ran against a database that does not exist.
func (p *Provisioner) QueryAppDatabase(_ context.Context, q controlplane.AppStatement) (controlplane.SQLResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateProvisionTarget(q.Env, q.Instance); err != nil {
		return controlplane.SQLResult{}, err
	}
	p.statements = append(p.statements, q)
	if p.queryErr != nil {
		return controlplane.SQLResult{}, p.queryErr
	}
	if !p.databases[q.Instance][q.App] {
		return controlplane.SQLResult{}, fmt.Errorf("fake: %s has no database on the instance %q: %w", q.App, q.Instance, controlplane.ErrNotFound)
	}
	return p.queryResult, nil
}

// validateProvisionTarget refuses an unnamed environment or instance exactly as the real provisioner
// does, so a caller that forgets either fails against the fake too rather than passing in tests and
// landing on another server's data in production (ADR-0067 §1, ADR-0091 §4).
func validateProvisionTarget(env, instance string) error {
	if env == "" {
		return fmt.Errorf("fake: provisioning needs an environment: %w", controlplane.ErrInvalid)
	}
	if instance == "" {
		return fmt.Errorf("fake: provisioning needs an instance: %w", controlplane.ErrInvalid)
	}
	return nil
}
