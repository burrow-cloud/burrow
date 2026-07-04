// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The client-version handshake (ADR-0039). burrowd is the compatibility anchor: it serves any
// client within one minor of its own version and never hard-blocks on version difference alone. The
// client sends its release version in X-Burrow-Client-Version (its transport sets the header); the
// checks here turn genuine skew into an actionable error instead of an opaque failure:
//
//   - a client more than one minor behind burrowd is refused with a "too old, upgrade the CLI"
//     error. A client that predates the handshake sends no header and is served, and a locally built
//     (untagged) client is exempt — there is nothing for it to upgrade to.
//   - a request for a route this burrowd does not have — a newer client calling a feature the server
//     lacks — becomes a structured "unknown operation, upgrade the control plane" error rather than a
//     bare 404.
//
// burrowd's own version comes from Config.Version; empty or a non-release build makes the handshake
// permissive, since there is no meaningful window to enforce.

// clientVersionHeader carries the calling client's release version (ADR-0039). It rides alongside
// X-Burrow-Token and, like it, survives the Kubernetes API-server proxy untouched.
const clientVersionHeader = "X-Burrow-Client-Version"

// clientSupported reports whether a client of clientVersion is within the compatibility window of a
// burrowd of serverVersion: the same minor or exactly one minor back. A newer client is also
// "supported" here — its only possible gap is a route this server lacks, which v1NotFound handles.
// A non-release version on either side (empty, "dev", anything semver cannot compare) or a locally
// built client (a Go pseudo-version) is treated as supported: there is no window to reason about or
// nothing to upgrade to, so burrowd serves rather than guesses — matching the passive `burrow
// version` nudge, which exempts the same builds.
func clientSupported(serverVersion, clientVersion string) bool {
	if !semver.IsValid(serverVersion) || !semver.IsValid(clientVersion) {
		return true
	}
	if module.IsPseudoVersion(clientVersion) {
		return true
	}
	floor := oneMinorBack(semver.MajorMinor(serverVersion))
	return semver.Compare(semver.MajorMinor(clientVersion), floor) >= 0
}

// oneMinorBack returns the major.minor one minor below majorMinor (e.g. "v0.9" -> "v0.8"), the
// oldest minor burrowd still serves. At a major's ".0" there is no older minor within the major, so
// it returns the input unchanged; a cross-major window is deliberately out of scope (ADR-0039 bounds
// the window to one minor, and Burrow is pre-1.0). It assumes a valid "vMAJOR.MINOR" input and stays
// defensive on anything else.
func oneMinorBack(majorMinor string) string {
	parts := strings.SplitN(strings.TrimPrefix(majorMinor, "v"), ".", 2)
	if len(parts) != 2 {
		return majorMinor
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || minor == 0 {
		return majorMinor
	}
	return fmt.Sprintf("v%d.%d", major, minor-1)
}

// versionGate refuses a request from a client more than one minor behind serverVersion with a
// structured, actionable error (ADR-0039) and otherwise serves it — the anchor never hard-blocks on
// version difference alone. A request with no client-version header comes from a pre-handshake client
// and is served. It runs after authentication so only authenticated callers learn the server version.
func versionGate(serverVersion string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cv := r.Header.Get(clientVersionHeader); cv != "" && !clientSupported(serverVersion, cv) {
			writeJSON(w, http.StatusUpgradeRequired, errorResponse{
				Error: fmt.Sprintf("your burrow client (%s) is too old for this control plane (%s); run `brew upgrade burrow` (or reinstall the CLI) to update it", cv, serverVersion),
				Code:  "client_too_old",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientVersionContext puts the request's X-Burrow-Client-Version on the context so the engine can
// record which client drove a guarded operation in the audit log, next to the principal (ADR-0039).
// It runs inside the version gate — on an authenticated, in-window request — so an audited operation
// carries the acting client's version whenever the client sent one; a pre-handshake request (no
// header) leaves the context untouched and records no version.
func clientVersionContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cv := r.Header.Get(clientVersionHeader); cv != "" {
			r = r.WithContext(controlplane.ContextWithClientVersion(r.Context(), cv))
		}
		next.ServeHTTP(w, r)
	})
}

// v1NotFound wraps the /v1 mux so a request for a route this burrowd does not have becomes a
// structured "unknown operation" error naming the fix (ADR-0039) — a newer client calling a feature
// the server lacks gets an actionable message, not a bare 404. A matched route (including one whose
// handler reports its own not-found) and a method mismatch on an existing path (405) are left
// untouched.
func v1NotFound(serverVersion string, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r) // a real route — serve it (it may return its own not-found)
			return
		}
		// No route matched the path and method: the mux's default handler yields 404 (no such path)
		// or 405 (path exists, wrong method). Capture which, so only a true 404 becomes the structured
		// unknown-operation error and a 405 keeps its standard meaning.
		rec := &statusCapture{header: http.Header{}, status: http.StatusOK}
		mux.ServeHTTP(rec, r)
		if rec.status != http.StatusNotFound {
			rec.replay(w)
			return
		}
		msg := "this control plane does not recognize this operation"
		if serverVersion != "" {
			msg = fmt.Sprintf("this control plane (%s) does not recognize %s %s", serverVersion, r.Method, r.URL.Path)
		}
		if cv := r.Header.Get(clientVersionHeader); cv != "" {
			msg += fmt.Sprintf("; if your burrow client (%s) is newer, ask an operator to run `burrow upgrade` to update the control plane", cv)
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: msg, Code: "unknown_operation"})
	})
}

// statusCapture is a minimal http.ResponseWriter that buffers a response so v1NotFound can inspect
// the status the mux's default handler chose (404 vs 405) and either replace it with a structured
// error or replay it unchanged. It only ever wraps the mux's built-in not-found/method-not-allowed
// handlers, which write a short body, so buffering is cheap.
type statusCapture struct {
	header http.Header
	status int
	body   []byte
}

func (c *statusCapture) Header() http.Header    { return c.header }
func (c *statusCapture) WriteHeader(status int) { c.status = status }
func (c *statusCapture) Write(b []byte) (int, error) {
	c.body = append(c.body, b...)
	return len(b), nil
}

// replay writes the buffered response through to the real ResponseWriter unchanged.
func (c *statusCapture) replay(w http.ResponseWriter) {
	for k, vs := range c.header {
		w.Header()[k] = vs
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body)
}
