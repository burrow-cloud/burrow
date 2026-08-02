// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// theToken is the credential these tests present. It is a distinctive string so any test can assert
// it did NOT appear somewhere it should not.
const theToken = "tok_a_real_looking_burrow_cloud_credential"

// TestBearerTransportPresentsTheCredentialAsBearer pins the wire format the managed control plane
// authenticates on. It is not interchangeable with X-Burrow-Token: the tenant router reads the
// bearer header to decide which tenant is calling and STRIPS X-Burrow-Token before the request
// reaches the per-tenant handler, so a credential sent in the other header would authenticate
// nothing.
func TestBearerTransportPresentsTheCredentialAsBearer(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[]}`))
	}))
	defer srv.Close()

	c, err := BearerTransport{BaseURL: srv.URL, Token: theToken, Name: ClientNameCLI, Version: "v9.9.9"}.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Apps(context.Background(), ""); err != nil {
		t.Fatalf("Apps: %v", err)
	}

	if want := "Bearer " + theToken; got.Get("Authorization") != want {
		t.Errorf("Authorization = %q, want the credential as a bearer token", got.Get("Authorization"))
	}
	if v := got.Get("X-Burrow-Token"); v != "" {
		t.Errorf("X-Burrow-Token = %q; the credential must not also ride the self-hosted header", v)
	}
	if got.Get("X-Burrow-Client") != ClientNameCLI || got.Get("X-Burrow-Client-Version") != "v9.9.9" {
		t.Errorf("handshake headers = %q/%q, want the binary and its version (ADR-0039)",
			got.Get("X-Burrow-Client"), got.Get("X-Burrow-Client-Version"))
	}
}

// TestBearerTransportRefusesCleartext: a bearer credential is a password on every request, so it
// does not go out over plain HTTP. Loopback is exempt so a test can drive a local server without
// weakening the rule.
func TestBearerTransportRefusesCleartext(t *testing.T) {
	_, err := BearerTransport{BaseURL: "http://burrow-cloud.dev", Token: theToken}.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect accepted a plain-HTTP control-plane URL")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error = %q, want it to say the URL must be https", err)
	}
	if strings.Contains(err.Error(), theToken) {
		t.Error("the error carried the credential")
	}

	if _, err := (BearerTransport{BaseURL: "https://burrow-cloud.dev", Token: theToken}).Connect(context.Background()); err != nil {
		t.Errorf("Connect refused an https URL: %v", err)
	}
}

// TestBearerTransportNeedsACredential: connecting with nothing to present is a bug worth naming
// here rather than a 401 several layers down.
func TestBearerTransportNeedsACredential(t *testing.T) {
	if _, err := (BearerTransport{BaseURL: "https://burrow-cloud.dev"}).Connect(context.Background()); err == nil {
		t.Fatal("Connect succeeded with no credential")
	}
}

// TestRefusedCredentialIsLegible. A person signed in yesterday and is unauthorized today; the
// control plane's own 401 says only "missing or invalid authorization", which is the one thing they
// already know. The transport replaces it with what actually happened and what to do about it.
func TestRefusedCredentialIsLegible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing or invalid authorization","code":"unauthorized"}`))
	}))
	defer srv.Close()

	const rejected = `that credential was revoked; sign in again with "burrow auth login"`
	c, err := BearerTransport{BaseURL: srv.URL, Token: theToken, Rejected: rejected}.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	_, err = c.Apps(context.Background(), "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want an *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401 preserved", apiErr.StatusCode)
	}
	if apiErr.Message != rejected {
		t.Errorf("Message = %q, want the transport's own wording", apiErr.Message)
	}
	if apiErr.Code != "unauthorized" {
		t.Errorf("Code = %q, want the machine-readable code preserved", apiErr.Code)
	}
	if strings.Contains(err.Error(), theToken) {
		t.Error("the error carried the credential")
	}
}

// TestRefusedCredentialWithoutWordingStillSaysSomethingUseful covers the fallback, so a transport
// that names no wording of its own does not surface a bare status line.
func TestRefusedCredentialWithoutWordingStillSaysSomethingUseful(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, err := BearerTransport{BaseURL: srv.URL, Token: theToken}.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err = c.Apps(context.Background(), "")
	if err == nil {
		t.Fatal("a 401 produced no error")
	}
	if !strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "burrow auth login") {
		t.Errorf("error = %q, want the default to name revocation and the remedy", err)
	}
}

// TestForbiddenKeepsTheControlPlanesOwnWords. Only 401 is rewritten. A 403 is the control plane
// refusing an OPERATION — a suspended tenant, a guardrail — and telling that reader to sign in again
// would send them to fix something authentication cannot fix.
func TestForbiddenKeepsTheControlPlanesOwnWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"tenant is suspended","code":"forbidden"}`))
	}))
	defer srv.Close()

	c, err := BearerTransport{BaseURL: srv.URL, Token: theToken, Rejected: "sign in again"}.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err = c.Apps(context.Background(), "")
	if err == nil {
		t.Fatal("a 403 produced no error")
	}
	if !strings.Contains(err.Error(), "tenant is suspended") {
		t.Errorf("error = %q, want the control plane's own message for a 403", err)
	}
	if strings.Contains(err.Error(), "sign in again") {
		t.Errorf("error = %q; a 403 must not be reported as a credential problem", err)
	}
}

// TestBearerTransportFollowsNoRedirect. Following one would re-present the credential to whatever
// the redirect pointed at, and there is nothing legitimate for a control-plane API call to be
// redirected to.
func TestBearerTransportFollowsNoRedirect(t *testing.T) {
	var elsewhereHit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhereHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[]}`))
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c, err := BearerTransport{BaseURL: srv.URL, Token: theToken}.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.Apps(context.Background(), ""); err == nil {
		t.Fatal("a redirect was followed and reported as success")
	}
	if elsewhereHit {
		t.Error("the credential was re-presented to the redirect target")
	}
}
