// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestStoreAddonsRoundTripAndUpsert(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	name := t.Name() + "-logs"

	a := cp.AddonInfo{
		Name:         name,
		Type:         cp.AddonLogs,
		Environment:  cp.DefaultEnvironment,
		Mode:         "installed",
		Backend:      "victorialogs",
		Image:        "victoria-logs:test",
		Endpoint:     name + ".burrow-addons.svc:9428",
		Capabilities: []string{"logs"},
		Ready:        true, // live property — must NOT be persisted
		CreatedAt:    time.Date(2026, 6, 25, 1, 2, 3, 0, time.UTC),
	}
	if err := s.SaveAddon(ctx, a); err != nil {
		t.Fatalf("SaveAddon: %v", err)
	}

	got, err := s.Addon(ctx, name)
	if err != nil {
		t.Fatalf("Addon: %v", err)
	}
	if got.Type != cp.AddonLogs || got.Mode != "installed" || got.Backend != "victorialogs" {
		t.Errorf("round trip: type=%q mode=%q backend=%q", got.Type, got.Mode, got.Backend)
	}
	if got.Endpoint != a.Endpoint || len(got.Capabilities) != 1 || got.Capabilities[0] != "logs" {
		t.Errorf("round trip: endpoint=%q caps=%v", got.Endpoint, got.Capabilities)
	}
	if got.Ready {
		t.Errorf("Ready = true, want false — readiness is never persisted")
	}
	if got.Environment != cp.DefaultEnvironment {
		t.Errorf("Environment = %q, want %q — the row records which environment the instance serves (ADR-0067 §1)", got.Environment, cp.DefaultEnvironment)
	}

	// A second environment's instance of the SAME type is a SEPARATE row: the registry is keyed by
	// instance name, and the instance name is derived from the environment (ADR-0067 §1), so the two
	// coexist rather than one upserting over the other.
	stagingName, err := cp.AddonInstanceName(cp.AddonLogs, "staging-"+t.Name())
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	staging := a
	staging.Name = stagingName
	staging.Environment = "staging-" + t.Name()
	if err := s.SaveAddon(ctx, staging); err != nil {
		t.Fatalf("SaveAddon staging: %v", err)
	}
	if got, gerr := s.Addon(ctx, stagingName); gerr != nil || got.Environment != staging.Environment {
		t.Errorf("staging row = %+v, err %v; want environment %q", got, gerr, staging.Environment)
	}
	if got, _ := s.Addon(ctx, name); got.Environment != cp.DefaultEnvironment {
		t.Errorf("the default environment's row was overwritten by the staging install: environment = %q", got.Environment)
	}
	t.Cleanup(func() { _ = s.DeleteAddon(context.Background(), stagingName) })
	if !got.CreatedAt.Equal(a.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, a.CreatedAt)
	}

	// Upsert by name: re-installing with a new image overwrites.
	a.Image = "victoria-logs:next"
	if err := s.SaveAddon(ctx, a); err != nil {
		t.Fatalf("SaveAddon upsert: %v", err)
	}
	if got, _ := s.Addon(ctx, name); got.Image != "victoria-logs:next" {
		t.Errorf("upsert image = %q, want victoria-logs:next", got.Image)
	}

	// Addons lists it (among any others in the shared database).
	all, err := s.Addons(ctx)
	if err != nil {
		t.Fatalf("Addons: %v", err)
	}
	found := false
	for _, q := range all {
		if q.Name == name {
			found = true
		}
	}
	if !found {
		t.Errorf("Addons did not include %q", name)
	}

	// Delete removes it; deleting a missing add-on is ErrNotFound.
	if err := s.DeleteAddon(ctx, name); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := s.Addon(ctx, name); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("after delete, Addon err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAddon(ctx, name); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("delete missing addon err = %v, want ErrNotFound", err)
	}

	// An unknown add-on is ErrNotFound.
	if _, err := s.Addon(ctx, t.Name()+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("missing addon err = %v, want ErrNotFound", err)
	}
}
