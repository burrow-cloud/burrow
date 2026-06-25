// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package dns

import (
	"context"
	"fmt"
	"net/http"

	"github.com/burrow-cloud/burrow/controlplane"
)

// digitalOceanAPI is the DigitalOcean API base. It is a field on the adapter so tests can
// point at an httptest server.
const digitalOceanAPI = "https://api.digitalocean.com"

var _ controlplane.DNSProvider = (*digitalOcean)(nil)

// digitalOcean is the DigitalOcean DNS adapter (ADR-0023). It manages records under domains
// the user has delegated to Burrow; v0.2 implements VerifyAccess, with record management to
// follow on the same adapter.
type digitalOcean struct {
	token   string
	baseURL string
	http    *http.Client
}

func newDigitalOcean(token string, hc *http.Client) *digitalOcean {
	return &digitalOcean{token: token, baseURL: digitalOceanAPI, http: hc}
}

// VerifyAccess lists domains (one page) to confirm the token authenticates and carries DNS
// read access. A 401/403 is a rejected token (ErrInvalid); any other non-2xx is a vendor or
// transport error reported as-is.
func (d *digitalOcean) VerifyAccess(ctx context.Context) error {
	status, body, err := getAuthorized(ctx, d.http, d.baseURL+"/v2/domains?per_page=1", d.token)
	if err != nil {
		return fmt.Errorf("digitalocean: verifying token: %w", err)
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("digitalocean rejected the token (http %d): %w", status, controlplane.ErrInvalid)
	default:
		return fmt.Errorf("digitalocean: unexpected response verifying token (http %d): %s", status, snippet(body))
	}
}
