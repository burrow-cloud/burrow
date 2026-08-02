// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// BearerTransport reaches a control plane directly over HTTPS with a bearer credential (ADR-0045).
// It is the managed product's path: a tenant has no cluster and no kubeconfig, so there is no
// API-server proxy to route through and no install Secret to read a token out of — the credential
// signing in produced is presented on the request itself.
//
// The credential rides `Authorization: Bearer`, not X-Burrow-Token. The two headers are not
// interchangeable on this path: the managed control plane reads the bearer header to decide which
// tenant is calling, and strips X-Burrow-Token from the request before it reaches the per-tenant
// handler, precisely so a client-supplied value cannot reach an inner authorization check. Sending
// the credential in the header that gets stripped would authenticate nothing.
//
// It lives beside DirectTransport rather than in the connect package because it needs no client-go,
// which keeps this package importable by both binaries and by a private module.
type BearerTransport struct {
	// BaseURL is the control plane's origin, e.g. https://burrow-cloud.dev. It must be HTTPS: a
	// bearer credential is a password on every request, and Connect refuses to put one on the wire in
	// the clear.
	BaseURL string
	// Token is the stored credential. It is never rendered — not by Connect, not by an error from
	// this file, and not by the client this returns.
	Token string
	// Name is the calling binary (ClientNameCLI or ClientNameAgent), sent as X-Burrow-Client so a
	// too-old refusal names the binary the user must actually update (ADR-0039). Empty omits it.
	Name string
	// Version is this client's release version, sent as X-Burrow-Client-Version so the control plane
	// can make version skew legible instead of opaque (ADR-0039). Empty omits the header.
	Version string
	// InstallID is the install this caller expects to reach, sent as X-Burrow-Install (ADR-0084 §5).
	// It is empty for the managed product, which is one control plane rather than an install someone
	// stood up and could stand up again — there is no id to record and nothing to confuse it with.
	// The field is here because this is the second of the two places every outbound request passes
	// through, and a self-hosted burrowd given a front door of its own (ADR-0084 §7) is reached over
	// exactly this transport. Leaving the header out of one choke point would make the check hold
	// only for whichever route a caller happened to take.
	InstallID string
	// Rejected is what the caller is told when the control plane refuses the credential. It exists
	// because a bare 401 is the least useful thing a signed-in person can be shown: the credential
	// was accepted an hour ago, so what changed is worth naming — it was revoked from the console, or
	// the sign-in it came from no longer exists. Each binary supplies its own wording because the
	// remedy differs. Empty falls back to a generic sentence rather than showing the raw refusal.
	Rejected string
}

// Connect returns a client for the configured URL, presenting the bearer credential on every
// request. It needs no context because the credential is already resolved; the parameter satisfies
// the Transport interface.
func (t BearerTransport) Connect(_ context.Context) (*Client, error) {
	base, err := checkBearerBaseURL(t.BaseURL)
	if err != nil {
		return nil, err
	}
	if t.Token == "" {
		return nil, fmt.Errorf("client: no credential to authenticate to %s with", base)
	}
	hc := &http.Client{
		// No redirects. A redirect on an authenticated request would re-present the credential to
		// whatever it pointed at, and there is nothing legitimate for a control-plane API to redirect
		// an API call to, so a redirect is surfaced as the unexpected status it is.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &bearerRoundTripper{
			token:         t.Token,
			clientName:    t.Name,
			clientVersion: t.Version,
			installID:     t.InstallID,
			rejected:      t.Rejected,
		},
	}
	return NewClientWithHTTP(base, hc), nil
}

// checkBearerBaseURL parses the origin and refuses to send a bearer credential anywhere it would
// travel in the clear. Loopback is allowed over plain HTTP so a test can drive this against a local
// server without either weakening the rule or standing up TLS for it.
func checkBearerBaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("client: no control-plane URL to connect to")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("client: %q is not a usable control-plane URL: %w", raw, err)
	}
	switch {
	case u.Scheme == "https":
		return raw, nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return raw, nil
	default:
		return "", fmt.Errorf("client: refusing to send a credential to %s; the control-plane URL must be https", raw)
	}
}

// isLoopback reports whether the host is the local machine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bearerRoundTripper presents the credential on every request and makes a refusal legible.
type bearerRoundTripper struct {
	token         string
	clientName    string
	clientVersion string
	installID     string
	rejected      string
}

// RoundTrip sets the credential and the handshake headers on a clone of req (a RoundTripper must not
// mutate the request it is given), then rewrites a 401 body into something that says what actually
// went wrong.
//
// Only 401 is rewritten. A 403 is the control plane refusing an OPERATION — a suspended tenant, a
// guardrail — and its own words are the useful ones; replacing them with a sentence about
// credentials would send the reader to re-authenticate over something authentication cannot fix.
func (t *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	// Never both. X-Burrow-Token is the self-hosted credential header; sending it here would put the
	// same secret in a second header that the managed control plane strips, for no gain.
	r.Header.Del("X-Burrow-Token")
	if t.clientName != "" {
		r.Header.Set("X-Burrow-Client", t.clientName)
	}
	if t.clientVersion != "" {
		r.Header.Set("X-Burrow-Client-Version", t.clientVersion)
	}
	if t.installID != "" {
		r.Header.Set(InstallHeader, t.installID)
	}

	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	return withRejectedBody(resp, t.rejected), nil
}

// unauthorizedCode is the machine-readable code put on a rewritten 401, matching what the control
// plane itself uses, so a caller branching on the code sees no difference.
const unauthorizedCode = "unauthorized"

// defaultRejectedMessage is used when a transport names no wording of its own.
const defaultRejectedMessage = "the control plane did not accept this credential; it may have been revoked. Sign in again with \"burrow auth login\""

// withRejectedBody replaces a 401's body with the caller's wording, discarding the server's. The
// only 401 this path can produce is "we do not recognise that credential", which is the one thing
// the caller already knows; what it does not know is that revoking is a thing that happens and where
// to go next. The original body is drained and closed so the connection can be reused.
func withRejectedBody(resp *http.Response, message string) *http.Response {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()

	if message == "" {
		message = defaultRejectedMessage
	}
	body, err := json.Marshal(struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{Error: message, Code: unauthorizedCode})
	if err != nil { // unreachable for two strings, but a nil body would panic the reader
		body = []byte(`{"code":"` + unauthorizedCode + `"}`)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp
}
