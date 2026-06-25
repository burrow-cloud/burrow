// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// configuredDNSEngine returns an engine with a "digitalocean" DNS provider already registered
// and its token seeded, ready for domain operations.
func configuredDNSEngine(t *testing.T) (*cp.Engine, *fake.DNSFactory, *fake.Database) {
	t.Helper()
	e, creds, dnsF, d, _ := newProviderEngine(t)
	creds.Set("digitalocean", "tok")
	if _, err := e.AddProvider(context.Background(), cp.AddProviderRequest{Type: cp.ProviderDigitalOcean}); err != nil {
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
