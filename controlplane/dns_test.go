// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// configuredDNSEngine returns an engine with a "digitalocean" DNS provider already registered
// (its token sent through AddProvider), ready for domain operations.
func configuredDNSEngine(t *testing.T) (*cp.Engine, *fake.DNSFactory, *fake.Database) {
	t.Helper()
	e, _, dnsF, d, _ := newProviderEngine(t)
	if _, err := e.AddProvider(context.Background(), cp.AddProviderRequest{Type: cp.ProviderDigitalOcean, Token: "tok"}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	return e, dnsF, d
}

func TestAddDomainCreatesRecordAndInfersType(t *testing.T) {
	e, dnsF, _ := configuredDNSEngine(t)
	ctx := context.Background()

	// An IPv4 address yields an A record.
	res, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.example.com", Provider: "digitalocean", Address: "203.0.113.5", Confirm: true})
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if res.Type != "A" || res.Address != "203.0.113.5" || res.Provider != "digitalocean" {
		t.Errorf("result = %+v", res)
	}
	if rec, ok := dnsF.Provider().Record("app.example.com"); !ok || rec.Type != cp.RecordA || rec.Value != "203.0.113.5" {
		t.Errorf("record = %+v ok=%v", rec, ok)
	}

	// A hostname yields a CNAME.
	res, err = e.AddDomain(ctx, cp.AddDomainRequest{Host: "www.example.com", Provider: "digitalocean", Address: "lb.example.net", Confirm: true})
	if err != nil {
		t.Fatalf("AddDomain cname: %v", err)
	}
	if res.Type != "CNAME" {
		t.Errorf("hostname address should be a CNAME, got %q", res.Type)
	}
}

func TestAddDomainGuardrailHoldsWithoutConfirm(t *testing.T) {
	e, _, _ := configuredDNSEngine(t)
	_, err := e.AddDomain(context.Background(), cp.AddDomainRequest{Host: "app.example.com", Provider: "digitalocean", Address: "203.0.113.5"})
	mustGuardrail(t, err, cp.GuardrailDNSWrite)
}

func TestAddDomainValidation(t *testing.T) {
	e, _, d := configuredDNSEngine(t)
	ctx := context.Background()

	// Empty host / address.
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Provider: "digitalocean", Address: "203.0.113.5", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("empty host err = %v, want ErrInvalid", err)
	}
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.example.com", Provider: "digitalocean", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("empty address err = %v, want ErrInvalid", err)
	}
	// Unknown provider.
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.example.com", Provider: "nope", Address: "203.0.113.5", Confirm: true}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("unknown provider err = %v, want ErrNotFound", err)
	}
	// A provider that does not serve DNS (recorded directly, bypassing AddProvider).
	_ = d.SaveProvider(ctx, cp.Provider{Name: "blob", Type: "blobstore", SecretKey: "blob"})
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.example.com", Provider: "blob", Address: "203.0.113.5", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("non-DNS provider err = %v, want ErrInvalid", err)
	}
}

func TestAddDomainZoneNotFoundAndProviderError(t *testing.T) {
	e, dnsF, _ := configuredDNSEngine(t)
	ctx := context.Background()

	// The provider manages no zone covering the host → ErrNotFound, surfaced with guidance.
	dnsF.SetEnsureError(fmt.Errorf("manages no zone: %w", cp.ErrNotFound))
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.elsewhere.com", Provider: "digitalocean", Address: "203.0.113.5", Confirm: true}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("zone-not-found err = %v, want ErrNotFound", err)
	}

	// A generic vendor error propagates.
	dnsF.SetEnsureError(errors.New("boom"))
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.example.com", Provider: "digitalocean", Address: "203.0.113.5", Confirm: true}); err == nil {
		t.Errorf("provider error should propagate")
	}
}

func TestRemoveDomain(t *testing.T) {
	e, dnsF, _ := configuredDNSEngine(t)
	ctx := context.Background()

	// Create then remove.
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.example.com", Provider: "digitalocean", Address: "203.0.113.5", Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RemoveDomain(ctx, cp.RemoveDomainRequest{Host: "app.example.com", Provider: "digitalocean", Confirm: true}); err != nil {
		t.Fatalf("RemoveDomain: %v", err)
	}
	if _, ok := dnsF.Provider().Record("app.example.com"); ok {
		t.Errorf("record should be gone after RemoveDomain")
	}

	// Removing a record that does not exist is ErrNotFound.
	if _, err := e.RemoveDomain(ctx, cp.RemoveDomainRequest{Host: "missing.example.com", Provider: "digitalocean", Confirm: true}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("missing record err = %v, want ErrNotFound", err)
	}
}

func TestRemoveDomainGuardrailHoldsWithoutConfirm(t *testing.T) {
	e, _, _ := configuredDNSEngine(t)
	if _, err := e.AddDomain(context.Background(), cp.AddDomainRequest{Host: "app.example.com", Provider: "digitalocean", Address: "203.0.113.5", Confirm: true}); err != nil {
		t.Fatal(err)
	}
	_, err := e.RemoveDomain(context.Background(), cp.RemoveDomainRequest{Host: "app.example.com", Provider: "digitalocean"})
	mustGuardrail(t, err, cp.GuardrailDNSDelete)
}

func TestAddDomainDerivesAddressFromExposedApp(t *testing.T) {
	k := fake.NewKubernetes()
	creds := fake.NewCredentials()
	dnsF := fake.NewDNSFactory()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Registry: fake.NewRegistry(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: dnsF,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := e.AddProvider(ctx, cp.AddProviderRequest{Type: cp.ProviderDigitalOcean, Token: "tok"}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	// Expose an app and let the ingress controller assign it an address.
	if err := k.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.svc", Port: 80}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	k.SetIngressAddress("web", "203.0.113.50")

	// --app, no --address: the address is read from the app's exposure.
	res, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "web.example.com", Provider: "digitalocean", App: "web", Confirm: true})
	if err != nil {
		t.Fatalf("AddDomain from app: %v", err)
	}
	if res.Address != "203.0.113.50" || res.Type != "A" {
		t.Errorf("result = %+v, want address 203.0.113.50 / A", res)
	}
	if rec, ok := dnsF.Provider().Record("web.example.com"); !ok || rec.Value != "203.0.113.50" {
		t.Errorf("record = %+v ok=%v", rec, ok)
	}

	// An explicit --address still wins over --app.
	if res, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "web.example.com", Provider: "digitalocean", App: "web", Address: "198.51.100.7", Confirm: true}); err != nil || res.Address != "198.51.100.7" {
		t.Errorf("explicit address should win: %+v %v", res, err)
	}

	// Neither address nor app → ErrInvalid.
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "x.example.com", Provider: "digitalocean", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("no address/app err = %v, want ErrInvalid", err)
	}

	// An app that is not exposed → ErrInvalid with guidance.
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "x.example.com", Provider: "digitalocean", App: "ghost", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("unexposed app err = %v, want ErrInvalid", err)
	}

	// Exposed but no external address assigned yet → ErrInvalid (wait or pass --address).
	if err := k.Expose(ctx, cp.ExposeSpec{App: "pending", Host: "pending.svc", Port: 80}); err != nil {
		t.Fatalf("Expose pending: %v", err)
	}
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "pending.example.com", Provider: "digitalocean", App: "pending", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("no-address-yet err = %v, want ErrInvalid", err)
	}
}

func TestAddDomainAutoSelectsSoleDNSProvider(t *testing.T) {
	e, dnsF, _ := configuredDNSEngine(t) // one provider configured: digitalocean
	// No Provider given — the engine auto-selects the only configured DNS provider.
	res, err := e.AddDomain(context.Background(), cp.AddDomainRequest{Host: "app.example.com", Address: "203.0.113.5", Confirm: true})
	if err != nil {
		t.Fatalf("AddDomain with auto provider: %v", err)
	}
	if res.Provider != "digitalocean" {
		t.Errorf("auto-selected provider = %q, want digitalocean", res.Provider)
	}
	if _, ok := dnsF.Provider().Record("app.example.com"); !ok {
		t.Errorf("record not created via the auto-selected provider")
	}
}

func TestDomainProviderResolutionErrors(t *testing.T) {
	e, _, _, d, _ := newProviderEngine(t)
	ctx := context.Background()

	// Zero DNS providers configured → actionable error, not a guess.
	if _, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.example.com", Address: "203.0.113.5", Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("no-provider err = %v, want ErrInvalid", err)
	}

	// Several DNS providers → must ask which one (recorded directly to skip token validation).
	_ = d.SaveProvider(ctx, cp.Provider{Name: "do", Type: cp.ProviderDigitalOcean, Capabilities: []cp.Capability{cp.CapabilityDNS}, SecretKey: "do"})
	_ = d.SaveProvider(ctx, cp.Provider{Name: "cf", Type: cp.ProviderCloudflare, Capabilities: []cp.Capability{cp.CapabilityDNS}, SecretKey: "cf"})
	_, err := e.AddDomain(ctx, cp.AddDomainRequest{Host: "app.example.com", Address: "203.0.113.5", Confirm: true})
	if !errors.Is(err, cp.ErrInvalid) || !strings.Contains(err.Error(), "multiple") {
		t.Errorf("multiple-provider err = %v, want ErrInvalid mentioning multiple", err)
	}
}
