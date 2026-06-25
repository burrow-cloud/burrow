// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// newProviderEngine builds an engine exposing the credential store and DNS factory, so a test
// can seed the token the CLI would have written and control the vendor's verdict.
func newProviderEngine(t *testing.T) (*cp.Engine, *fake.Credentials, *fake.DNSFactory, *fake.Database, *fake.Clock) {
	t.Helper()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	creds := fake.NewCredentials()
	dnsF := fake.NewDNSFactory()
	c := fake.NewClock(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC))
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Registry: fake.NewRegistry(), Database: d,
		Clock: c, IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: dnsF,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, creds, dnsF, d, c
}

func TestAddProviderDefaultsCapabilitiesAndVerifies(t *testing.T) {
	e, creds, dnsF, d, c := newProviderEngine(t)
	ctx := context.Background()
	creds.Set("digitalocean", "dop_v1_tok") // the CLI wrote this before calling AddProvider

	p, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	// Name defaults to the type; the secret key defaults to the name.
	if p.Name != "digitalocean" || p.SecretKey != "digitalocean" {
		t.Errorf("defaults: name=%q key=%q, want both %q", p.Name, p.SecretKey, "digitalocean")
	}
	if !p.Serves(cp.CapabilityDNS) {
		t.Errorf("capabilities %v should include dns", p.Capabilities)
	}
	if !p.CreatedAt.Equal(c.Now()) {
		t.Errorf("CreatedAt = %v, want injected clock %v", p.CreatedAt, c.Now())
	}
	// The engine read the token from the Secret and passed it to the vendor adapter — it is
	// never sent over the API.
	if tok, calls := dnsF.LastToken(); tok != "dop_v1_tok" || calls != 1 {
		t.Errorf("verify used token=%q calls=%d, want dop_v1_tok / 1", tok, calls)
	}
	if _, err := d.Provider(ctx, "digitalocean"); err != nil {
		t.Errorf("provider not persisted: %v", err)
	}
}

func TestAddProviderExplicitNameAndKey(t *testing.T) {
	e, creds, dnsF, _, _ := newProviderEngine(t)
	creds.Set("do_token", "tok")
	p, err := e.AddProvider(context.Background(), cp.AddProviderRequest{
		Name: "do-dns", Type: cp.ProviderDigitalOcean, SecretKey: "do_token",
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if p.Name != "do-dns" || p.SecretKey != "do_token" {
		t.Errorf("name=%q key=%q, want do-dns / do_token", p.Name, p.SecretKey)
	}
	if tok, _ := dnsF.LastToken(); tok != "tok" {
		t.Errorf("verify read the wrong key's token: %q", tok)
	}
}

func TestAddProviderRejectsUnknownTypeAndBadName(t *testing.T) {
	e, creds, _, _, _ := newProviderEngine(t)
	ctx := context.Background()
	creds.Set("digitalocean", "tok") // present, to prove rejection is about the request, not the token

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

func TestAddProviderRejectsBadTokenAndDoesNotSave(t *testing.T) {
	e, creds, dnsF, d, _ := newProviderEngine(t)
	ctx := context.Background()
	creds.Set("digitalocean", "bad")
	dnsF.SetVerifyError(fmt.Errorf("digitalocean rejected the token: %w", cp.ErrInvalid))

	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("rejected token err = %v, want ErrInvalid", err)
	}
	if _, err := d.Provider(ctx, "digitalocean"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("a provider with a rejected token must not be recorded (got %v)", err)
	}
}

func TestAddProviderMissingTokenIsInvalid(t *testing.T) {
	e, _, _, d, _ := newProviderEngine(t)
	ctx := context.Background()
	// No token seeded — as if the Secret write never landed.
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("missing token err = %v, want ErrInvalid", err)
	}
	if _, err := d.Provider(ctx, "digitalocean"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("nothing should be recorded when the token is missing (got %v)", err)
	}
}

func TestProvidersListedInNameOrderAndUpsert(t *testing.T) {
	e, creds, _, _, _ := newProviderEngine(t)
	ctx := context.Background()
	creds.Set("digitalocean", "do-tok")
	creds.Set("cloudflare", "cf-tok")
	creds.Set("rotated", "do-tok2") // the rotation's new key

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
	e, creds, _, d, _ := newProviderEngine(t)
	creds.Set("digitalocean", "tok")
	d.SetError(fake.OpSaveProvider, errors.New("boom"))
	if _, err := e.AddProvider(context.Background(), cp.AddProviderRequest{Type: cp.ProviderDigitalOcean}); err == nil {
		t.Errorf("AddProvider should surface a registry write error")
	}
}
