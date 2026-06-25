// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

func TestAddProviderDefaultsAndCapabilities(t *testing.T) {
	e, _, _, d, c := newEngine(t, permissive())
	ctx := context.Background()

	p, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	// Name defaults to the type; the secret key defaults to the name.
	if p.Name != "digitalocean" || p.SecretKey != "digitalocean" {
		t.Errorf("defaults: name=%q key=%q, want both %q", p.Name, p.SecretKey, "digitalocean")
	}
	if p.Type != cp.ProviderDigitalOcean {
		t.Errorf("type = %q", p.Type)
	}
	if !p.Serves(cp.CapabilityDNS) {
		t.Errorf("capabilities %v should include dns", p.Capabilities)
	}
	if !p.CreatedAt.Equal(c.Now()) {
		t.Errorf("CreatedAt = %v, want injected clock %v", p.CreatedAt, c.Now())
	}
	// It is persisted in the registry.
	got, err := d.Provider(ctx, "digitalocean")
	if err != nil {
		t.Fatalf("Provider after add: %v", err)
	}
	if got.SecretKey != "digitalocean" {
		t.Errorf("persisted secret key = %q", got.SecretKey)
	}
}

func TestAddProviderExplicitNameAndKey(t *testing.T) {
	e, _, _, _, _ := newEngine(t, permissive())
	p, err := e.AddProvider(context.Background(), cp.AddProviderRequest{
		Name: "do-dns", Type: cp.ProviderDigitalOcean, SecretKey: "do_token",
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if p.Name != "do-dns" || p.SecretKey != "do_token" {
		t.Errorf("name=%q key=%q, want do-dns / do_token", p.Name, p.SecretKey)
	}
}

func TestAddProviderRejectsUnknownTypeAndBadName(t *testing.T) {
	e, _, _, _, _ := newEngine(t, permissive())
	ctx := context.Background()

	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: "aws"}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("unknown type err = %v, want ErrInvalid", err)
	}
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Name: "Bad Name", Type: cp.ProviderDigitalOcean}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("bad name err = %v, want ErrInvalid", err)
	}
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Name: "ok", Type: cp.ProviderDigitalOcean, SecretKey: "bad key"}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("bad secret key err = %v, want ErrInvalid", err)
	}
}

func TestProvidersListedInNameOrderAndUpsert(t *testing.T) {
	e, _, _, _, _ := newEngine(t, permissive())
	ctx := context.Background()

	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderCloudflare}); err != nil {
		t.Fatal(err)
	}
	// Re-adding the same name upserts rather than duplicating.
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean, SecretKey: "rotated"}); err != nil {
		t.Fatal(err)
	}

	ps, err := e.Providers(ctx)
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("len(providers) = %d, want 2 (upsert, not duplicate)", len(ps))
	}
	if ps[0].Name != "cloudflare" || ps[1].Name != "digitalocean" {
		t.Errorf("order = %q, %q; want name order cloudflare, digitalocean", ps[0].Name, ps[1].Name)
	}
	if ps[1].SecretKey != "rotated" {
		t.Errorf("upsert did not update secret key: %q", ps[1].SecretKey)
	}
}

func TestAddProviderSurfacesRegistryError(t *testing.T) {
	e, _, _, d, _ := newEngine(t, permissive())
	d.SetError(fake.OpSaveProvider, errors.New("boom"))
	if _, err := e.AddProvider(context.Background(), cp.AddProviderRequest{Type: cp.ProviderDigitalOcean}); err == nil {
		t.Errorf("AddProvider should surface a registry write error")
	}
}
