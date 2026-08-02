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

// The binary names a client identifies itself by in the X-Burrow-Client half of the handshake
// (ADR-0039). burrowd needs the NAME as well as the version because Burrow ships two client
// binaries that are installed together but can drift apart on disk, and the remedy for a stale
// one differs: refusing `burrow-agent` and telling the user to update the `burrow` CLI sends them
// somewhere that cannot fix it. The values are the executable names, so what the message prints is
// what the user types.
const (
	// ClientNameCLI is the human admin CLI (ADR-0049 §1).
	ClientNameCLI = "burrow"
	// ClientNameAgent is the agent's scoped control channel (ADR-0049 §1).
	ClientNameAgent = "burrow-agent"
)

// InstallHeader carries the install id the caller expects the control plane to be (ADR-0084 §5). It
// is the request half of the install check: burrowd knows its own id, the client sends the one its
// target recorded, and a difference is refused with a message naming both instead of a command
// quietly acting on a cluster nobody meant to reach.
//
// It is a header rather than part of the credential for the same reason X-Burrow-Client and
// X-Burrow-Client-Version are: it says something about the CALL, not about who is making it. The id
// authorises nothing — it identifies an install — and Authorization is unavailable on the
// API-server proxy path in any case, where client-go owns that header to authenticate to the API
// server itself (ADR-0015). Like the rest of the X-Burrow- family it survives the proxy untouched.
//
// It is exported because both round trippers in this package set it and the control plane's
// middleware reads it; keeping one spelling in one place is the point.
const InstallHeader = "X-Burrow-Install"

// tokenRoundTripper adds Burrow's per-request headers — the X-Burrow-Token credential and, when
// set, the X-Burrow-Client and X-Burrow-Client-Version handshake headers (ADR-0039) and the
// X-Burrow-Install expectation (ADR-0084 §5) — to every request, wrapping an inner RoundTripper
// that performs the actual transport (a plain transport for the direct path, or client-go's
// kubeconfig-authenticated proxy transport for the in-cluster path).
//
// The token rides X-Burrow-Token only — never Authorization. On the API-server proxy path the
// kubeconfig transport (the inner RoundTripper) authenticates to the API server via the
// Authorization header, and client-go does not overwrite an Authorization header that is
// already set, so setting the token there would block the kubeconfig credential and the API
// server would reject the request. burrowd reads X-Burrow-Token, which the proxy forwards
// untouched; the direct/ingress path works the same way (ADR-0015). The client-name and
// client-version headers ride alongside it and are likewise forwarded untouched by the proxy.
type tokenRoundTripper struct {
	token         string
	clientName    string
	clientVersion string
	installID     string
	inner         http.RoundTripper
}

// NewTokenRoundTripper returns an http.RoundTripper that sets X-Burrow-Token to token — and, when
// clientVersion is non-empty, X-Burrow-Client-Version to it (ADR-0039) — before delegating to
// inner. It sends no client NAME; a client that knows which binary it is should use
// NewNamedTokenRoundTripper so burrowd can name the right binary when it refuses a stale one.
// A nil inner uses http.DefaultTransport.
func NewTokenRoundTripper(token, clientVersion string, inner http.RoundTripper) http.RoundTripper {
	return NewNamedTokenRoundTripper(token, "", clientVersion, inner)
}

// NewNamedTokenRoundTripper is NewTokenRoundTripper plus the client-NAME half of the ADR-0039
// handshake: it also sets X-Burrow-Client to clientName (ClientNameCLI or ClientNameAgent) so
// burrowd's too-old refusal can name the binary that is actually stale rather than guessing.
// Both the kubeconfig transport and the direct-URL transport wrap their http.Client's transport
// with this, so every self-host request carries the same headers on the wire (ADR-0015, ADR-0039).
//
// This is the single place every outbound control-plane request passes through, which is why the
// handshake headers live here alongside the credential. An empty name or version omits its header,
// so a transport that does not know what it is (or a test) sends nothing rather than something
// misleading; burrowd treats an absent header as a pre-handshake client and serves it (ADR-0039).
func NewNamedTokenRoundTripper(token, clientName, clientVersion string, inner http.RoundTripper) http.RoundTripper {
	return NewInstallTokenRoundTripper(token, clientName, clientVersion, "", inner)
}

// NewInstallTokenRoundTripper is NewNamedTokenRoundTripper plus the install the caller expects to be
// talking to, sent in X-Burrow-Install (ADR-0084 §5). A target that records an install id supplies
// it here, and the control plane refuses a request that names an install it is not, so a cluster
// rebuilt under a name a provider generates deterministically is caught on arrival rather than
// deployed into.
//
// An empty installID omits the header, which is the ordinary case and must stay served: a target
// recorded before install ids existed has none, and a control plane older than this check ignores
// the header if one is sent. Skew in both directions keeps working.
func NewInstallTokenRoundTripper(token, clientName, clientVersion, installID string, inner http.RoundTripper) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &tokenRoundTripper{token: token, clientName: clientName, clientVersion: clientVersion, installID: installID, inner: inner}
}

// RoundTrip sets the Burrow headers on a clone of req (a RoundTripper must not mutate the request
// it is given) and delegates to the inner transport.
func (t *tokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Burrow-Token", t.token)
	if t.clientName != "" {
		r.Header.Set("X-Burrow-Client", t.clientName)
	}
	if t.clientVersion != "" {
		r.Header.Set("X-Burrow-Client-Version", t.clientVersion)
	}
	if t.installID != "" {
		r.Header.Set(InstallHeader, t.installID)
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
	// Name is the calling binary (ClientNameCLI or ClientNameAgent), sent as X-Burrow-Client so a
	// too-old refusal names the binary the user must actually update (ADR-0039). Empty omits it.
	Name string
	// Version is this client's release version, sent as X-Burrow-Client-Version so burrowd can make
	// version skew legible instead of opaque (ADR-0039). Empty omits the header.
	Version string
	// InstallID is the install this caller expects to reach, sent as X-Burrow-Install so a control
	// plane that is a different install refuses the request and says so (ADR-0084 §5). Empty omits
	// the header, and is served.
	InstallID string
}

// Connect returns a client for the configured URL and token. It needs no context because the
// direct path resolves no credential; the parameter satisfies the Transport interface.
func (t DirectTransport) Connect(_ context.Context) (*Client, error) {
	// The http.Client is assembled here rather than through NewNamedClient so the install-id header
	// rides the same round tripper as the credential and the handshake, with no fifth positional
	// constructor. With an empty InstallID this is exactly what NewNamedClient builds.
	hc := &http.Client{
		Transport: NewInstallTokenRoundTripper(t.Token, t.Name, t.Version, t.InstallID, nil),
	}
	return NewClientWithHTTP(t.BaseURL, hc), nil
}
