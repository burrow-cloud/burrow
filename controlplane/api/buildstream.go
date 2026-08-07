// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api

import (
	"net/http"

	"github.com/burrow-cloud/burrow/controlplane"
)

// A build is the operation this matters most for (issue #503). It clones, resolves dependencies, and
// runs every Dockerfile step — minutes, routinely — and a single JSON response written at the end of
// all that is a response nobody receives: every reverse proxy in front of a control plane has a read
// timeout, 60 seconds is ingress-nginx's default, and it is measured between two successive reads. A
// build that says nothing while it works is therefore unobservable from outside the cluster HOWEVER
// WELL IT GOES — the Job runs on happily to completion and the caller is handed a 504.
//
// It is the SAME machinery the deploy stream uses, from the negotiation (an `Accept:
// application/x-ndjson` a control plane that predates it ignores) down to the line framing and the
// lazily-written header. The only thing that differs is what the terminal line carries.
//
// ONE BUILD IS ONE SEQUENCE. A build ends where deploy begins (ADR-0053 §4), and the engine hands the
// caller's reporter straight on into that deploy, so the stages arrive as one run — clone, build,
// then the deploy's own — rather than as two disjoint streams with a silence in between.
//
// WHAT A HOLD LOOKS LIKE HERE differs from the deploy stream, and it has to. A deploy's guardrail
// decision happens before any stage runs, so a held deploy is refused status-coded with nothing
// written. A build's guardrail is the deploy's, and the deploy comes AFTER the build: by the time
// app.deploy is consulted the build has been running for minutes and its stages are already on the
// wire. A held build therefore arrives as the stream's error line — carrying the same 422, the same
// code, and the same needs_confirmation the status-coded refusal would have carried, which the client
// rebuilds into the identical *APIError. Nothing downstream can tell the two apart.

// build clones a git source reference and builds the app's image inside the cluster, then hands the
// resulting digest-pinned reference into the guarded deploy path (ADR-0053). A caller that sends
// `Accept: application/x-ndjson` gets the build's stages reported as they happen; everyone else gets
// the single JSON object this endpoint has always returned.
func (s *server) build(w http.ResponseWriter, r *http.Request) {
	var req controlplane.BuildRequest
	if !decode(w, r, &req) {
		return
	}
	req.App = r.PathValue("app") // the path is authoritative for the app name
	if wantsProgressStream(r) {
		s.buildStream(w, r, req)
		return
	}
	s.buildPlain(w, r, req)
}

// buildStream runs a build, reporting its stages as they happen and ending with one terminal line.
func (s *server) buildStream(w http.ResponseWriter, r *http.Request, req controlplane.BuildRequest) {
	stream, ok := newProgressStream(w)
	if !ok {
		s.buildPlain(w, r, req)
		return
	}
	defer stream.close()
	req.Progress = stream.progress
	res, err := s.engine.Build(r.Context(), req)
	if err != nil {
		if !stream.fail(err) {
			writeEngineError(w, err)
		}
		return
	}
	stream.finish(&res)
}

// buildPlain is the build the API has always served: one JSON object, status-coded.
func (s *server) buildPlain(w http.ResponseWriter, r *http.Request, req controlplane.BuildRequest) {
	res, err := s.engine.Build(r.Context(), req)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
