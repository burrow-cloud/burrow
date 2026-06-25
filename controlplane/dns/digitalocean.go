// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

// doRecord is one DigitalOcean DNS record. Name is relative to the domain ("@" for the apex).
type doRecord struct {
	ID   int    `json:"id,omitempty"`
	Type string `json:"type"`
	Name string `json:"name"`
	Data string `json:"data"`
	TTL  int    `json:"ttl,omitempty"`
}

// EnsureRecord creates or updates the record so r.Name resolves to r.Value (ADR-0018). It
// resolves the managed domain that covers the host, then upserts the record under it.
func (d *digitalOcean) EnsureRecord(ctx context.Context, r controlplane.DNSRecord) error {
	zone, err := d.zoneFor(ctx, r.Name)
	if err != nil {
		return err
	}
	existing, err := d.recordsFor(ctx, zone, r.Name, string(r.Type))
	if err != nil {
		return err
	}
	ttl := r.TTL
	if ttl == 0 {
		ttl = defaultTTL
	}
	if len(existing) > 0 {
		cur := existing[0]
		if cur.Data == r.Value && (r.TTL == 0 || cur.TTL == ttl) {
			return nil // idempotent: already as desired
		}
		status, body, err := doJSON(ctx, d.http, http.MethodPut,
			d.baseURL+"/v2/domains/"+zone+"/records/"+strconv.Itoa(cur.ID), d.token,
			doRecord{Data: r.Value, TTL: ttl})
		if err != nil {
			return fmt.Errorf("digitalocean: updating record: %w", err)
		}
		return checkResponse("digitalocean", "updating record", status, body)
	}
	status, body, err := doJSON(ctx, d.http, http.MethodPost,
		d.baseURL+"/v2/domains/"+zone+"/records", d.token,
		doRecord{Type: string(r.Type), Name: relName(r.Name, zone), Data: r.Value, TTL: ttl})
	if err != nil {
		return fmt.Errorf("digitalocean: creating record: %w", err)
	}
	return checkResponse("digitalocean", "creating record", status, body)
}

// DeleteRecord removes the A/CNAME record(s) held for host.
func (d *digitalOcean) DeleteRecord(ctx context.Context, host string) error {
	zone, err := d.zoneFor(ctx, host)
	if err != nil {
		return err
	}
	recs, err := d.recordsFor(ctx, zone, host, "")
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("digitalocean: no record for %q: %w", host, controlplane.ErrNotFound)
	}
	for _, rec := range recs {
		status, body, err := doJSON(ctx, d.http, http.MethodDelete,
			d.baseURL+"/v2/domains/"+zone+"/records/"+strconv.Itoa(rec.ID), d.token, nil)
		if err != nil {
			return fmt.Errorf("digitalocean: deleting record: %w", err)
		}
		if err := checkResponse("digitalocean", "deleting record", status, body); err != nil {
			return err
		}
	}
	return nil
}

// zoneFor returns the managed domain that is the longest suffix of host, or ErrNotFound.
func (d *digitalOcean) zoneFor(ctx context.Context, host string) (string, error) {
	status, body, err := getAuthorized(ctx, d.http, d.baseURL+"/v2/domains?per_page=200", d.token)
	if err != nil {
		return "", fmt.Errorf("digitalocean: listing domains: %w", err)
	}
	if err := checkResponse("digitalocean", "listing domains", status, body); err != nil {
		return "", err
	}
	var resp struct {
		Domains []struct {
			Name string `json:"name"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("digitalocean: decoding domains: %w", err)
	}
	zones := make([]string, len(resp.Domains))
	for i, z := range resp.Domains {
		zones[i] = z.Name
	}
	zone, ok := longestZoneSuffix(host, zones)
	if !ok {
		return "", fmt.Errorf("digitalocean manages no domain covering %q: %w", host, controlplane.ErrNotFound)
	}
	return zone, nil
}

// recordsFor lists the A/CNAME records for host in zone, optionally narrowed to one type. It
// reconstructs each record's fully-qualified name so the match does not depend on the server's
// name-filter semantics.
func (d *digitalOcean) recordsFor(ctx context.Context, zone, host, recordType string) ([]doRecord, error) {
	u := d.baseURL + "/v2/domains/" + zone + "/records?per_page=200&name=" + url.QueryEscape(host)
	if recordType != "" {
		u += "&type=" + recordType
	}
	status, body, err := getAuthorized(ctx, d.http, u, d.token)
	if err != nil {
		return nil, fmt.Errorf("digitalocean: listing records: %w", err)
	}
	if err := checkResponse("digitalocean", "listing records", status, body); err != nil {
		return nil, err
	}
	var resp struct {
		Records []doRecord `json:"domain_records"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("digitalocean: decoding records: %w", err)
	}
	out := make([]doRecord, 0, len(resp.Records))
	for _, rec := range resp.Records {
		if !strings.EqualFold(doFQDN(rec.Name, zone), host) {
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

// relName turns a fully-qualified host into the name DigitalOcean stores it under: relative to
// the zone, or "@" for the apex.
func relName(host, zone string) string {
	if strings.EqualFold(host, zone) {
		return "@"
	}
	return strings.TrimSuffix(host, "."+zone)
}

// doFQDN reconstructs a record's fully-qualified name from its relative name and zone.
func doFQDN(name, zone string) string {
	if name == "@" || name == "" {
		return zone
	}
	return name + "." + zone
}
