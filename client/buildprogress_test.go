// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/client"
)

// The client half of the build progress stream (issue #503). Asking for progress must change NOTHING
// a caller can otherwise observe — the same result, the same *APIError, and a control plane that does
// not offer a stream handled without the caller noticing — and it must make a build that runs longer
// than a proxy's read timeout survivable, which is the reason the stream exists at all.

// buildRequest is a valid build: a git source and an explicit push target.
func buildRequest(progress func(client.DeployProgress)) client.BuildRequest {
	return client.BuildRequest{
		Source:      client.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		TargetImage: "ghcr.io/acme/web:1.0.0",
		Progress:    progress,
	}
}

// buildResultLine is a terminal result line carrying a digest and the deploy it ended in.
const buildResultLine = `{"result":{"digest":"sha256:abc","deploy":{"release":{"id":"r1","app":"web","image":"ghcr.io/acme/web:1.0.0@sha256:abc","status":"deployed"}}}}`

// TestBuildDecodesTheProgressStream is the happy path: the client asks for ndjson, reports each stage
// as it arrives — the build's own and then the deploy's, one sequence — and returns the result from
// the stream's last line.
func TestBuildDecodesTheProgressStream(t *testing.T) {
	var accept string
	srv := ndjsonServer(t, &accept,
		`{"event":{"stage":"clone","status":"started"}}`,
		`{"event":{"stage":"clone","status":"done"}}`,
		`{"event":{"stage":"build","status":"started"}}`,
		`{"event":{"stage":"build","status":"progressing"}}`,
		`{"event":{"stage":"build","status":"done"}}`,
		`{"event":{"stage":"apply","status":"started"}}`,
		`{"event":{"stage":"apply","status":"done"}}`,
		buildResultLine,
	)

	var got []string
	c := client.NewClient(srv.URL, "tok")
	res, err := c.Build(context.Background(), "web", buildRequest(recordProgress(&got)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if accept != "application/x-ndjson" {
		t.Errorf("Accept = %q, want application/x-ndjson", accept)
	}
	want := "clone:started,clone:done,build:started,build:progressing,build:done,apply:started,apply:done"
	if strings.Join(got, ",") != want {
		t.Errorf("stages = %v, want %s", got, want)
	}
	if res.Digest != "sha256:abc" || res.Deploy.Release.ID != "r1" {
		t.Errorf("result = %+v", res)
	}
}

// TestBuildWithoutProgressAsksForNoStream pins the opt-in. A caller that sets no reporter — which is
// `burrow-agent` — sends no ndjson Accept header and takes the path this client has always taken.
func TestBuildWithoutProgressAsksForNoStream(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"digest":"sha256:abc","deploy":{"release":{"id":"r1"}}}`)
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Build(context.Background(), "web", buildRequest(nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(accept, "x-ndjson") {
		t.Errorf("Accept = %q, want no ndjson: a caller that set no Progress must not ask for one", accept)
	}
	if res.Digest != "sha256:abc" || res.Deploy.Release.ID != "r1" {
		t.Errorf("result = %+v", res)
	}
}

// TestBuildAgainstAControlPlaneWithNoStream is the compatibility case an install mid-upgrade is in: a
// server that predates this ignores the Accept header and answers with the single JSON object it
// always has. The client must take that answer, report no stages, and succeed.
func TestBuildAgainstAControlPlaneWithNoStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"digest":"sha256:abc","deploy":{"release":{"id":"r1"}}}`)
	}))
	defer srv.Close()

	var got []string
	c := client.NewClient(srv.URL, "tok")
	res, err := c.Build(context.Background(), "web", buildRequest(recordProgress(&got)))
	if err != nil {
		t.Fatalf("Build against a control plane with no stream: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("stages = %v, want none: the server reported none", got)
	}
	if res.Digest != "sha256:abc" {
		t.Errorf("result = %+v", res)
	}
}

// TestAHeldBuildIsAnAPIErrorWithProgressSet covers the refusal a build is most likely to meet. It
// arrives in the stream rather than status-coded, because a build's guardrail is the deploy's and the
// deploy happens after the build — so what matters is that the *APIError rebuilt from the stream line
// is indistinguishable from the status-coded one, NeedsConfirmation included.
func TestAHeldBuildIsAnAPIErrorWithProgressSet(t *testing.T) {
	srv := ndjsonServer(t, nil,
		`{"event":{"stage":"clone","status":"started"}}`,
		`{"event":{"stage":"clone","status":"done"}}`,
		`{"event":{"stage":"build","status":"started"}}`,
		`{"event":{"stage":"build","status":"done"}}`,
		`{"error":{"status":422,"error":"app.deploy is held for confirmation","code":"app.deploy","needs_confirmation":true}}`,
	)

	var got []string
	c := client.NewClient(srv.URL, "tok")
	_, err := c.Build(context.Background(), "web", buildRequest(recordProgress(&got)))
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("Build err = %v, want an *APIError", err)
	}
	if api.StatusCode != http.StatusUnprocessableEntity || api.Code != "app.deploy" || !api.NeedsConfirmation {
		t.Errorf("APIError = %+v, want 422 app.deploy with needs_confirmation", api)
	}
	if strings.Join(got, ",") != "clone:started,clone:done,build:started,build:done" {
		t.Errorf("stages = %v, want the build's stages up to the hold", got)
	}
}

// TestABuildOutlivesAShortReadTimeout is the bug, reproduced and fixed in one test. A build is put
// behind a proxy whose read timeout is far shorter than the build takes — which is every real
// install, since a build routinely runs for minutes and 60 seconds is ingress-nginx's default.
//
// The silent build is what issue #503 hit against a live cluster: the response begins only when the
// build is over, the proxy gives up long before that, and the caller learns nothing even though the
// build succeeded. The streaming build answers immediately and finishes normally, because the first
// stage is written when the build STARTS.
func TestABuildOutlivesAShortReadTimeout(t *testing.T) {
	const readTimeout = 50 * time.Millisecond
	const buildTakes = 500 * time.Millisecond

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "x-ndjson") {
			// The build this endpoint has always served: one JSON object, written when the build is
			// over and not a byte before.
			time.Sleep(buildTakes)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"digest":"sha256:abc","deploy":{"release":{"id":"r1"}}}`)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"event":{"stage":"clone","status":"started"}}`+"\n")
		w.(http.Flusher).Flush()
		time.Sleep(buildTakes)
		_, _ = io.WriteString(w, buildResultLine+"\n")
		w.(http.Flusher).Flush()
	}))
	defer backend.Close()

	// A proxy in front of the control plane, with a read timeout an order of magnitude shorter than
	// the build.
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parsing the backend URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{ResponseHeaderTimeout: readTimeout}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Gateway Time-out", http.StatusGatewayTimeout)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	c := client.NewClient(front.URL, "tok")

	// Without progress the caller never hears the answer, however well the build goes.
	if _, err := c.Build(context.Background(), "web", buildRequest(nil)); err == nil {
		t.Fatal("a silent build survived a read timeout shorter than the build; the test proves nothing")
	}

	// With progress it completes and carries its result.
	var got []string
	res, err := c.Build(context.Background(), "web", buildRequest(recordProgress(&got)))
	if err != nil {
		t.Fatalf("a streaming build behind a %s read timeout: %v", readTimeout, err)
	}
	if res.Digest != "sha256:abc" || res.Deploy.Release.ID != "r1" {
		t.Errorf("result = %+v", res)
	}
	if len(got) == 0 {
		t.Error("the build reported no stages")
	}
}
