// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/burrow-cloud/burrow/client"
)

// TestTokenRoundTripperSetsHeader confirms the X-Burrow-Token RoundTripper adds the token header
// to every outgoing request and never sets Authorization (the token rides X-Burrow-Token only,
// ADR-0015). A client built on an http.Client wrapped in the RoundTripper authenticates without
// the Client itself knowing the credential (ADR-0045).
func TestTokenRoundTripperSetsHeader(t *testing.T) {
	var gotToken, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotAuth = r.Header.Get("X-Burrow-Token"), r.Header.Get("Authorization")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	hc := &http.Client{Transport: client.NewTokenRoundTripper("s3cr3t", "", nil)}
	c := client.NewClientWithHTTP(srv.URL, hc)
	if _, err := c.ListEnvironments(context.Background()); err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if gotToken != "s3cr3t" {
		t.Errorf("X-Burrow-Token = %q, want s3cr3t", gotToken)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (the token must ride X-Burrow-Token only, ADR-0015)", gotAuth)
	}
}

// TestTokenRoundTripperSendsClientVersion confirms a non-empty client version rides
// X-Burrow-Client-Version on every request (the ADR-0039 handshake), and that an empty version
// omits the header rather than sending an empty one — burrowd treats an absent header as a
// pre-handshake client.
func TestTokenRoundTripperSendsClientVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		want    string
	}{
		{name: "set", version: "v1.2.3", want: "v1.2.3"},
		{name: "empty omits header", version: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var present bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Burrow-Client-Version")
				_, present = r.Header["X-Burrow-Client-Version"]
				_, _ = w.Write([]byte("{}"))
			}))
			defer srv.Close()

			hc := &http.Client{Transport: client.NewTokenRoundTripper("tok", tc.version, nil)}
			c := client.NewClientWithHTTP(srv.URL, hc)
			if _, err := c.ListEnvironments(context.Background()); err != nil {
				t.Fatalf("ListEnvironments: %v", err)
			}
			if got != tc.want {
				t.Errorf("X-Burrow-Client-Version = %q, want %q", got, tc.want)
			}
			if tc.version == "" && present {
				t.Errorf("X-Burrow-Client-Version was sent for an empty version; want the header absent")
			}
		})
	}
}

// TestClientWithoutTokenTransportSendsNoToken confirms an auth-agnostic client built on a plain
// http.Client sends no token header: authentication is the RoundTripper's job, not the Client's
// (ADR-0045). This is the seam a non-token transport (e.g. SSO bearer) relies on.
func TestClientWithoutTokenTransportSendsNoToken(t *testing.T) {
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		if got := r.Header.Get("X-Burrow-Token"); got != "" {
			t.Errorf("X-Burrow-Token = %q, want empty for a client with no token transport", got)
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := client.NewClientWithHTTP(srv.URL, &http.Client{})
	if _, err := c.ListEnvironments(context.Background()); err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if !seen {
		t.Fatalf("server was not reached")
	}
}

// TestDirectTransportConnect confirms the direct-URL transport (ADR-0045) returns a client for its
// URL that carries the token in X-Burrow-Token, the same header the kubeconfig proxy path sends
// (ADR-0015). It resolves no credential, so Connect ignores its context.
func TestDirectTransportConnect(t *testing.T) {
	var gotToken, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Burrow-Token")
		gotVersion = r.Header.Get("X-Burrow-Client-Version")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, err := client.DirectTransport{BaseURL: srv.URL, Token: "s3cr3t", Version: "v9.9.9"}.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.ListEnvironments(context.Background()); err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if gotToken != "s3cr3t" {
		t.Errorf("X-Burrow-Token = %q, want s3cr3t", gotToken)
	}
	if gotVersion != "v9.9.9" {
		t.Errorf("X-Burrow-Client-Version = %q, want v9.9.9 (the DirectTransport Version, ADR-0039)", gotVersion)
	}
}

// TestNamedTokenRoundTripperSendsClientName confirms the client-NAME half of the ADR-0039 handshake
// rides X-Burrow-Client, and that an unnamed client omits the header rather than sending an empty
// one. burrowd needs the name because Burrow ships two client binaries whose remedies differ: the
// refusal for a stale burrow-agent must not send the user to the CLI's Homebrew upgrade as if the
// CLI were at fault (issue #308).
func TestNamedTokenRoundTripperSendsClientName(t *testing.T) {
	for _, tc := range []struct {
		name       string
		clientName string
		present    bool
	}{
		{name: "the agent binary names itself", clientName: client.ClientNameAgent, present: true},
		{name: "the CLI names itself", clientName: client.ClientNameCLI, present: true},
		{name: "an unnamed client omits the header", clientName: "", present: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var present bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Burrow-Client")
				_, present = r.Header["X-Burrow-Client"]
				_, _ = w.Write([]byte("{}"))
			}))
			defer srv.Close()

			hc := &http.Client{Transport: client.NewNamedTokenRoundTripper("tok", tc.clientName, "v1.2.3", nil)}
			c := client.NewClientWithHTTP(srv.URL, hc)
			if _, err := c.ListEnvironments(context.Background()); err != nil {
				t.Fatalf("ListEnvironments: %v", err)
			}
			if got != tc.clientName {
				t.Errorf("X-Burrow-Client = %q, want %q", got, tc.clientName)
			}
			if present != tc.present {
				t.Errorf("X-Burrow-Client present = %v, want %v", present, tc.present)
			}
		})
	}
	// The names are the executable names, so a message that prints one prints what the user types.
	if client.ClientNameCLI != "burrow" || client.ClientNameAgent != "burrow-agent" {
		t.Errorf("client names = %q/%q, want the executable names burrow/burrow-agent",
			client.ClientNameCLI, client.ClientNameAgent)
	}
}

// TestAPIErrorCarriesServerVersion confirms the control plane's release version on a client_too_old
// refusal reaches the caller as a field, not only inside prose. A client rewrites the remedy locally
// (it alone knows how it was installed) and needs the target version to name in that remedy.
func TestAPIErrorCarriesServerVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = w.Write([]byte(`{"error":"too old","code":"client_too_old","server_version":"v0.13.0"}`))
	}))
	defer srv.Close()

	c := client.NewNamedClient(srv.URL, "tok", client.ClientNameAgent, "v0.1.0")
	_, err := c.ListEnvironments(context.Background())
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if api.Code != client.CodeClientTooOld {
		t.Errorf("code = %q, want %q", api.Code, client.CodeClientTooOld)
	}
	if api.ServerVersion != "v0.13.0" {
		t.Errorf("ServerVersion = %q, want v0.13.0", api.ServerVersion)
	}
}

// TestInstallRoundTripperSendsInstallHeader confirms the install a caller expects to reach rides
// X-Burrow-Install on every request (ADR-0084 §5), and that an empty id omits the header entirely
// rather than sending a blank one. The absent header is what keeps a target recorded before install
// ids existed working: burrowd serves a request that claims nothing.
func TestInstallRoundTripperSendsInstallHeader(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installID string
		want      string
	}{
		{name: "set", installID: "abc123", want: "abc123"},
		{name: "empty omits header", installID: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var present bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get(client.InstallHeader)
				_, present = r.Header[client.InstallHeader]
				_, _ = w.Write([]byte("{}"))
			}))
			defer srv.Close()

			hc := &http.Client{Transport: client.NewInstallTokenRoundTripper("tok", client.ClientNameCLI, "v1.0.0", tc.installID, nil)}
			c := client.NewClientWithHTTP(srv.URL, hc)
			if _, err := c.ListEnvironments(context.Background()); err != nil {
				t.Fatalf("ListEnvironments: %v", err)
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", client.InstallHeader, got, tc.want)
			}
			if tc.installID == "" && present {
				t.Errorf("%s was sent for an empty install id; want the header absent", client.InstallHeader)
			}
		})
	}
}

// TestDirectTransportSendsInstallID confirms the direct-URL transport carries an install id it is
// given alongside the credential, so the check is a property of every transport in this package
// rather than of one route. It asserts what THIS transport does with a configured id; whether a
// given caller has one to configure is that caller's business (the `burrow` CLI's --control-plane
// path resolves no target, so it has none).
func TestDirectTransportSendsInstallID(t *testing.T) {
	var gotInstall string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotInstall = r.Header.Get(client.InstallHeader)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, err := client.DirectTransport{BaseURL: srv.URL, Token: "s3cr3t", InstallID: "abc123"}.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := c.ListEnvironments(context.Background()); err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if gotInstall != "abc123" {
		t.Errorf("%s = %q, want abc123", client.InstallHeader, gotInstall)
	}
}

// TestAPIErrorCarriesServerInstallID confirms the id of the install that actually answered survives
// onto the structured error (ADR-0084 §5), the same way the too-old refusal carries server_version:
// a caller can re-point a target at what is really there without parsing the message.
func TestAPIErrorCarriesServerInstallID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"wrong install","code":"install_mismatch","server_install_id":"abc123"}`))
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	_, err := c.ListEnvironments(context.Background())
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %v, want an *client.APIError", err)
	}
	if api.Code != client.CodeInstallMismatch {
		t.Errorf("code = %q, want %q", api.Code, client.CodeInstallMismatch)
	}
	if api.ServerInstallID != "abc123" {
		t.Errorf("ServerInstallID = %q, want abc123", api.ServerInstallID)
	}
}
