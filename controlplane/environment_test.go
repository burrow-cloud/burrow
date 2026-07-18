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
		{"default", "ns", "default is reserved"},
		{"prod", "", "empty namespace"},
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

func TestListEnvironmentsDefaultFirst(t *testing.T) {
	e, _ := newEnvEngine(t, "burrow-apps")
	ctx := context.Background()

	// With nothing registered, only the implicit default is listed, with the engine's app namespace.
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
	if _, err := e.AddEnvironment(ctx, "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("add prod: %v", err)
	}
	envs, err = e.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	gotNames := []string{}
	for _, en := range envs {
		gotNames = append(gotNames, en.Name)
	}
	want := []string{"default", "prod", "staging"}
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

	// Removing a registered environment leaves only the implicit default.
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

	// The implicit default cannot be removed (it is synthesized, never stored).
	if err := e.RemoveEnvironment(ctx, cp.DefaultEnvironment); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("RemoveEnvironment(default) err = %v, want ErrInvalid", err)
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
