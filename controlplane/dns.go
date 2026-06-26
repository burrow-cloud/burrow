// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// AddDomain creates or updates a DNS record pointing Host at an address, through the named
// provider (ADR-0018). The address is given explicitly (req.Address) or derived from an exposed
// app's ingress (req.App) so the agent need not look it up. It is a guarded operation: pointing
// a public hostname at the cluster is a blast-radius change, so it trips the dns_write guardrail
// (confirm by default). The record is an A record when the address is an IPv4 address, a CNAME
// otherwise. The provider must be configured and serve DNS; burrowd holds the token and is the
// only thing that calls the vendor.
func (e *Engine) AddDomain(ctx context.Context, req AddDomainRequest) (DomainResult, error) {
	host := strings.TrimSpace(req.Host)
	if host == "" {
		return DomainResult{}, fmt.Errorf("domain add: host is empty: %w", ErrInvalid)
	}
	address, err := e.resolveDomainAddress(ctx, host, req)
	if err != nil {
		return DomainResult{}, err
	}

	p, err := e.loadDNSProvider(ctx, req.Provider)
	if err != nil {
		return DomainResult{}, err
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return DomainResult{}, fmt.Errorf("domain add %s: loading guardrail policy: %w", host, err)
	}
	if err := pol.evaluateGuardrail("domain add", GuardrailDNSWrite, req.Confirm,
		fmt.Sprintf("pointing %s at %s via %s", host, address, p.Name)); err != nil {
		return DomainResult{}, err
	}

	dnsp, err := e.dnsAdapter(ctx, p)
	if err != nil {
		return DomainResult{}, fmt.Errorf("domain add %s: %w", host, err)
	}
	rec := DNSRecord{Type: recordTypeFor(address), Name: host, Value: address}
	if err := dnsp.EnsureRecord(ctx, rec); err != nil {
		if errors.Is(err, ErrNotFound) {
			return DomainResult{}, fmt.Errorf("domain add %s: provider %q manages no zone covering %s — delegate the domain to %s first: %w", host, p.Name, host, p.Type, err)
		}
		return DomainResult{}, fmt.Errorf("domain add %s: %w", host, err)
	}
	return DomainResult{Host: host, Provider: p.Name, Type: string(rec.Type), Address: address}, nil
}

// RemoveDomain deletes the DNS record the provider holds for Host (ADR-0018). It trips the
// dns_delete guardrail (confirm by default) — it is the destructive side of DNS management.
func (e *Engine) RemoveDomain(ctx context.Context, req RemoveDomainRequest) (DomainResult, error) {
	host := strings.TrimSpace(req.Host)
	if host == "" {
		return DomainResult{}, fmt.Errorf("domain remove: host is empty: %w", ErrInvalid)
	}

	p, err := e.loadDNSProvider(ctx, req.Provider)
	if err != nil {
		return DomainResult{}, err
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return DomainResult{}, fmt.Errorf("domain remove %s: loading guardrail policy: %w", host, err)
	}
	if err := pol.evaluateGuardrail("domain remove", GuardrailDNSDelete, req.Confirm,
		fmt.Sprintf("removing the DNS record for %s via %s", host, p.Name)); err != nil {
		return DomainResult{}, err
	}

	dnsp, err := e.dnsAdapter(ctx, p)
	if err != nil {
		return DomainResult{}, fmt.Errorf("domain remove %s: %w", host, err)
	}
	if err := dnsp.DeleteRecord(ctx, host); err != nil {
		if errors.Is(err, ErrNotFound) {
			return DomainResult{}, fmt.Errorf("domain remove %s: provider %q holds no record for it: %w", host, p.Name, err)
		}
		return DomainResult{}, fmt.Errorf("domain remove %s: %w", host, err)
	}
	return DomainResult{Host: host, Provider: p.Name}, nil
}

// loadDNSProvider fetches a configured provider by name and confirms it serves DNS.
func (e *Engine) loadDNSProvider(ctx context.Context, name string) (Provider, error) {
	if strings.TrimSpace(name) == "" {
		return Provider{}, fmt.Errorf("a provider is required (--provider): %w", ErrInvalid)
	}
	p, err := e.db.Provider(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Provider{}, fmt.Errorf("provider %q is not configured — add it with `burrow provider add`: %w", name, ErrNotFound)
		}
		return Provider{}, fmt.Errorf("reading provider %q: %w", name, err)
	}
	if !p.Serves(CapabilityDNS) {
		return Provider{}, fmt.Errorf("provider %q (%s) does not serve DNS: %w", name, p.Type, ErrInvalid)
	}
	return p, nil
}

// dnsAdapter reads the provider's token and builds its DNS adapter. The token is read from the
// burrow-credentials Secret at call time and never leaves the control plane.
func (e *Engine) dnsAdapter(ctx context.Context, p Provider) (DNSProvider, error) {
	token, err := e.credentials.Token(ctx, p.SecretKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("no token for provider %q in burrow-credentials — re-run `burrow provider add`: %w", p.Name, ErrInvalid)
		}
		return nil, fmt.Errorf("reading token for provider %q: %w", p.Name, err)
	}
	dnsp, err := e.dns.DNS(p.Type, token)
	if err != nil {
		return nil, err
	}
	return dnsp, nil
}

// resolveDomainAddress returns the address to point host at: req.Address when given, otherwise
// the controller-assigned external address of the exposed app named in req.App. Deriving it
// from the app's ingress is how `domain add --app web` spares the agent from copying the
// cluster's IP by hand — it is the same address the reachability surface reports.
func (e *Engine) resolveDomainAddress(ctx context.Context, host string, req AddDomainRequest) (string, error) {
	if a := strings.TrimSpace(req.Address); a != "" {
		return a, nil
	}
	app := strings.TrimSpace(req.App)
	if app == "" {
		return "", fmt.Errorf("domain add %s: provide an address (--address) or an exposed app (--app): %w", host, ErrInvalid)
	}
	exp, err := e.k8s.ExposureStatus(ctx, app)
	if err != nil {
		return "", fmt.Errorf("domain add %s: reading the exposure of %q: %w", host, app, err)
	}
	if !exp.Exposed {
		return "", fmt.Errorf("domain add %s: app %q is not exposed — run `burrow expose %s` first, or pass --address: %w", host, app, app, ErrInvalid)
	}
	if exp.Address == "" {
		return "", fmt.Errorf("domain add %s: app %q has no external address yet — the ingress controller has not assigned one; wait and retry, or pass --address: %w", host, app, ErrInvalid)
	}
	return exp.Address, nil
}

// recordTypeFor picks A for an IPv4 address and CNAME for anything else (a hostname).
func recordTypeFor(address string) DNSRecordType {
	if ip := net.ParseIP(address); ip != nil && ip.To4() != nil {
		return RecordA
	}
	return RecordCNAME
}
