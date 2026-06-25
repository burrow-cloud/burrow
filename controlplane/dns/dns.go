// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package dns is the production controlplane.DNSFactory and the per-vendor DNS adapters
// (ADR-0018, ADR-0023). burrowd holds each provider's token and is the only thing that talks
// to the vendor's API; the agent never does. The adapters are thin net/http clients — no
// vendor SDK, to keep the dependency graph small (see CLAUDE.md) — with an injectable base URL
// so they are tested against an httptest server rather than the live API.
//
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the managed
// module can wire it; it is source-available under FSL-1.1-ALv2.
package dns

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.DNSFactory = (*Factory)(nil)

// Factory maps a provider type to its vendor adapter (ADR-0023). Adding a vendor is one case
// here plus its adapter file.
type Factory struct {
	http *http.Client
}

// NewFactory returns a Factory with a sensible HTTP timeout.
func NewFactory() *Factory {
	return &Factory{http: &http.Client{Timeout: 15 * time.Second}}
}

// DNS returns a DNSProvider for t authenticated with token, or ErrNotImplemented when no
// adapter serves the type.
func (f *Factory) DNS(t controlplane.ProviderType, token string) (controlplane.DNSProvider, error) {
	switch t {
	case controlplane.ProviderDigitalOcean:
		return newDigitalOcean(token, f.http), nil
	case controlplane.ProviderCloudflare:
		return newCloudflare(token, f.http), nil
	default:
		return nil, fmt.Errorf("dns: no adapter for provider type %q: %w", t, controlplane.ErrNotImplemented)
	}
}

// getAuthorized issues an authenticated GET and returns the status code and body. It is the
// shared shape of the vendors' bearer-token APIs.
func getAuthorized(ctx context.Context, hc *http.Client, url, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}
