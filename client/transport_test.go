// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client_test

import (
	"context"
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
