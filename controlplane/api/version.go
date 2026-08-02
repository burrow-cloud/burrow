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
// client sends its release version in X-Burrow-Client-Version and its binary name in
// X-Burrow-Client (its transport sets both headers); the checks here turn genuine skew into an
// actionable error instead of an opaque failure:
//
//   - a client more than one minor behind burrowd is refused with a "too old" error that names the
//     remedy for THAT binary — see tooOldMessage. A client that predates the handshake sends no
//     header and is served, and a locally built (untagged) client is exempt — there is nothing for it
//     to upgrade to.
//   - a request for a route this burrowd does not have — a newer client calling a feature the server
//     lacks — becomes a structured "unknown operation, upgrade the control plane" error rather than a
//     bare 404.
//
// burrowd's own version comes from Config.Version; empty or a non-release build makes the handshake
// permissive, since there is no meaningful window to enforce.

// clientVersionHeader carries the calling client's release version (ADR-0039). It rides alongside
// X-Burrow-Token and, like it, survives the Kubernetes API-server proxy untouched.
const clientVersionHeader = "X-Burrow-Client-Version"

// clientNameHeader carries the calling binary's name — "burrow" (the human CLI) or "burrow-agent"
// (the agent's scoped control channel, ADR-0049 §1) — alongside the version header. Burrow ships two
// clients that install together but drift apart on disk, and the remedy for a stale one is NOT the
// remedy for the other: telling someone whose burrow-agent is stale to upgrade the burrow CLI sends
// them to a command they have very likely already run. A client that predates this header sends
// nothing and gets the both-binaries wording below.
const clientNameHeader = "X-Burrow-Client"

// burrowAgentBinary and burrowCLIBinary are the two client names the messages below recognize; they
// are the executable names, so what a refusal prints is what the user types.
const (
	burrowCLIBinary   = "burrow"
	burrowAgentBinary = "burrow-agent"
)

// tooOldMessage renders the refusal for a client outside the compatibility window. Its whole job is
// to name a remedy that fixes THE BINARY THAT WAS REFUSED, so it branches on the client name:
//
//   - burrow-agent: say so explicitly, and say "not just the CLI" — the failure this replaces is a
//     user who had already run the Homebrew upgrade, was told to run it again, and concluded Burrow
//     was broken. It also names the session restart, because a running agent keeps executing the
//     binary it launched with.
//   - burrow: the CLI remedy on its own.
//   - unknown (a client older than the name header, which is exactly the stranded case being
//     reported): name both binaries rather than guess at one.
//
// Both Homebrew and source remedies are given, because the control plane cannot know how the caller
// was installed. A client that carries its own too-old handling narrows this further on the client
// side, where the installed path is knowable.
//
// Every Homebrew remedy names the formula TAP-QUALIFIED — `burrow-cloud/tap/burrow`. Burrow ships
// from its own tap, and homebrew-core already has an unrelated formula called `burrow` (LinkedIn's
// Kafka consumer-lag checker), so a bare `brew upgrade burrow` never updates Burrow: it either fails
// as not-installed or upgrades a different project. This text is read at the exact moment a user is
// already blocked, so the one command it gives has to be the one that works.
func tooOldMessage(clientName, clientVersion, serverVersion string) string {
	switch clientName {
	case burrowAgentBinary:
		return fmt.Sprintf("your burrow-agent (%s) is too old for this control plane (%s); update the burrow-agent binary, not just the burrow CLI: they are separate binaries. `brew upgrade burrow-cloud/tap/burrow` updates both, or from source `go install github.com/burrow-cloud/burrow/cmd/burrow-agent@%s`. Then restart your agent session so it runs the new binary.", clientVersion, serverVersion, serverVersion)
	case burrowCLIBinary:
		return fmt.Sprintf("your burrow CLI (%s) is too old for this control plane (%s); run `brew upgrade burrow-cloud/tap/burrow` (or reinstall the CLI from the release archive) to update it.", clientVersion, serverVersion)
	default:
		return fmt.Sprintf("your burrow client (%s) is too old for this control plane (%s); burrow and burrow-agent are separate binaries and both must be current, so update the one that made this call. `brew upgrade burrow-cloud/tap/burrow` updates both, or from source `go install github.com/burrow-cloud/burrow/cmd/burrow-agent@%s`. Then restart your agent session so it runs the new burrow-agent.", clientVersion, serverVersion, serverVersion)
	}
}

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
				Error:         tooOldMessage(r.Header.Get(clientNameHeader), cv, serverVersion),
				Code:          "client_too_old",
				ServerVersion: serverVersion,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// The install check (ADR-0084 §5). A kube context name says HOW TO GET somewhere; it does not say
// WHETHER YOU ARRIVED. It is user-controlled, it is reusable, and cloud providers generate it
// deterministically — `doctl kubernetes cluster kubeconfig save` writes names like `do-nyc3-burrow`,
// so destroying a cluster and standing another one up produces a byte-identical context name for an
// entirely different cluster. Every visible signal reads correct and the command lands somewhere
// nobody meant.
//
// So each install gets an id, and the caller says which install it means. The id is not a secret and
// authorises nothing: it identifies an install so that arriving at the wrong one is a refusal that
// names the cause, rather than a surprise or a bare 401 from a cluster the caller did not know they
// had reached.
//
// It rides a header for the same reason the ADR-0039 handshake does, and it is checked here, beside
// versionGate, for the same reason: both are facts about the CALL rather than about the caller, both
// are one choke point on each end, and both serve a request that says nothing.

// installHeader carries the install id the CALLER EXPECTS this control plane to be (ADR-0084 §5). It
// is X-Burrow-Install rather than part of Authorization because it is not a credential — the id
// grants nothing, and a second person joining an install has to be able to read it — and because
// Authorization is not available on the API-server proxy path at all, where client-go owns that
// header to authenticate to the API server itself (ADR-0015). Like X-Burrow-Token and the two
// handshake headers, the proxy forwards it untouched.
const installHeader = "X-Burrow-Install"

// installMismatchCode is the machine-readable tag on the refusal, alongside "client_too_old" and
// "unknown_operation". A caller branches on it to offer re-pointing the target rather than treating
// the failure as the cluster being down.
const installMismatchCode = "install_mismatch"

// installMismatchMessage renders the refusal for a caller that reached an install it did not mean
// to. It names BOTH ids, because the useful fact is not "something is wrong" but "the Burrow you
// recorded and the Burrow that answered are different things" — which is what tells a reader that
// the cluster behind a context name they still recognise has been rebuilt, rather than sending them
// to look for a network or credential problem they do not have.
//
// It says "the cluster at this context" rather than naming the context, because the control plane
// does not know what the caller's kubeconfig calls it: the name is local to the machine that made
// the call, and inventing one here would print something the reader cannot find.
func installMismatchMessage(want, have string) string {
	return fmt.Sprintf("this is not the Burrow install you are pointed at: your target expects the Burrow installed as %s, "+
		"and the cluster at this context is running install %s. A cluster rebuilt under the same kube context name is the "+
		"usual cause — the name still resolves, the Burrow behind it is a different one. Point at it again with "+
		"`burrow auth login`, or select another target with `burrow auth switch <name>`.", want, have)
}

// installGate refuses a request whose X-Burrow-Install names an install this control plane is not,
// with a structured error carrying this install's own id (ADR-0084 §5), and otherwise serves it.
//
// Two absences are served, and both are load-bearing rather than lenient:
//
//   - No header. The caller's target predates install ids, or is a Burrow Cloud target, or is a
//     command reaching a control plane directly. It has made no claim about which install this is,
//     so there is nothing to contradict — the same tolerance versionGate gives a client that
//     predates the handshake.
//   - No installID configured on this server. burrowd was installed before ids existed and does not
//     know its own. It cannot establish that the caller is wrong, and refusing on an unknown would
//     break every caller of an older install the moment their CLI learned to send the header.
//
// It runs after authentication, so an anonymous request never learns this install's id, and before
// versionGate, so a caller who has reached the wrong Burrow is told that rather than being handed a
// version remedy for a control plane they did not mean to talk to.
func installGate(installID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := r.Header.Get(installHeader); installID != "" && want != "" && want != installID {
			writeJSON(w, http.StatusConflict, errorResponse{
				Error:           installMismatchMessage(want, installID),
				Code:            installMismatchCode,
				ServerInstallID: installID,
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
			name := r.Header.Get(clientNameHeader)
			if name != burrowCLIBinary && name != burrowAgentBinary {
				name = "burrow client"
			}
			msg += fmt.Sprintf("; if your %s (%s) is newer, ask an operator to run `burrow upgrade` to update the control plane", name, cv)
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
