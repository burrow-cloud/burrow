// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"

	"github.com/burrow-cloud/burrow/controlplane"
)

// A deploy takes ten to twenty seconds and every stage of it happens server-side, so the client
// cannot narrate it (issue #480). This is how the control plane says what it is doing over the call
// the client is already blocked on.
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

// deployStreamLine is one line of the stream. Exactly one field is ever set: an event while the
// deploy runs, then a single terminal line that is either the result or the error. The wrapper keys
// are what let a reader tell the three apart without guessing at a shape.
type deployStreamLine struct {
	Event  *controlplane.DeployEvent  `json:"event,omitempty"`
	Result *controlplane.DeployResult `json:"result,omitempty"`
	Error  *streamError               `json:"error,omitempty"`
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

// deployStream runs a deploy, reporting its stages as they happen and ending with one terminal line.
//
// A ResponseWriter that cannot flush falls back to the ordinary buffered response: the caller then
// gets its answer at the end instead of the stages as they happen, which is today's behaviour rather
// than a failure. Every writer on the real serving path flushes — the API-server service proxy this
// travels through streams fine, and no middleware on a matched /v1 route wraps the writer.
func (s *server) deployStream(w http.ResponseWriter, r *http.Request, req controlplane.DeployRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.deployPlain(w, r, req)
		return
	}
	enc := json.NewEncoder(w)
	open := false
	// begin writes the response header, once, at the moment the first line is produced. Until it is
	// called the handler can still answer with an ordinary status-coded error.
	begin := func() {
		if open {
			return
		}
		open = true
		w.Header().Set("Content-Type", ndjsonMediaType)
		// The stream is JSON objects, not a document; nosniff stops a proxy or a browser deciding
		// otherwise part-way through.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
	}
	write := func(line deployStreamLine) {
		begin()
		_ = enc.Encode(line)
		flusher.Flush()
	}
	// The engine calls this synchronously on this goroutine, in stage order, so there is no
	// concurrent writer to guard against.
	req.Progress = func(ev controlplane.DeployEvent) { write(deployStreamLine{Event: &ev}) }

	res, err := s.engine.Deploy(r.Context(), req)
	if err != nil {
		if !open {
			// Nothing has been written, so this is an ordinary refusal and stays one — byte for byte
			// what a caller that never asked for progress receives.
			writeEngineError(w, err)
			return
		}
		status, body := engineError(err)
		write(deployStreamLine{Error: &streamError{Status: status, errorResponse: body}})
		return
	}
	write(deployStreamLine{Result: &res})
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
