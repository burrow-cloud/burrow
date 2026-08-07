// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// readTimeoutProxy is a reverse proxy that gives up when the upstream goes quiet, which is what a
// real one does and what makes issue #503 a bug rather than an inconvenience: nginx's
// proxy_read_timeout is measured BETWEEN TWO SUCCESSIVE READS, not over the whole response. So it
// enforces the deadline twice — on the wait for the response header, and on every wait for the next
// body bytes — and abandons the response mid-flight when either elapses, exactly as a proxy does.
func readTimeoutProxy(t *testing.T, upstream string, idle time.Duration) *httptest.Server {
	t.Helper()
	transport := &http.Transport{ResponseHeaderTimeout: idle}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.Path, r.Body)
		if err != nil {
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := transport.RoundTrip(req)
		if err != nil {
			http.Error(w, "Gateway Time-out", http.StatusGatewayTimeout)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		w.(http.Flusher).Flush()

		type read struct {
			b   []byte
			err error
		}
		reads := make(chan read)
		go func() {
			defer close(reads)
			buf := make([]byte, 4096)
			for {
				n, err := resp.Body.Read(buf)
				reads <- read{b: append([]byte(nil), buf[:n]...), err: err}
				if err != nil {
					return
				}
			}
		}()
		for {
			select {
			case got, ok := <-reads:
				if !ok {
					return
				}
				_, _ = w.Write(got.b)
				w.(http.Flusher).Flush()
				if got.err != nil {
					return
				}
			case <-time.After(idle):
				// The upstream said nothing for a whole read timeout. The proxy drops the response
				// where it stands, which is what leaves the caller with a truncated body.
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestABuildOutlivesAShortReadTimeout is the bug, reproduced and fixed in one test. A build is put
// behind a proxy whose read timeout is far shorter than the build takes — which is every real
// install, since a build routinely runs for minutes and 60 seconds is ingress-nginx's default.
//
// The silent build is what issue #503 hit against a live cluster: the response begins only when the
// build is over, the proxy gives up long before that, and the caller learns nothing even though the
// build succeeded. The streaming build survives BOTH halves of the timeout — it answers immediately,
// because the first stage is written when the build starts, and it keeps writing while the build
// runs, because a stream that goes quiet for a whole read timeout is dropped just the same.
func TestABuildOutlivesAShortReadTimeout(t *testing.T) {
	// The read timeout is generous relative to how often the stream writes, so the test asserts the
	// property rather than the scheduler: the stream reports six times per timeout, and a runner would
	// have to stall for six intervals running to make this fail for a reason that is not the bug.
	const readTimeout = 300 * time.Millisecond
	const buildTakes = 600 * time.Millisecond

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
		// The long stage, reported the way the control plane reports it: the running stage repeats
		// itself, and the stream's keepalive fills any silence that is left.
		for elapsed := time.Duration(0); elapsed < buildTakes; elapsed += readTimeout / 6 {
			time.Sleep(readTimeout / 6)
			_, _ = io.WriteString(w, "\n")
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, buildResultLine+"\n")
		w.(http.Flusher).Flush()
	}))
	defer backend.Close()

	front := readTimeoutProxy(t, backend.URL, readTimeout)
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

// TestAStreamThatGoesQuietIsDroppedByTheProxy is the control for the test above: with the keepalive
// removed and nothing else changed, the same streaming response dies in the same place. Without this,
// a passing test above could mean the proxy was never enforcing anything.
func TestAStreamThatGoesQuietIsDroppedByTheProxy(t *testing.T) {
	const readTimeout = 200 * time.Millisecond

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"event":{"stage":"clone","status":"started"}}`+"\n")
		w.(http.Flusher).Flush()
		time.Sleep(5 * readTimeout) // the build works, silently
		_, _ = io.WriteString(w, buildResultLine+"\n")
		w.(http.Flusher).Flush()
	}))
	defer backend.Close()

	front := readTimeoutProxy(t, backend.URL, readTimeout)
	c := client.NewClient(front.URL, "tok")

	var got []string
	_, err := c.Build(context.Background(), "web", buildRequest(recordProgress(&got)))
	if err == nil {
		t.Fatal("a stream silent for five read timeouts was delivered; the proxy is not enforcing one")
	}
	// And the client says what it does not know, rather than reporting a build that did not happen.
	if !strings.Contains(err.Error(), "may still be in progress") {
		t.Errorf("err = %v, want it to say the operation may still be running", err)
	}
}
