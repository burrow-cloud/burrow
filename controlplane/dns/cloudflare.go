// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	// List zones — the read the adapter performs to resolve a record's zone — rather than
	// /user/tokens/verify. The verify endpoint only accepts user-scoped tokens and rejects an
	// account-scoped token (the cfat_ kind) with a 401, even when it is valid; listing zones
	// works for both and confirms the DNS permission Burrow actually needs.
	status, body, err := getAuthorized(ctx, c.http, c.baseURL+"/zones?per_page=1", c.token)
	if err != nil {
		return fmt.Errorf("cloudflare: verifying token: %w", err)
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("cloudflare rejected the token (http %d) — it needs Zone:Read and DNS:Edit: %w", status, controlplane.ErrInvalid)
	default:
		return fmt.Errorf("cloudflare: unexpected response verifying token (http %d): %s", status, snippet(body))
	}
}

// cfRecord is one Cloudflare DNS record. Name is the fully-qualified host.
type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl,omitempty"`
}

// EnsureRecord creates or updates the record so r.Name resolves to r.Value (ADR-0018). It
// resolves the zone that covers the host, then upserts the record in it.
func (c *cloudflare) EnsureRecord(ctx context.Context, r controlplane.DNSRecord) error {
	zid, err := c.zoneFor(ctx, r.Name)
	if err != nil {
		return err
	}
	existing, err := c.recordsFor(ctx, zid, r.Name, string(r.Type))
	if err != nil {
		return err
	}
	ttl := r.TTL
	if ttl == 0 {
		ttl = defaultTTL
	}
	rec := cfRecord{Type: string(r.Type), Name: r.Name, Content: r.Value, TTL: ttl}
	if len(existing) > 0 {
		cur := existing[0]
		if cur.Content == r.Value && (r.TTL == 0 || cur.TTL == ttl) {
			return nil // idempotent
		}
		status, body, err := doJSON(ctx, c.http, http.MethodPut,
			c.baseURL+"/zones/"+zid+"/dns_records/"+cur.ID, c.token, rec)
		if err != nil {
			return fmt.Errorf("cloudflare: updating record: %w", err)
		}
		return checkResponse("cloudflare", "updating record", status, body)
	}
	status, body, err := doJSON(ctx, c.http, http.MethodPost,
		c.baseURL+"/zones/"+zid+"/dns_records", c.token, rec)
	if err != nil {
		return fmt.Errorf("cloudflare: creating record: %w", err)
	}
	return checkResponse("cloudflare", "creating record", status, body)
}

// DeleteRecord removes the A/CNAME record(s) held for host.
func (c *cloudflare) DeleteRecord(ctx context.Context, host string) error {
	zid, err := c.zoneFor(ctx, host)
	if err != nil {
		return err
	}
	recs, err := c.recordsFor(ctx, zid, host, "")
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("cloudflare: no record for %q: %w", host, controlplane.ErrNotFound)
	}
	for _, rec := range recs {
		status, body, err := doJSON(ctx, c.http, http.MethodDelete,
			c.baseURL+"/zones/"+zid+"/dns_records/"+rec.ID, c.token, nil)
		if err != nil {
			return fmt.Errorf("cloudflare: deleting record: %w", err)
		}
		if err := checkResponse("cloudflare", "deleting record", status, body); err != nil {
			return err
		}
	}
	return nil
}

// zoneFor returns the id of the zone that is the longest suffix of host, or ErrNotFound.
func (c *cloudflare) zoneFor(ctx context.Context, host string) (string, error) {
	status, body, err := getAuthorized(ctx, c.http, c.baseURL+"/zones?per_page=50", c.token)
	if err != nil {
		return "", fmt.Errorf("cloudflare: listing zones: %w", err)
	}
	if err := checkResponse("cloudflare", "listing zones", status, body); err != nil {
		return "", err
	}
	var env struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("cloudflare: decoding zones: %w", err)
	}
	names := make([]string, len(env.Result))
	byName := make(map[string]string, len(env.Result))
	for i, z := range env.Result {
		names[i] = z.Name
		byName[z.Name] = z.ID
	}
	zone, ok := longestZoneSuffix(host, names)
	if !ok {
		return "", fmt.Errorf("cloudflare manages no zone covering %q: %w", host, controlplane.ErrNotFound)
	}
	return byName[zone], nil
}

// recordsFor lists the A/CNAME records for host in the zone, optionally narrowed to one type.
func (c *cloudflare) recordsFor(ctx context.Context, zoneID, host, recordType string) ([]cfRecord, error) {
	u := c.baseURL + "/zones/" + zoneID + "/dns_records?per_page=100&name=" + url.QueryEscape(host)
	if recordType != "" {
		u += "&type=" + recordType
	}
	status, body, err := getAuthorized(ctx, c.http, u, c.token)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: listing records: %w", err)
	}
	if err := checkResponse("cloudflare", "listing records", status, body); err != nil {
		return nil, err
	}
	var env struct {
		Result []cfRecord `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("cloudflare: decoding records: %w", err)
	}
	out := make([]cfRecord, 0, len(env.Result))
	for _, rec := range env.Result {
		if !strings.EqualFold(rec.Name, host) {
			continue
		}
		if recordType != "" && !strings.EqualFold(rec.Type, recordType) {
			continue
		}
		if recordType == "" && !strings.EqualFold(rec.Type, "A") && !strings.EqualFold(rec.Type, "CNAME") {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
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
