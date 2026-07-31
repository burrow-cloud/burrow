// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// The declared health endpoint's store tests (ADR-0076 §5). They scope every app name to the test's
// own name so they are safe against the shared database the suite runs on.

func healthApp(t *testing.T, suffix string) string {
	t.Helper()
	return strings.ToLower(t.Name()) + "-" + suffix
}

// TestStoreHealthEndpointRoundTrip: declare, read back, redeclare, unset. The read of an app that
// declared nothing is the important row — it must be the zero value and NOT an error, because the
// deploy path reads it on every apply and "no endpoint" is where every app starts.
func TestStoreHealthEndpointRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := healthApp(t, "web")
	at := time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC)

	ep, err := s.HealthEndpoint(ctx, app, "prod")
	if err != nil {
		t.Fatalf("HealthEndpoint (undeclared) returned an error, want the zero value: %v", err)
	}
	if ep.Declared() {
		t.Errorf("HealthEndpoint (undeclared) = %+v, want the zero value", ep)
	}

	if err := s.SetHealthEndpoint(ctx, cp.HealthEndpoint{App: app, Environment: "prod", Path: "/healthz", Port: 8080, UpdatedAt: at}); err != nil {
		t.Fatalf("SetHealthEndpoint: %v", err)
	}
	ep, err = s.HealthEndpoint(ctx, app, "prod")
	if err != nil {
		t.Fatalf("HealthEndpoint: %v", err)
	}
	if ep.Path != "/healthz" || ep.Port != 8080 || !ep.UpdatedAt.Equal(at) {
		t.Errorf("HealthEndpoint = %+v, want /healthz:8080 at %s", ep, at)
	}

	// Redeclaring upserts rather than duplicating.
	if err := s.SetHealthEndpoint(ctx, cp.HealthEndpoint{App: app, Environment: "prod", Path: "/ready", UpdatedAt: at.Add(time.Hour)}); err != nil {
		t.Fatalf("SetHealthEndpoint (redeclare): %v", err)
	}
	ep, _ = s.HealthEndpoint(ctx, app, "prod")
	if ep.Path != "/ready" || ep.Port != 0 {
		t.Errorf("HealthEndpoint after redeclare = %+v, want /ready with no port", ep)
	}

	// Unsetting is idempotent — it is what a user reaches for when unsure what is set.
	for i := 0; i < 2; i++ {
		if err := s.UnsetHealthEndpoint(ctx, app, "prod"); err != nil {
			t.Fatalf("UnsetHealthEndpoint %d: %v", i, err)
		}
	}
	if ep, _ := s.HealthEndpoint(ctx, app, "prod"); ep.Declared() {
		t.Errorf("HealthEndpoint after unset = %+v, want the zero value", ep)
	}
}

// TestStoreHealthEndpointIsPerEnvironment: the same app in two environments can serve on two
// different ports, so the endpoint is keyed by (app, environment) and one does not read the other's.
func TestStoreHealthEndpointIsPerEnvironment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := healthApp(t, "web")
	at := time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC)

	if err := s.SetHealthEndpoint(ctx, cp.HealthEndpoint{App: app, Environment: "prod", Path: "/healthz", Port: 8080, UpdatedAt: at}); err != nil {
		t.Fatalf("SetHealthEndpoint(prod): %v", err)
	}
	if err := s.SetHealthEndpoint(ctx, cp.HealthEndpoint{App: app, Environment: "staging", Path: "/ready", Port: 3000, UpdatedAt: at}); err != nil {
		t.Fatalf("SetHealthEndpoint(staging): %v", err)
	}
	prod, _ := s.HealthEndpoint(ctx, app, "prod")
	staging, _ := s.HealthEndpoint(ctx, app, "staging")
	if prod.Path != "/healthz" || prod.Port != 8080 {
		t.Errorf("prod = %+v, want /healthz:8080", prod)
	}
	if staging.Path != "/ready" || staging.Port != 3000 {
		t.Errorf("staging = %+v, want /ready:3000", staging)
	}

	// Unsetting one leaves the other alone.
	if err := s.UnsetHealthEndpoint(ctx, app, "staging"); err != nil {
		t.Fatalf("UnsetHealthEndpoint(staging): %v", err)
	}
	if ep, _ := s.HealthEndpoint(ctx, app, "prod"); ep.Path != "/healthz" {
		t.Errorf("prod = %+v after unsetting staging, want it untouched", ep)
	}

	// A teardown clears every environment's row at once.
	if err := s.DeleteHealthEndpoints(ctx, app); err != nil {
		t.Fatalf("DeleteHealthEndpoints: %v", err)
	}
	if ep, _ := s.HealthEndpoint(ctx, app, "prod"); ep.Declared() {
		t.Errorf("prod = %+v after the app was deleted, want the zero value", ep)
	}
}

// TestStoreExposureTargetedRead: the readiness default's input. An app that is not published reads
// as ErrNotFound, which the engine treats as "no port known" and therefore "no probe" — the
// conservative answer (ADR-0076 §3).
func TestStoreExposureTargetedRead(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := healthApp(t, "web")
	at := time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC)

	if _, err := s.Exposure(ctx, app, "prod"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Exposure (unpublished) err = %v, want ErrNotFound", err)
	}
	if err := s.RecordExposure(ctx, cp.Exposure{App: app, Environment: "prod", Host: app + ".example.com", Port: 8080, CreatedAt: at}); err != nil {
		t.Fatalf("RecordExposure: %v", err)
	}
	ex, err := s.Exposure(ctx, app, "prod")
	if err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if ex.Port != 8080 || ex.Host != app+".example.com" {
		t.Errorf("Exposure = %+v, want port 8080", ex)
	}
	if _, err := s.Exposure(ctx, app, "staging"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Exposure(staging) err = %v, want ErrNotFound: an exposure is per environment", err)
	}
}
