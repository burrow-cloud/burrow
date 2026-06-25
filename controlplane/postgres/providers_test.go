// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestStoreProvidersRoundTripAndUpsert(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	name := t.Name() + "-do"

	p := cp.Provider{
		Name:         name,
		Type:         cp.ProviderDigitalOcean,
		Capabilities: []cp.Capability{cp.CapabilityDNS},
		SecretKey:    "do_token",
		CreatedAt:    time.Date(2026, 6, 25, 1, 2, 3, 0, time.UTC),
	}
	if err := s.SaveProvider(ctx, p); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	got, err := s.Provider(ctx, name)
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	if got.Type != cp.ProviderDigitalOcean || got.SecretKey != "do_token" {
		t.Errorf("round trip: type=%q key=%q", got.Type, got.SecretKey)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != cp.CapabilityDNS {
		t.Errorf("capabilities = %v, want [dns]", got.Capabilities)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, p.CreatedAt)
	}

	// Upsert by name: rotating the key (a new token under the same provider) overwrites.
	p.SecretKey = "do_rotated"
	if err := s.SaveProvider(ctx, p); err != nil {
		t.Fatalf("SaveProvider upsert: %v", err)
	}
	if got, _ := s.Provider(ctx, name); got.SecretKey != "do_rotated" {
		t.Errorf("upsert secret key = %q, want do_rotated", got.SecretKey)
	}

	// Providers lists it (among any others in the shared database).
	all, err := s.Providers(ctx)
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	found := false
	for _, q := range all {
		if q.Name == name {
			found = true
		}
	}
	if !found {
		t.Errorf("Providers did not include %q", name)
	}

	// An unknown provider is ErrNotFound.
	if _, err := s.Provider(ctx, t.Name()+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("missing provider err = %v, want ErrNotFound", err)
	}
}
