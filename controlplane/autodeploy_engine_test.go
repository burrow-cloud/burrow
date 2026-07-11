// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// newEngineWithRegistry builds an engine wired with a fake registry so the enriched auto-deploy read
// path (AutoDeployStatus) can be exercised against a known tag set, returning the fake database so a
// test can seed a running release.
func newEngineWithRegistry(t *testing.T, reg cp.RegistryClient) (*cp.Engine, *fake.Database) {
	t.Helper()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		RegistryClient: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, d
}

// seedRunningRelease records a deployed release for app at image so LatestRelease returns it.
func seedRunningRelease(t *testing.T, d *fake.Database, app, image string) {
	t.Helper()
	if err := d.SaveRelease(context.Background(), cp.Release{ID: app + "-1", App: app, Image: image, Status: cp.ReleaseDeployed}); err != nil {
		t.Fatalf("SaveRelease: %v", err)
	}
}

// TestAutoDeployStatusReportsTargetAndUpgrade proves AutoDeployStatus lists the registry anonymously,
// threads the running tag and available tags through ResolveAutoDeploy, and reports the within-level
// target and the higher available upgrade above the cap (ADR-0052 §2/§3).
func TestAutoDeployStatusReportsTargetAndUpgrade(t *testing.T) {
	ctx := context.Background()
	reg := fake.NewRegistry()
	reg.SetTags("1.2.5", "1.2.6", "1.3.0", "2.0.0")
	e, d := newEngineWithRegistry(t, reg)
	seedRunningRelease(t, d, "web", "ghcr.io/u/web:1.2.5")

	// Default level is minor: 1.3.0 is the highest within the current major, 2.0.0 is the held upgrade.
	st, err := e.AutoDeployStatus(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeployStatus: %v", err)
	}
	if !st.Checked {
		t.Fatalf("Checked = false, want true; note=%q", st.Note)
	}
	if st.Current != "1.2.5" || st.Target != "1.3.0" || st.Upgrade != "2.0.0" {
		t.Errorf("status = current %q target %q upgrade %q, want 1.2.5 / 1.3.0 / 2.0.0", st.Current, st.Target, st.Upgrade)
	}
	if st.Repository != "ghcr.io/u/web" {
		t.Errorf("Repository = %q, want ghcr.io/u/web", st.Repository)
	}
	// The read path lists anonymously in this phase, against the current image reference.
	if got := reg.LastAuth(); got != (cp.RegistryAuth{}) {
		t.Errorf("registry auth = %+v, want zero-value (anonymous)", got)
	}
	if got := reg.LastRef(); got != "ghcr.io/u/web:1.2.5" {
		t.Errorf("registry ref = %q, want ghcr.io/u/web:1.2.5", got)
	}
}

// TestAutoDeployStatusUpToDate reports no target and no upgrade when the running version is already
// the newest, with the check still marked as run.
func TestAutoDeployStatusUpToDate(t *testing.T) {
	ctx := context.Background()
	reg := fake.NewRegistry()
	reg.SetTags("1.0.0", "1.2.0", "1.2.5")
	e, d := newEngineWithRegistry(t, reg)
	seedRunningRelease(t, d, "web", "ghcr.io/u/web:1.2.5")

	st, err := e.AutoDeployStatus(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeployStatus: %v", err)
	}
	if !st.Checked || st.Target != "" || st.Upgrade != "" {
		t.Errorf("status = checked %v target %q upgrade %q, want checked / no target / no upgrade", st.Checked, st.Target, st.Upgrade)
	}
}

// TestAutoDeployStatusDegradesOnRegistryError proves a registry failure degrades to the level plus a
// note without erroring the call, keeping the path independent of registry reachability (ADR-0040).
func TestAutoDeployStatusDegradesOnRegistryError(t *testing.T) {
	ctx := context.Background()
	reg := fake.NewRegistry()
	reg.SetError(errors.New("registry unreachable"))
	e, d := newEngineWithRegistry(t, reg)
	seedRunningRelease(t, d, "web", "ghcr.io/u/web:1.2.5")

	st, err := e.AutoDeployStatus(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeployStatus returned an error on registry failure, want graceful degrade: %v", err)
	}
	if st.Checked {
		t.Errorf("Checked = true, want false on registry failure")
	}
	if st.Note == "" {
		t.Errorf("Note is empty, want a reason the check could not run")
	}
	if st.Current != "1.2.5" {
		t.Errorf("Current = %q, want 1.2.5 (still known from the running release)", st.Current)
	}
	if st.Level != cp.DefaultAutoDeployLevel {
		t.Errorf("Level = %q, want the default (minor)", st.Level)
	}
}

// TestAutoDeployStatusDegradesWithoutRegistry proves an unwired registry seam degrades cleanly.
func TestAutoDeployStatusDegradesWithoutRegistry(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive()) // newEngine wires no registry
	seedRunningRelease(t, d, "web", "ghcr.io/u/web:1.2.5")

	st, err := e.AutoDeployStatus(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeployStatus: %v", err)
	}
	if st.Checked || st.Note == "" {
		t.Errorf("status = checked %v note %q, want not-checked with a note", st.Checked, st.Note)
	}
	if st.Current != "1.2.5" {
		t.Errorf("Current = %q, want 1.2.5", st.Current)
	}
}

// TestAutoDeployStatusDegradesWithoutRunningRelease proves an app with no running release degrades
// (no version to compare against) rather than erroring.
func TestAutoDeployStatusDegradesWithoutRunningRelease(t *testing.T) {
	ctx := context.Background()
	reg := fake.NewRegistry()
	reg.SetTags("1.0.0", "1.1.0")
	e, _ := newEngineWithRegistry(t, reg)

	st, err := e.AutoDeployStatus(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeployStatus: %v", err)
	}
	if st.Checked || st.Note == "" || st.Current != "" {
		t.Errorf("status = checked %v note %q current %q, want not-checked with a note and no current", st.Checked, st.Note, st.Current)
	}
	if reg.Calls() != 0 {
		t.Errorf("registry called %d times with no running release, want 0", reg.Calls())
	}
}

// TestAutoDeployStatusNonSemverCurrent proves a non-semver running tag degrades without even calling
// the registry, since there is no basis to compute an upgrade (ADR-0052 §4).
func TestAutoDeployStatusNonSemverCurrent(t *testing.T) {
	ctx := context.Background()
	reg := fake.NewRegistry()
	reg.SetTags("1.0.0", "1.1.0")
	e, d := newEngineWithRegistry(t, reg)
	seedRunningRelease(t, d, "web", "ghcr.io/u/web:latest")

	st, err := e.AutoDeployStatus(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeployStatus: %v", err)
	}
	if st.Checked || st.Note == "" || st.Current != "latest" {
		t.Errorf("status = checked %v note %q current %q, want not-checked note current=latest", st.Checked, st.Note, st.Current)
	}
	if reg.Calls() != 0 {
		t.Errorf("registry called %d times for a non-semver current tag, want 0", reg.Calls())
	}
}

// TestAutoDeployDefaultAndSet covers the level lifecycle through the engine: an app with no stored
// level reads the default (minor), a set is reflected on the next read, and an invalid level is
// rejected as ErrInvalid (ADR-0052 §2).
func TestAutoDeployDefaultAndSet(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	// A brand-new app has no stored row, so it reads the built-in default.
	got, err := e.AutoDeploy(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeploy: %v", err)
	}
	if got != cp.DefaultAutoDeployLevel {
		t.Fatalf("default level = %q, want %q", got, cp.DefaultAutoDeployLevel)
	}

	// A set is reflected on the next read.
	if err := e.SetAutoDeploy(ctx, "web", "", cp.AutoDeployOff); err != nil {
		t.Fatalf("SetAutoDeploy: %v", err)
	}
	got, err = e.AutoDeploy(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeploy after set: %v", err)
	}
	if got != cp.AutoDeployOff {
		t.Fatalf("level after set = %q, want off", got)
	}

	// An invalid level is rejected as ErrInvalid and does not change the stored value.
	if err := e.SetAutoDeploy(ctx, "web", "", cp.AutoDeployLevel("sometimes")); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("invalid level err = %v, want ErrInvalid", err)
	}
	if got, _ := e.AutoDeploy(ctx, "web", ""); got != cp.AutoDeployOff {
		t.Fatalf("level after rejected set = %q, want off (unchanged)", got)
	}

	// An invalid app name is rejected as ErrInvalid before any store access.
	if _, err := e.AutoDeploy(ctx, "Bad Name", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("invalid app name err = %v, want ErrInvalid", err)
	}
}

// TestAutoDeployPerEnvironment proves the level is keyed per environment: prod and the default
// environment carry independent levels, and an unknown environment is a clear ErrNotFound on both the
// read and the write (ADR-0052 §2).
func TestAutoDeployPerEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	// prod at patch leaves the default environment at its default (minor).
	if err := e.SetAutoDeploy(ctx, "web", "prod", cp.AutoDeployPatch); err != nil {
		t.Fatalf("SetAutoDeploy prod: %v", err)
	}
	if got, _ := e.AutoDeploy(ctx, "web", "prod"); got != cp.AutoDeployPatch {
		t.Fatalf("prod level = %q, want patch", got)
	}
	if got, _ := e.AutoDeploy(ctx, "web", "default"); got != cp.DefaultAutoDeployLevel {
		t.Fatalf("default env level = %q, want %q", got, cp.DefaultAutoDeployLevel)
	}

	// An unknown environment is ErrNotFound on both read and write.
	if _, err := e.AutoDeploy(ctx, "web", "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("read unknown env err = %v, want ErrNotFound", err)
	}
	if err := e.SetAutoDeploy(ctx, "web", "ghost", cp.AutoDeployMajor); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("set unknown env err = %v, want ErrNotFound", err)
	}
}

// TestAutoDeploySetRefusesAmbiguousEnvironment confirms setting the level with no environment named is
// refused once more than one environment is registered, like every other per-app mutation (ADR-0047).
func TestAutoDeploySetRefusesAmbiguousEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	err := e.SetAutoDeploy(ctx, "web", "", cp.AutoDeployMajor)
	if _, ok := cp.AsAmbiguousEnvironment(err); !ok {
		t.Fatalf("set with ambiguous env = %v, want AmbiguousEnvironmentError", err)
	}
}
