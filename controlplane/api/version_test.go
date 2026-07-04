// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doVersion is do() plus the ADR-0039 client-version handshake header, so a test can drive a request
// as a client of a specific version.
func doVersion(h http.Handler, method, path, tok, clientVersion, body string) *httptest.ResponseRecorder {
	var br io.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, br)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if clientVersion != "" {
		req.Header.Set("X-Burrow-Client-Version", clientVersion)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestVersionGateTooOldClient confirms a client more than one minor behind burrowd is refused with a
// structured, actionable error before the request reaches a handler (ADR-0039).
func TestVersionGateTooOldClient(t *testing.T) {
	h, _, _ := newAPIVersion(t, "v0.9.1")

	rec := doVersion(h, "GET", "/v1/apps", token, "v0.7.0", "")
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426; body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != "client_too_old" {
		t.Errorf("code = %q, want client_too_old", e.Code)
	}
	for _, want := range []string{"v0.7.0", "v0.9.1", "brew upgrade"} {
		if !strings.Contains(e.Error, want) {
			t.Errorf("error %q, want substring %q", e.Error, want)
		}
	}
}

// TestVersionGateServesInWindowAndNewer confirms burrowd never hard-blocks on version difference
// alone: an in-window client, a newer client, and a pre-handshake (no header) or dev client are all
// served rather than refused (ADR-0039).
func TestVersionGateServesInWindowAndNewer(t *testing.T) {
	h, _, _ := newAPIVersion(t, "v0.9.1")

	for _, cv := range []string{"v0.9.0", "v0.8.4", "v0.10.0", "v1.0.0", "", "dev"} {
		rec := doVersion(h, "GET", "/v1/apps", token, cv, "")
		if rec.Code == http.StatusUpgradeRequired {
			t.Errorf("client %q got 426, want served (never hard-block on difference alone)", cv)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("client %q status = %d, want 200; body = %s", cv, rec.Code, rec.Body.String())
		}
	}
}

// TestUnknownOperationStructured confirms a request for a route this burrowd does not have becomes a
// structured "unknown operation" error that names the server version and the fix (ADR-0039), rather
// than a bare 404 — the newer-client-against-older-server case.
func TestUnknownOperationStructured(t *testing.T) {
	h, _, _ := newAPIVersion(t, "v0.9.1")

	rec := doVersion(h, "POST", "/v1/frobnicate", token, "v0.10.0", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != "unknown_operation" {
		t.Errorf("code = %q, want unknown_operation", e.Code)
	}
	for _, want := range []string{"v0.9.1", "v0.10.0", "burrow upgrade"} {
		if !strings.Contains(e.Error, want) {
			t.Errorf("error %q, want substring %q", e.Error, want)
		}
	}
}

// TestUnknownOperationPreservesMethodNotAllowed confirms the structured-404 wrapper does not swallow
// a method mismatch on an existing path: that stays a 405, not an unknown_operation (ADR-0039).
func TestUnknownOperationPreservesMethodNotAllowed(t *testing.T) {
	h, _, _ := newAPIVersion(t, "v0.9.1")

	rec := doVersion(h, "GET", "/v1/apps/web/deploy", token, "v0.9.0", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 (a wrong method on a real route must stay 405); body = %s", rec.Code, rec.Body.String())
	}
}

// TestVersionHandshakePermissiveWithoutServerVersion confirms a burrowd with no version set (a local
// or e2e build) enforces no window: even an ancient client is served (ADR-0039).
func TestVersionHandshakePermissiveWithoutServerVersion(t *testing.T) {
	h, _, _ := newAPIVersion(t, "")

	if rec := doVersion(h, "GET", "/v1/apps", token, "v0.1.0", ""); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no server version → permissive handshake); body = %s", rec.Code, rec.Body.String())
	}
}
