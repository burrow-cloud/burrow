// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// A deploy takes ten to twenty seconds and every stage of it happens server-side, so the client
// cannot narrate it (issue #480). An in-cluster build takes minutes and is worse: a response that
// starts only when the build ends never arrives at all, because every reverse proxy in front of a
// control plane has a read timeout and 60 seconds is the common default (issue #503). This is how the
// control plane says what it is doing over the call the client is already blocked on, for both.
//
// IT IS THE SAME ENDPOINT, OPT-IN PER REQUEST. A client that wants the stages sends
// `Accept: application/x-ndjson`; a control plane that predates this ignores an Accept header it
// does not act on and answers exactly as it always has, so a new client against an old server keeps
// working — which is why the client checks the response Content-Type rather than assuming it got
// what it asked for.
//
// THE RESPONSE HEADER IS WRITTEN LAZILY, on the first event, and that is the load-bearing part.
// Everything a deploy can be refused for happens before the first stage runs: validation, the
// environment, the replica ceiling, and above all the guardrail decision that produces
// `held_for_confirmation` and `denied`. Writing 200 up front would turn every one of those into a
// success with an error inside it — and `burrow-agent` classifies a hold by the *client.APIError's
// status and NeedsConfirmation, so a held deploy arriving as a 200 stream would be relayed to the
// human as a failure. Nothing is written until the engine has committed to doing work.

// ndjsonMediaType is the content type of the progress stream: one JSON object per line.
const ndjsonMediaType = "application/x-ndjson"

// streamLine is one line of the stream. Exactly one field is ever set: an event while the operation
// runs, then a single terminal line that is either the result or the error. The wrapper keys are what
// let a reader tell the three apart without guessing at a shape.
//
// Result is an interface because the same framing carries a deploy's result and a build's; what a
// reader has to do with it — read the key, decode the value — is identical either way.
type streamLine struct {
	Event  *controlplane.DeployEvent `json:"event,omitempty"`
	Result any                       `json:"result,omitempty"`
	Error  *streamError              `json:"error,omitempty"`
}

// streamError is an error raised AFTER the first event — a failed `pre-deploy` hook, a failed apply,
// a store failure — carried in the stream because the status line has already gone out. It is the
// ordinary error body plus the status code that body would have been written with, so the client
// rebuilds the identical *APIError and every caller behaves the same either way.
type streamError struct {
	Status int `json:"status"`
	errorResponse
}

// wantsProgressStream reports whether the caller asked for the ndjson progress stream. It parses the
// Accept header rather than matching a substring so a parameterised or multi-valued header
// ("application/x-ndjson;q=1, application/json") is read correctly, and an unparseable entry is
// skipped rather than failing the request — a malformed Accept is not a reason to refuse a deploy.
func wantsProgressStream(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err == nil && mt == ndjsonMediaType {
			return true
		}
	}
	return false
}

// streamKeepalive is how long an open stream may stay silent before it writes something to prove the
// connection is alive. It is chosen against the timeout it exists to survive: ingress-nginx's default
// proxy read timeout is 60 seconds, measured between two successive reads.
const streamKeepalive = 20 * time.Second

// progressStream is the ndjson writer both streaming operations share: the lazily-written header, one
// flushed line per event, the keepalive, and the single terminal line. There is exactly ONE of these
// because there is exactly one framing — a deploy and a build differ in the work they do and the
// result they carry, not in how either is put on the wire.
type progressStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	enc     *json.Encoder
	// interval is how long this stream may stay silent, held per stream rather than read from the
	// package so a test can shorten it without a mutable global. It is set before the first write and
	// never changed after.
	interval time.Duration
	// mu guards the writer. The engine reports on the serving goroutine, but the keepalive ticks on
	// its own, so the two must not write at once.
	mu     sync.Mutex
	open   bool
	closed bool
	done   chan struct{}
}

// newProgressStream returns a stream over w, or ok=false when w cannot flush — the caller then serves
// the ordinary buffered response instead. That is today's behaviour rather than a failure: the caller
// gets its answer at the end instead of the stages as they happen. Every writer on the real serving
// path flushes — the API-server service proxy this travels through streams fine, and no middleware on
// a matched /v1 route wraps the writer.
//
// The caller MUST close the returned stream before its handler returns: writing to a ResponseWriter
// after that is not allowed, and the keepalive is what would do it.
func newProgressStream(w http.ResponseWriter) (*progressStream, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	return &progressStream{w: w, flusher: flusher, enc: json.NewEncoder(w), interval: streamKeepalive, done: make(chan struct{})}, true
}

// write emits one line, writing the response header first if this is the first. Until that happens
// the handler can still answer with an ordinary status-coded error.
func (s *progressStream) write(line streamLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		s.open = true
		s.w.Header().Set("Content-Type", ndjsonMediaType)
		// The stream is JSON objects, not a document; nosniff stops a proxy or a browser deciding
		// otherwise part-way through.
		s.w.Header().Set("X-Content-Type-Options", "nosniff")
		s.w.WriteHeader(http.StatusOK)
		// The response is committed, so from here the connection has to be kept alive until the
		// operation ends.
		go s.keepalive()
	}
	_ = s.enc.Encode(line)
	s.flusher.Flush()
}

// keepalive writes a bare newline whenever the stream has gone quiet, so a stage with nothing to
// report cannot outlive the connection carrying its result.
//
// IT IS WHITESPACE, DELIBERATELY, and that is what makes it safe to add to a wire format clients are
// already reading: ndjson is a sequence of JSON values, a decoder skips the whitespace between them,
// and a client that predates this cannot tell a keepalive from the line ending it already ignores.
// An empty object would have been a fourth line shape every reader had to learn.
//
// It exists because REPORTING CANNOT COVER EVERY WAIT. A build's stages come from a loop that polls
// and can therefore repeat itself; the rollout settle a deploy waits on is a single call into the
// cluster seam, bounded by `deploy.settle_timeout` and capable of running for minutes with nothing to
// say — and after an in-cluster build it is precisely where the node pulls the large image just
// built. A transport-level keepalive covers that, and every future stage, without every wait in the
// engine having to learn about proxies.
func (s *progressStream) keepalive() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.closed {
				_, _ = io.WriteString(s.w, "\n")
				s.flusher.Flush()
			}
			s.mu.Unlock()
		}
	}
}

// close stops the keepalive and forbids any further write. It must be called before the handler
// returns, and returning under the lock is what guarantees no keepalive is mid-write when it does.
func (s *progressStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
}

// progress is the reporter handed to the engine.
func (s *progressStream) progress(ev controlplane.DeployEvent) { s.write(streamLine{Event: &ev}) }

// finish closes the stream on an operation that produced a result.
func (s *progressStream) finish(res any) { s.write(streamLine{Result: res}) }

// fail closes the stream on an operation that errored, and reports whether it did. It returns false
// when nothing has been written yet: the refusal is then an ordinary status-coded one and the caller
// writes it as such — byte for byte what a caller that never asked for progress receives.
func (s *progressStream) fail(err error) bool {
	if !s.open {
		return false
	}
	status, body := engineError(err)
	s.write(streamLine{Error: &streamError{Status: status, errorResponse: body}})
	return true
}

// deployStream runs a deploy, reporting its stages as they happen and ending with one terminal line.
func (s *server) deployStream(w http.ResponseWriter, r *http.Request, req controlplane.DeployRequest) {
	stream, ok := newProgressStream(w)
	if !ok {
		s.deployPlain(w, r, req)
		return
	}
	defer stream.close()
	req.Progress = stream.progress
	res, err := s.engine.Deploy(r.Context(), req)
	if err != nil {
		if !stream.fail(err) {
			writeEngineError(w, err)
		}
		return
	}
	stream.finish(&res)
}

// deployPlain is the deploy the API has always served: one JSON object, status-coded.
func (s *server) deployPlain(w http.ResponseWriter, r *http.Request, req controlplane.DeployRequest) {
	res, err := s.engine.Deploy(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
