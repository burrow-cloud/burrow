// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// newEnvEngine builds an engine with a known app namespace so the synthesized default environment's
// namespace is assertable, returning the engine and its database.
func newEnvEngine(t *testing.T, appNamespace string) (*cp.Engine, *fake.Database) {
	t.Helper()
	d := fake.NewDatabase()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		AppNamespace: appNamespace,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, d
}

func TestAddEnvironmentValidation(t *testing.T) {
	e, _ := newEnvEngine(t, "burrow-apps")
	ctx := context.Background()

	// A valid name + namespace registers.
	env, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging")
	if err != nil {
		t.Fatalf("AddEnvironment(staging): %v", err)
	}
	if env.Name != "staging" || env.Namespace != "burrow-apps-staging" || env.Default {
		t.Errorf("registered environment = %+v", env)
	}

	cases := []struct {
		name, ns string
		why      string
	}{
		{"Staging", "ns", "uppercase is not a DNS-1123 label"},
		{"stg_1", "ns", "underscore is not a DNS-1123 label"},
		{"default", "ns", "the retired name of the first environment is reserved (ADR-0067 §2)"},
		{"prod", "ns", "install already created prod (ADR-0067 §2)"},
		{"dev", "", "empty namespace"},
	}
	for _, c := range cases {
		if _, err := e.AddEnvironment(ctx, c.name, c.ns); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("AddEnvironment(%q,%q) err = %v, want ErrInvalid (%s)", c.name, c.ns, err, c.why)
		}
	}

	// A duplicate name is rejected (ErrInvalid).
	if _, err := e.AddEnvironment(ctx, "staging", "other-ns"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("duplicate AddEnvironment err = %v, want ErrInvalid", err)
	}
}

// TestReservedEnvironmentNamesAreRefused pins the exported set to the refusal it claims to describe,
// which is the only thing that makes the accessor worth reading. A caller that refuses BEFORE it
// provisions — `burrow env add`, which creates the environment's namespace and RBAC first, or an
// operator embedding the engine — asks ReservedEnvironmentNames what to refuse and never reaches
// AddEnvironment for those names. If the two ever disagree, that caller creates cluster state for a
// request the control plane was always going to reject, and nothing errors where the state is made.
// So: every name the accessor reports is actually rejected, and a name it does not report is
// accepted — a set that grew to swallow ordinary names would pass the first half alone.
func TestReservedEnvironmentNamesAreRefused(t *testing.T) {
	e, _ := newEnvEngine(t, "burrow-apps")
	ctx := context.Background()

	reserved := cp.ReservedEnvironmentNames()
	if len(reserved) == 0 {
		t.Fatal("ReservedEnvironmentNames is empty; at least the default environment is reserved (ADR-0067 §2)")
	}
	if !slices.Contains(reserved, cp.DefaultEnvironment) {
		t.Errorf("ReservedEnvironmentNames = %q, missing the default environment %q", reserved, cp.DefaultEnvironment)
	}
	// The retired `default` stays reserved so a re-added `default` cannot collide with a row
	// migration 00018 rewrote (ADR-0067 §2). It is named literally here on purpose: this is the
	// assertion that the set has not quietly shrunk, so it must not read the set to check it.
	if !slices.Contains(reserved, "default") {
		t.Errorf("ReservedEnvironmentNames = %q, missing the retired `default` (ADR-0067 §2)", reserved)
	}

	for _, name := range reserved {
		if _, err := e.AddEnvironment(ctx, name, "burrow-apps-"+name); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("AddEnvironment(%q) err = %v, want ErrInvalid: the accessor reports it reserved", name, err)
		}
	}

	// A name the accessor does NOT report registers, so "refuses everything" cannot pass above.
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Errorf("AddEnvironment(staging): %v, want success (staging is not reserved)", err)
	}

	// The returned slice is the caller's own: mutating it does not edit the package's set.
	reserved[0] = "clobbered"
	if slices.Contains(cp.ReservedEnvironmentNames(), "clobbered") {
		t.Error("ReservedEnvironmentNames returns the backing slice; a caller can rewrite the reserved set")
	}
}

func TestListEnvironmentsDefaultFirst(t *testing.T) {
	e, _ := newEnvEngine(t, "burrow-apps")
	ctx := context.Background()

	// With nothing registered, the default environment is still listed — synthesized against the
	// engine's app namespace until the startup ensure writes its row (ADR-0067 §2).
	envs, err := e.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 1 || envs[0].Name != cp.DefaultEnvironment || !envs[0].Default || envs[0].Namespace != "burrow-apps" {
		t.Fatalf("default-only listing = %+v", envs)
	}

	// Register two out of order; the default stays first, registered ones follow in name order.
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("add staging: %v", err)
	}
	if _, err := e.AddEnvironment(ctx, "dev", "burrow-apps-dev"); err != nil {
		t.Fatalf("add dev: %v", err)
	}
	envs, err = e.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	gotNames := []string{}
	for _, en := range envs {
		gotNames = append(gotNames, en.Name)
	}
	want := []string{cp.DefaultEnvironment, "dev", "staging"}
	if len(gotNames) != len(want) {
		t.Fatalf("names = %v, want %v", gotNames, want)
	}
	for i, w := range want {
		if gotNames[i] != w {
			t.Errorf("name[%d] = %q, want %q (all: %v)", i, gotNames[i], w, gotNames)
		}
	}
	if !envs[0].Default || envs[1].Default || envs[2].Default {
		t.Errorf("only the first (default) environment should be marked default: %+v", envs)
	}
}

// TestRemoveEnvironment covers the inverse of AddEnvironment: a registered environment can be
// unregistered (and re-added), the implicit default is refused, and an unknown name is ErrNotFound.
func TestRemoveEnvironment(t *testing.T) {
	e, _ := newEnvEngine(t, "burrow-apps")
	ctx := context.Background()

	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("add staging: %v", err)
	}

	// Removing a registered environment leaves only the default environment.
	if err := e.RemoveEnvironment(ctx, "staging"); err != nil {
		t.Fatalf("RemoveEnvironment(staging): %v", err)
	}
	envs, err := e.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 1 || envs[0].Name != cp.DefaultEnvironment {
		t.Fatalf("after remove, listing = %+v, want default only", envs)
	}

	// The environment install created cannot be removed: every unqualified operation resolves to it.
	if err := e.RemoveEnvironment(ctx, cp.DefaultEnvironment); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("RemoveEnvironment(%s) err = %v, want ErrInvalid", cp.DefaultEnvironment, err)
	}
	// An empty name is invalid too.
	if err := e.RemoveEnvironment(ctx, ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("RemoveEnvironment(\"\") err = %v, want ErrInvalid", err)
	}
	// Removing an unregistered name is ErrNotFound (already removed above).
	if err := e.RemoveEnvironment(ctx, "staging"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("RemoveEnvironment(unknown) err = %v, want ErrNotFound", err)
	}

	// A removed environment can be re-added.
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Errorf("re-add staging after remove: %v", err)
	}
}

// TestListEnvironmentsDefaultsNamespace confirms an engine with no configured app namespace falls
// back to "default" for the implicit environment, matching the kube Adapter's default.
func TestListEnvironmentsDefaultsNamespace(t *testing.T) {
	e, _ := newEnvEngine(t, "")
	envs, err := e.ListEnvironments(context.Background())
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if envs[0].Namespace != "default" {
		t.Errorf("default environment namespace = %q, want %q", envs[0].Namespace, "default")
	}
}
