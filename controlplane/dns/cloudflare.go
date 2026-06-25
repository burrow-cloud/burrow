// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/burrow-cloud/burrow/controlplane"
)

// cloudflareAPI is the Cloudflare v4 API base. It is a field on the adapter so tests can point
// at an httptest server.
const cloudflareAPI = "https://api.cloudflare.com/client/v4"

var _ controlplane.DNSProvider = (*cloudflare)(nil)

// cloudflare is the Cloudflare DNS adapter (ADR-0023). v0.2 implements VerifyAccess, with
// record management to follow on the same adapter.
type cloudflare struct {
	token   string
	baseURL string
	http    *http.Client
}

func newCloudflare(token string, hc *http.Client) *cloudflare {
	return &cloudflare{token: token, baseURL: cloudflareAPI, http: hc}
}

// VerifyAccess calls Cloudflare's token-verify endpoint. A valid token returns 200 with an
// active status; a rejected token returns 401/403 (ErrInvalid). Cloudflare also reports
// failure in the JSON envelope, so a 200 whose token is not active is treated as invalid too.
func (c *cloudflare) VerifyAccess(ctx context.Context) error {
	status, body, err := getAuthorized(ctx, c.http, c.baseURL+"/user/tokens/verify", c.token)
	if err != nil {
		return fmt.Errorf("cloudflare: verifying token: %w", err)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("cloudflare rejected the token (http %d): %w", status, controlplane.ErrInvalid)
	case status < 200 || status >= 300:
		return fmt.Errorf("cloudflare: unexpected response verifying token (http %d): %s", status, snippet(body))
	}

	var env struct {
		Success bool `json:"success"`
		Result  struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("cloudflare: decoding verify response: %w", err)
	}
	if !env.Success || !strings.EqualFold(env.Result.Status, "active") {
		return fmt.Errorf("cloudflare reports the token is not active (status %q): %w", env.Result.Status, controlplane.ErrInvalid)
	}
	return nil
}

// snippet trims a response body to a short, single-line form for error messages.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
