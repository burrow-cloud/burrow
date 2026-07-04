// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"context"
	"net/http"
)

// Transport builds an authenticated control-plane API client (ADR-0045). It is the seam
// that decouples HOW the CLI reaches and authenticates to burrowd from WHAT it does once
// connected: the Client's ~40 request methods are auth-agnostic and reused unchanged, while
// each Transport implementation owns the endpoint and the credential.
//
// The open-source implementations are the kubeconfig API-server proxy (ADR-0014) and a
// direct control-plane URL with an API token; a separate private module can add other
// transports (for example a managed HTTPS endpoint behind SSO) by supplying an *http.Client
// whose RoundTripper carries the right credential — no fork of the request methods.
type Transport interface {
	// Connect returns a control-plane API client, resolving any credential it needs.
	Connect(ctx context.Context) (*Client, error)
}

// tokenRoundTripper adds Burrow's per-request headers — the X-Burrow-Token credential and, when
// set, the X-Burrow-Client-Version handshake header (ADR-0039) — to every request, wrapping an
// inner RoundTripper that performs the actual transport (a plain transport for the direct path, or
// client-go's kubeconfig-authenticated proxy transport for the in-cluster path).
//
// The token rides X-Burrow-Token only — never Authorization. On the API-server proxy path the
// kubeconfig transport (the inner RoundTripper) authenticates to the API server via the
// Authorization header, and client-go does not overwrite an Authorization header that is
// already set, so setting the token there would block the kubeconfig credential and the API
// server would reject the request. burrowd reads X-Burrow-Token, which the proxy forwards
// untouched; the direct/ingress path works the same way (ADR-0015). The client-version header
// rides alongside it and is likewise forwarded untouched by the proxy.
type tokenRoundTripper struct {
	token         string
	clientVersion string
	inner         http.RoundTripper
}

// NewTokenRoundTripper returns an http.RoundTripper that sets X-Burrow-Token to token — and, when
// clientVersion is non-empty, X-Burrow-Client-Version to it (ADR-0039) — before delegating to
// inner. A nil inner uses http.DefaultTransport. Both the kubeconfig transport and the direct-URL
// transport wrap their http.Client's transport with this so every self-host request carries the
// same headers on the wire (ADR-0015, ADR-0039).
//
// This is the single place every outbound control-plane request passes through, which is why the
// client-version handshake header lives here alongside the credential. An empty clientVersion omits
// the header, so a transport that does not know its version (or a test) sends no handshake rather
// than a misleading empty one; burrowd treats an absent header as a pre-handshake client and serves
// it (ADR-0039).
func NewTokenRoundTripper(token, clientVersion string, inner http.RoundTripper) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &tokenRoundTripper{token: token, clientVersion: clientVersion, inner: inner}
}

// RoundTrip sets the Burrow headers on a clone of req (a RoundTripper must not mutate the request
// it is given) and delegates to the inner transport.
func (t *tokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Burrow-Token", t.token)
	if t.clientVersion != "" {
		r.Header.Set("X-Burrow-Client-Version", t.clientVersion)
	}
	return t.inner.RoundTrip(r)
}

// DirectTransport talks to a control-plane URL directly (e.g. an ingress) with an API token,
// selected by --control-plane/--token (or BURROW_CONTROL_PLANE_URL/BURROW_API_TOKEN). NewClient
// wires the X-Burrow-Token RoundTripper, so the direct path sends the same header as the
// kubeconfig proxy path (ADR-0015, ADR-0045).
//
// It lives here rather than in the connect package because it needs only NewClient and no
// client-go, keeping this package client-go-free while remaining importable by both binaries and
// a private module (ADR-0045).
type DirectTransport struct {
	BaseURL string
	Token   string
	// Version is this client's release version, sent as X-Burrow-Client-Version so burrowd can make
	// version skew legible instead of opaque (ADR-0039). Empty omits the header.
	Version string
}

// Connect returns a client for the configured URL and token. It needs no context because the
// direct path resolves no credential; the parameter satisfies the Transport interface.
func (t DirectTransport) Connect(_ context.Context) (*Client, error) {
	return NewClientVersion(t.BaseURL, t.Token, t.Version), nil
}
