// SPDX-License-Identifier: Apache-2.0
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
		Kubernetes: fake.NewKubernetes(), Database: d,
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

	// The token VALUE travels in the request (over burrowd's authenticated API in production).
	p, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean, Token: "dop_v1_tok"})
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
	// The engine validated with the passed token and then wrote it into the credential store.
	if tok, calls := dnsF.LastToken(); tok != "dop_v1_tok" || calls != 1 {
		t.Errorf("verify used token=%q calls=%d, want dop_v1_tok / 1", tok, calls)
	}
	if tok, ok := creds.Get("digitalocean"); !ok || tok != "dop_v1_tok" {
		t.Errorf("SetToken stored %q ok=%v, want dop_v1_tok true", tok, ok)
	}
	if _, err := d.Provider(ctx, "digitalocean"); err != nil {
		t.Errorf("provider not persisted: %v", err)
	}
}

func TestAddProviderExplicitNameAndKey(t *testing.T) {
	e, creds, dnsF, _, _ := newProviderEngine(t)
	p, err := e.AddProvider(context.Background(), cp.AddProviderRequest{
		Name: "do-dns", Type: cp.ProviderDigitalOcean, SecretKey: "do_token", Token: "tok",
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if p.Name != "do-dns" || p.SecretKey != "do_token" {
		t.Errorf("name=%q key=%q, want do-dns / do_token", p.Name, p.SecretKey)
	}
	if tok, _ := dnsF.LastToken(); tok != "tok" {
		t.Errorf("verify used the wrong token: %q", tok)
	}
	// The token was written under the explicit key.
	if tok, ok := creds.Get("do_token"); !ok || tok != "tok" {
		t.Errorf("SetToken stored %q ok=%v under do_token, want tok true", tok, ok)
	}
}

func TestAddProviderRejectsUnknownTypeAndBadName(t *testing.T) {
	e, _, _, _, _ := newProviderEngine(t)
	ctx := context.Background()

	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: "aws", Token: "tok"}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("unknown type err = %v, want ErrInvalid", err)
	}
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Name: "Bad Name", Type: cp.ProviderDigitalOcean, Token: "tok"}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("bad name err = %v, want ErrInvalid", err)
	}
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Name: "ok", Type: cp.ProviderDigitalOcean, SecretKey: "bad key", Token: "tok"}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("bad secret key err = %v, want ErrInvalid", err)
	}
}

func TestAddProviderRejectsBadTokenAndDoesNotSave(t *testing.T) {
	e, creds, dnsF, d, _ := newProviderEngine(t)
	ctx := context.Background()
	dnsF.SetVerifyError(fmt.Errorf("digitalocean rejected the token: %w", cp.ErrInvalid))

	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean, Token: "bad"}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("rejected token err = %v, want ErrInvalid", err)
	}
	// Validation runs BEFORE any write: a rejected token leaves neither the Secret nor the registry.
	if _, ok := creds.Get("digitalocean"); ok {
		t.Errorf("a rejected token must not be written to the credential store")
	}
	if _, err := d.Provider(ctx, "digitalocean"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("a provider with a rejected token must not be recorded (got %v)", err)
	}
}

func TestAddProviderMissingTokenIsInvalid(t *testing.T) {
	e, _, _, d, _ := newProviderEngine(t)
	ctx := context.Background()
	// No token in the request.
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

	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean, Token: "do-tok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderCloudflare, Token: "cf-tok"}); err != nil {
		t.Fatal(err)
	}
	// Re-adding the same name upserts rather than duplicating; the rotation lands under a new key.
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean, SecretKey: "rotated", Token: "do-tok2"}); err != nil {
		t.Fatal(err)
	}
	if tok, ok := creds.Get("rotated"); !ok || tok != "do-tok2" {
		t.Errorf("rotation stored %q ok=%v, want do-tok2 true", tok, ok)
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
	e, _, _, d, _ := newProviderEngine(t)
	d.SetError(fake.OpSaveProvider, errors.New("boom"))
	if _, err := e.AddProvider(context.Background(), cp.AddProviderRequest{Type: cp.ProviderDigitalOcean, Token: "tok"}); err == nil {
		t.Errorf("AddProvider should surface a registry write error")
	}
}

// TestAddProviderSourceProviderStoresTokenWithoutDNSVerify asserts a source provider (github)
// registers with the source capability and its token is written to the credential store WITHOUT a
// DNS verification call — a source token has no DNS vendor to check against (ADR-0057). The token
// still rides the guarded control-plane path (ADR-0030) and is never echoed back.
func TestAddProviderSourceProviderStoresTokenWithoutDNSVerify(t *testing.T) {
	e, creds, dnsF, d, _ := newProviderEngine(t)
	ctx := context.Background()

	p, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderGitHub, Token: "ghp_source_token"})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if p.Name != "github" || p.SecretKey != "github" {
		t.Errorf("defaults: name=%q key=%q, want both %q", p.Name, p.SecretKey, "github")
	}
	if !p.Serves(cp.CapabilitySource) || p.Serves(cp.CapabilityDNS) {
		t.Errorf("capabilities %v, want source-only", p.Capabilities)
	}
	// No DNS verification for a source provider.
	if _, calls := dnsF.LastToken(); calls != 0 {
		t.Errorf("source provider triggered %d DNS verify calls, want 0", calls)
	}
	if tok, ok := creds.Get("github"); !ok || tok != "ghp_source_token" {
		t.Errorf("SetToken stored %q ok=%v, want the source token", tok, ok)
	}
	if _, err := d.Provider(ctx, "github"); err != nil {
		t.Errorf("source provider not persisted: %v", err)
	}
}
