// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// poisonDefaultClient installs a RoundTripper on http.DefaultClient that stamps a Burrow credential
// on every request, and restores the default client afterwards. It stands in for anything in the
// process writing to that global — which is exactly what the connect package used to do (issue
// #459) — so a call that reaches a third party through http.DefaultClient is caught here rather than
// discovered in someone's proxy logs.
func poisonDefaultClient(t *testing.T) {
	t.Helper()
	before := http.DefaultClient.Transport
	http.DefaultClient.Transport = stampRoundTripper{inner: http.DefaultTransport}
	t.Cleanup(func() { http.DefaultClient.Transport = before })
}

type stampRoundTripper struct{ inner http.RoundTripper }

func (s stampRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Burrow-Token", "leaked-control-plane-token")
	return s.inner.RoundTrip(r)
}

// assertNoBurrowHeaders fails when a request that reached a third party carried anything from the
// X-Burrow- family.
func assertNoBurrowHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	for _, name := range []string{"X-Burrow-Token", "X-Burrow-Client", "X-Burrow-Client-Version", "X-Burrow-Install"} {
		if v := h.Get(name); v != "" {
			t.Errorf("%s sent %s: %q to a third party; it must use a client no Burrow credential can reach", what, name, v)
		}
	}
}

// TestLatestReleaseCheckSendsNoBurrowHeaders covers the GitHub release check behind `burrow version`.
// It reaches api.github.com, which has no business seeing a control-plane credential. The test points
// the fetch at a local server so nothing leaves the machine.
func TestLatestReleaseCheckSendsNoBurrowHeaders(t *testing.T) {
	poisonDefaultClient(t)

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer srv.Close()

	orig := latestReleaseURL
	latestReleaseURL = srv.URL
	t.Cleanup(func() { latestReleaseURL = orig })

	tag, err := fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if tag != "v9.9.9" {
		t.Errorf("tag = %q, want v9.9.9", tag)
	}
	assertNoBurrowHeaders(t, "the latest-release check", got)
}

// TestFetchManifestSendsNoBurrowHeaders covers the URL manifest fetch behind `apply`. The URL comes
// from the caller and can point anywhere, so this is the path that would hand the control-plane
// token to an arbitrary host.
func TestFetchManifestSendsNoBurrowHeaders(t *testing.T) {
	poisonDefaultClient(t)

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("kind: Namespace\n"))
	}))
	defer srv.Close()

	body, err := fetchManifest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if body != "kind: Namespace\n" {
		t.Errorf("body = %q, want the served manifest", body)
	}
	assertNoBurrowHeaders(t, "the manifest URL fetch", got)
}

// TestPublicIPDetectorSendsNoBurrowHeaders covers the public-IP echo service the VPS bootstrap
// queries. It is a third party like the other two and is wired to the same client.
func TestPublicIPDetectorSendsNoBurrowHeaders(t *testing.T) {
	poisonDefaultClient(t)

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("203.0.113.7"))
	}))
	defer srv.Close()

	ip, err := echoIPDetector{url: srv.URL, client: outboundHTTPClient}.DetectPublicIP(context.Background())
	if err != nil {
		t.Fatalf("DetectPublicIP: %v", err)
	}
	if ip != "203.0.113.7" {
		t.Errorf("ip = %q, want 203.0.113.7", ip)
	}
	assertNoBurrowHeaders(t, "the public-IP detector", got)
}

// TestOutboundClientIsNotTheDefaultClient states the property the three tests above rely on: the
// client third-party calls travel on is one this binary owns, so no write to http.DefaultClient
// anywhere in the process can reach them.
func TestOutboundClientIsNotTheDefaultClient(t *testing.T) {
	if outboundHTTPClient == http.DefaultClient {
		t.Fatal("outboundHTTPClient is http.DefaultClient; third-party calls would inherit anything installed on the global default")
	}
	if outboundHTTPClient.Transport != nil {
		t.Errorf("outboundHTTPClient.Transport = %T, want nil (a plain client with no RoundTripper that could add a credential)", outboundHTTPClient.Transport)
	}
}

// TestDetectorUsesTheOutboundClient confirms the production detector is wired to the owned client
// rather than the global default, since the detector is built behind a seam and the wiring is the
// part a refactor could quietly undo.
func TestDetectorUsesTheOutboundClient(t *testing.T) {
	d, ok := newIPDetector().(echoIPDetector)
	if !ok {
		t.Fatalf("newIPDetector returned %T, want echoIPDetector", newIPDetector())
	}
	if d.client != outboundHTTPClient {
		t.Errorf("the public-IP detector uses %p, want the owned outbound client", d.client)
	}
}
