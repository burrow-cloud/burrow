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

// TestClientVersionRecordedInAudit confirms the X-Burrow-Client-Version header rides all the way into
// the audit log: a guarded operation driven by a versioned client records that version on its audit
// rows, next to the principal (ADR-0039). It exercises the full path — header → request context →
// engine → audit row.
func TestClientVersionRecordedInAudit(t *testing.T) {
	h, _, d := newAPIVersion(t, "v0.9.1")

	rec := doVersion(h, "POST", "/v1/apps/web/deploy", token, "v0.9.0", `{"image":"registry.example.com/web:1","replicas":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := d.AuditRows()
	if len(rows) == 0 {
		t.Fatal("no audit rows recorded for the deploy")
	}
	for i, r := range rows {
		if r.ClientVersion != "v0.9.0" {
			t.Errorf("audit row[%d] (%s/%s) client version = %q, want v0.9.0", i, r.Operation, r.Outcome, r.ClientVersion)
		}
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

// doClient is doVersion plus the client-NAME header, so a test can drive a request as a specific
// Burrow binary (ADR-0039). A client that predates the name header is expressed as an empty name.
func doClient(h http.Handler, method, path, tok, clientName, clientVersion, body string) *httptest.ResponseRecorder {
	var br io.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, br)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if clientName != "" {
		req.Header.Set("X-Burrow-Client", clientName)
	}
	if clientVersion != "" {
		req.Header.Set("X-Burrow-Client-Version", clientVersion)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTooOldNamesTheRemedyForTheRefusedBinary is the regression pin for issue #308.
//
// The reported failure was not the refusal: skew is expected and ADR-0039 governs it. The failure
// was that the refusal sent the user somewhere that could not fix it. `burrow` and `burrow-agent`
// are two binaries (ADR-0049 §1); when the stale one was burrow-agent, the message still read "your
// burrow client ... run `brew upgrade burrow` (or reinstall the CLI)", so a user who had just
// upgraded the CLI was told to upgrade the CLI and concluded Burrow was broken.
//
// This asserts the message names the remedy for the binary that was ACTUALLY refused, in all three
// cases the control plane can be in: it knows the caller is burrow-agent, it knows the caller is the
// CLI, or (a client older than the name header — the reported case) it knows neither and must name
// both.
func TestTooOldNamesTheRemedyForTheRefusedBinary(t *testing.T) {
	h, _, _ := newAPIVersion(t, "v0.9.1")

	cases := []struct {
		name       string
		clientName string
		want       []string
		notWant    []string
	}{
		{
			name:       "burrow-agent is named, along with the command that updates IT",
			clientName: "burrow-agent",
			want: []string{
				"burrow-agent",
				// The line that stops the user repeating what they already did.
				"not just the burrow CLI",
				// A source install is not reachable by brew, so the message must carry both remedies.
				"go install github.com/burrow-cloud/burrow/cmd/burrow-agent@v0.9.1",
				// Updating the file is half the fix: a running session keeps the old binary.
				"restart your agent session",
			},
		},
		{
			name:       "the CLI gets the CLI remedy and is not told about the agent binary",
			clientName: "burrow",
			want:       []string{"your burrow CLI", "brew upgrade burrow"},
			notWant:    []string{"burrow-agent", "restart your agent session"},
		},
		{
			name:       "a client too old to name itself is told both binaries must be current",
			clientName: "",
			want: []string{
				"burrow and burrow-agent are separate binaries",
				"brew upgrade burrow",
				"go install github.com/burrow-cloud/burrow/cmd/burrow-agent@v0.9.1",
				"restart your agent session",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doClient(h, "GET", "/v1/apps", token, tc.clientName, "v0.7.0", "")
			if rec.Code != http.StatusUpgradeRequired {
				t.Fatalf("status = %d, want 426; body = %s", rec.Code, rec.Body.String())
			}
			var e struct {
				Error         string `json:"error"`
				Code          string `json:"code"`
				ServerVersion string `json:"server_version"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if e.Code != "client_too_old" {
				t.Errorf("code = %q, want client_too_old", e.Code)
			}
			// The client renders its own install-aware remedy from this, so it has to be on the wire.
			if e.ServerVersion != "v0.9.1" {
				t.Errorf("server_version = %q, want v0.9.1 (the client needs the target version)", e.ServerVersion)
			}
			for _, want := range append([]string{"v0.7.0", "v0.9.1"}, tc.want...) {
				if !strings.Contains(e.Error, want) {
					t.Errorf("error %q\n  want substring %q", e.Error, want)
				}
			}
			for _, no := range tc.notWant {
				if strings.Contains(e.Error, no) {
					t.Errorf("error %q\n  should not contain %q", e.Error, no)
				}
			}
		})
	}
}

// TestUnknownOperationNamesTheClientBinary confirms the newer-client-against-older-server message
// also names which binary is ahead, so an operator reading a relayed error knows what is installed
// where (ADR-0039).
func TestUnknownOperationNamesTheClientBinary(t *testing.T) {
	h, _, _ := newAPIVersion(t, "v0.9.1")

	rec := doClient(h, "POST", "/v1/frobnicate", token, "burrow-agent", "v0.10.0", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(e.Error, "your burrow-agent (v0.10.0)") {
		t.Errorf("error %q, want it to name the burrow-agent client", e.Error)
	}
}
