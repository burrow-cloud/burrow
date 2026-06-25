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

// AddDomainRequest points a host at an address through a configured DNS provider (ADR-0018).
// The address is the cluster's external entry point — the ingress controller's IP or hostname,
// which `burrow reachability` reports — so the agent supplies it explicitly rather than the
// control plane guessing.
type AddDomainRequest struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	Address  string `json:"address"`
	// Confirm acknowledges the dns_write guardrail so the operation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// RemoveDomainRequest removes the DNS record a provider holds for a host (ADR-0018).
type RemoveDomainRequest struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	// Confirm acknowledges the dns_delete guardrail so the operation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// DomainResult reports the DNS record a domain operation created, updated, or removed.
type DomainResult struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	Type     string `json:"type,omitempty"`
	Address  string `json:"address,omitempty"`
}

// AddDomain creates or updates a DNS record pointing Host at Address, through the named
// provider (ADR-0018). It is a guarded operation: pointing a public hostname at the cluster is
// a blast-radius change, so it trips the dns_write guardrail (confirm by default). The record
// is an A record when Address is an IPv4 address, a CNAME otherwise. The provider must be
// configured and serve DNS; burrowd holds the token and is the only thing that calls the
// vendor.
func (e *Engine) AddDomain(ctx context.Context, req AddDomainRequest) (DomainResult, error) {
	host := strings.TrimSpace(req.Host)
	address := strings.TrimSpace(req.Address)
	if host == "" {
		return DomainResult{}, fmt.Errorf("domain add: host is empty: %w", ErrInvalid)
	}
	if address == "" {
		return DomainResult{}, fmt.Errorf("domain add %s: address is empty: %w", host, ErrInvalid)
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

// recordTypeFor picks A for an IPv4 address and CNAME for anything else (a hostname).
func recordTypeFor(address string) DNSRecordType {
	if ip := net.ParseIP(address); ip != nil && ip.To4() != nil {
		return RecordA
	}
	return RecordCNAME
}
