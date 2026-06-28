// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Capability is a service Burrow can drive a provider to perform on the user's behalf
// (ADR-0023). A capability is served by a chosen provider — DNS is the first; compute and
// registry capabilities follow. Selecting which provider serves a capability is explicit
// for now (the operation names a --provider); auto-detection comes later.
type Capability string

const (
	// CapabilityDNS is managing DNS records for a domain the user has delegated to Burrow.
	CapabilityDNS Capability = "dns"
)

// ProviderType identifies a vendor Burrow knows how to talk to. The type implies the
// capabilities it can serve; the adapter that actually makes the API calls plugs in per
// capability (ADR-0023). A credential is held per provider, so a user can split services
// across vendors (e.g. compute on DigitalOcean, DNS on Cloudflare).
type ProviderType string

const (
	// ProviderDigitalOcean is DigitalOcean (DNS for v0.2; compute later).
	ProviderDigitalOcean ProviderType = "digitalocean"
	// ProviderCloudflare is Cloudflare (DNS).
	ProviderCloudflare ProviderType = "cloudflare"
)

// knownProviderTypes maps each supported vendor to the capabilities it serves. It is the
// registry's knowledge of what a provider type is for; the adapters that make the calls
// land per capability (ADR-0023). Adding a vendor here, and its adapter, is additive.
var knownProviderTypes = map[ProviderType][]Capability{
	ProviderDigitalOcean: {CapabilityDNS},
	ProviderCloudflare:   {CapabilityDNS},
}

// Valid reports whether t is a provider type Burrow supports.
func (t ProviderType) Valid() bool {
	_, ok := knownProviderTypes[t]
	return ok
}

// Capabilities returns the capabilities a provider of type t serves, as a fresh slice so
// callers cannot mutate the registry's knowledge.
func (t ProviderType) Capabilities() []Capability {
	caps := knownProviderTypes[t]
	out := make([]Capability, len(caps))
	copy(out, caps)
	return out
}

// SupportedProviderTypes returns the provider types Burrow supports, sorted — for help
// text and the message shown when an unsupported type is requested.
func SupportedProviderTypes() []ProviderType {
	out := make([]ProviderType, 0, len(knownProviderTypes))
	for t := range knownProviderTypes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// secretKeyPattern matches a valid key in a Kubernetes Secret's data map: alphanumerics,
// '-', '_', and '.'. The token for each provider is stored under such a key in the one
// burrow-credentials Secret (ADR-0023).
var secretKeyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// Provider is a configured vendor credential in the registry (ADR-0023): a name, the
// vendor type, the capabilities it serves, and the key in the burrow-credentials Secret
// that holds its token. The token itself never appears here — the registry is the
// non-secret structure, stored in the database; the control plane reads the token from the
// Secret at call time, so a rotation needs no restart.
type Provider struct {
	// Name identifies the provider within the registry. It defaults to the type but can be
	// set explicitly so a user can register two providers of the same type. It must be a
	// DNS-1123 label.
	Name string `json:"name"`
	// Type is the vendor this provider talks to.
	Type ProviderType `json:"type"`
	// Capabilities are the services this provider serves, derived from its type.
	Capabilities []Capability `json:"capabilities"`
	// SecretKey is the key under which this provider's token lives in burrow-credentials.
	SecretKey string `json:"secret_key"`
	// CreatedAt is when the provider was registered, read from the injected clock.
	CreatedAt time.Time `json:"created_at"`
}

// Serves reports whether the provider serves capability c.
func (p Provider) Serves(c Capability) bool {
	for _, have := range p.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// Validate reports whether the provider is well-formed enough to record.
func (p Provider) Validate() error {
	switch {
	case p.Name == "":
		return fmt.Errorf("provider name is empty")
	case len(p.Name) > maxNameLen:
		return fmt.Errorf("provider name %q is longer than %d characters", p.Name, maxNameLen)
	case !dns1123Label.MatchString(p.Name):
		return fmt.Errorf("provider name %q is not a valid DNS-1123 label", p.Name)
	case !p.Type.Valid():
		return fmt.Errorf("provider type %q is not supported (supported: %v)", p.Type, SupportedProviderTypes())
	case p.SecretKey == "":
		return fmt.Errorf("provider secret key is empty")
	case !secretKeyPattern.MatchString(p.SecretKey):
		return fmt.Errorf("provider secret key %q is not a valid Secret data key", p.SecretKey)
	}
	return nil
}

// AddProvider records a vendor credential in the registry, after validating the token and writing
// it into the burrow-credentials Secret (ADR-0023, ADR-0030). The token VALUE arrives in req over
// burrowd's authenticated control-plane API; burrowd is the only thing that holds it. The order is
// validate-then-write: build the vendor adapter with the passed token and make a cheap
// authenticated call, and only on success write the token to the Secret and record the registry
// entry. A rejected token returns an error and writes NOTHING — no rollback needed, since nothing
// is written until validation passes. The token is never logged, never stored in Postgres, and
// never returned. The provider's capabilities are derived from its type.
func (e *Engine) AddProvider(ctx context.Context, req AddProviderRequest) (Provider, error) {
	name := req.Name
	if name == "" {
		name = string(req.Type)
	}
	secretKey := req.SecretKey
	if secretKey == "" {
		secretKey = name
	}
	p := Provider{
		Name:         name,
		Type:         req.Type,
		Capabilities: req.Type.Capabilities(),
		SecretKey:    secretKey,
		CreatedAt:    e.clock.Now(),
	}
	if err := p.Validate(); err != nil {
		return Provider{}, fmt.Errorf("add provider: %w: %w", ErrInvalid, err)
	}
	if req.Token == "" {
		return Provider{}, fmt.Errorf("add provider %s: a token is required: %w", name, ErrInvalid)
	}

	// Validate the token BEFORE writing anything, so the registry never holds a provider whose
	// credential does not work and the Secret never holds a rejected token. The value is used here
	// in memory and never logged or placed in an error.
	if p.Serves(CapabilityDNS) {
		if err := e.verifyDNSToken(ctx, p, req.Token); err != nil {
			return Provider{}, err
		}
	}

	// Validation passed: write the token into burrow-credentials, then record the entry.
	if err := e.credentials.SetToken(ctx, p.SecretKey, req.Token); err != nil {
		return Provider{}, fmt.Errorf("add provider %s: storing the token: %w", name, err)
	}
	if err := e.db.SaveProvider(ctx, p); err != nil {
		return Provider{}, fmt.Errorf("add provider %s: recording in the registry: %w", name, err)
	}
	return p, nil
}

// verifyDNSToken confirms token authenticates against the vendor's DNS API by building the adapter
// and making a cheap read call. A rejected token is reported as ErrInvalid — a usable, actionable
// failure rather than a server fault. The token value is never logged or formatted into the error.
func (e *Engine) verifyDNSToken(ctx context.Context, p Provider, token string) error {
	dnsp, err := e.dns.DNS(p.Type, token)
	if err != nil {
		return fmt.Errorf("add provider %s: %w", p.Name, err)
	}
	if err := dnsp.VerifyAccess(ctx); err != nil {
		return fmt.Errorf("add provider %s: the %s token was rejected: %w", p.Name, p.Type, err)
	}
	return nil
}

// Providers returns the configured providers, name order (ADR-0023). It reports the
// non-secret registry only; no token is read.
func (e *Engine) Providers(ctx context.Context) ([]Provider, error) {
	ps, err := e.db.Providers(ctx)
	if err != nil {
		return nil, fmt.Errorf("providers: reading the registry: %w", err)
	}
	return ps, nil
}
