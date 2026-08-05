// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// These tests cover the app dimension of the agent's read-only `guard`
// ([ADR-0085](../../docs/adr/0085-a-guardrail-can-name-the-app-it-guards.md)).
//
// The property they protect is that the agent can see the rule that will ACTUALLY apply to the
// operation it is about to attempt. A guardrail resolves in three tiers — the named app or add-on
// instance, then the environment, then the global policy or the built-in default — so a read that
// could only reach the widest tier would report an operation as allowed while the narrowest tier
// denies it. ADR-0065 §7 requires the agent be able to see what it cannot do, and a disposition
// reported for the wrong tier is worse than no answer: it is a confident wrong one.

// guardRecorder is a control plane that records what was asked of it, so a test can assert not only
// the answer but the ROUTE it came from — which is the whole of the version-skew property below.
type guardRecorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *guardRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.Method+" "+req.URL.RequestURI())
}

func (r *guardRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func (r *guardRecorder) sawPrefix(prefix string) bool {
	for _, got := range r.seen() {
		if strings.HasPrefix(got, prefix) {
			return true
		}
	}
	return false
}

// guardControlPlane answers the name-scoped guard read with fixed guardrails, and every other route
// with a 404 — so a call that lands anywhere else is visible as a failure rather than absorbed.
func guardControlPlane(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *guardRecorder) {
	t.Helper()
	rec := &guardRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// runAgentErr drives run against a control plane and returns stdout and the error, for the cases
// where the error IS the behaviour under test.
func runAgentErr(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	full := append(args, "--control-plane", srv.URL, "--token", "t")
	err := run(context.Background(), full, &out, &errb)
	return out.String(), err
}

// TestGuardReportsTheTierThatAnswered asserts the agent sees WHICH TIER supplied each disposition,
// not merely what it is. The distinction is what an agent relays to a person: "denied for this app"
// points at `guard set --env prod --name website ... allow`, "denied by the built-in default" points
// somewhere else entirely, and an answer that cannot tell them apart leaves the human guessing at
// which of three rules to move.
func TestGuardReportsTheTierThatAnswered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{"the app tier", "name"},
		{"the environment tier", "env"},
		{"the built-in default", "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := guardControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/guard/name/website" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
					{"code": "app.run", "disposition": "deny",
						"description": "run a one-off command", "source": tc.source},
				}})
			})

			out, err := runAgentErr(t, srv, "guard", "--env", "prod", "--name", "website")
			if err != nil {
				t.Fatalf("guard --name: %v", err)
			}
			var got agentsurface.GuardReport
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("guard output is not a guard report: %v\n%s", err, out)
			}
			if len(got.Guardrails) != 1 || got.Guardrails[0].Source != tc.source {
				t.Fatalf("guard did not report the tier that answered: %+v", got.Guardrails)
			}
			if got.Guardrails[0].Disposition != "deny" {
				t.Errorf("disposition = %q, want deny", got.Guardrails[0].Disposition)
			}
			// The scope is echoed, so "source: name" is readable without knowing the arguments the
			// call was made with — an agent relaying the answer generally cannot supply them.
			if got.Scope == nil || got.Scope.Env != "prod" || got.Scope.Name != "website" {
				t.Errorf("guard report does not say what it is about: %+v", got.Scope)
			}
			// The name rode the ROUTE, not a query parameter. See the version-skew test below.
			if !rec.sawPrefix("GET /v1/guard/name/website?env=prod") {
				t.Errorf("requests = %v, want GET /v1/guard/name/website?env=prod", rec.seen())
			}
		})
	}
}

// TestUnscopedGuardStillReadsTheGlobalPolicy pins that adding the flag did not move the default. An
// agent that names nothing asks about the cluster, gets the global listing from the unscoped route,
// and gets no scope and no per-entry source, because there is no tier to distinguish.
func TestUnscopedGuardStillReadsTheGlobalPolicy(t *testing.T) {
	srv, rec := guardControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/guard" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.deploy", "disposition": "allow", "description": "deploy an app"},
		}})
	})

	out, err := runAgentErr(t, srv, "guard")
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	var got agentsurface.GuardReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("guard output is not a guard report: %v\n%s", err, out)
	}
	if got.Scope != nil {
		t.Errorf("unscoped guard reports a scope: %+v", got.Scope)
	}
	if !rec.sawPrefix("GET /v1/guard") || rec.sawPrefix("GET /v1/guard/name") {
		t.Errorf("requests = %v, want the unscoped GET /v1/guard", rec.seen())
	}
}

// TestGuardNameWithoutAnEnvironmentIsRefused asserts ADR-0085 §1's rule holds on the READ path, and
// that it is the CONTROL PLANE's refusal rather than a second copy of the rule in this binary.
//
// The rule is not a write-path formality. The policy key is env.name.code; without an environment
// segment the only key left to try is name.code, which is byte-identical to the key an ENVIRONMENT
// of that name produces — app and environment names are both DNS labels, so nothing tells them
// apart. An answer for "everything in the `website` environment" returned for "the `website` app"
// is exactly the mis-report this flag exists to prevent, so it is refused rather than guessed at.
//
// It is enforced in one place because the environment reaching the control plane is not simply the
// --env flag: a pinned environment handle supplies it with no flag given, so a check on the flag
// here would refuse calls the control plane accepts.
func TestGuardNameWithoutAnEnvironmentIsRefused(t *testing.T) {
	srv, rec := guardControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/guard/name/website" && r.URL.Query().Get("env") == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": `guardrails: "website" names something without saying which environment it is in; add --env`,
				"code":  "invalid",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	out, err := runAgentErr(t, srv, "guard", "--name", "website")
	if err == nil {
		t.Fatalf("guard --name with no environment succeeded: %s", out)
	}
	if !strings.Contains(err.Error(), "--env") {
		t.Errorf("refusal does not say what is missing: %v", err)
	}
	if out != "" {
		t.Errorf("a refused read printed to stdout: %q", out)
	}
	// It must not retry one tier wider. A silent fall back to the environment's policy would report
	// every app's disposition as though it were this one app's.
	if rec.sawPrefix("GET /v1/guard?") || rec.sawPrefix("GET /v1/guard ") {
		t.Errorf("requests = %v; the refused read fell back to the unscoped policy", rec.seen())
	}
}

// TestOlderControlPlaneRefusesRatherThanWidening is the version-skew property, and it is the reason
// the name rides the ROUTE rather than a query parameter.
//
// A control plane that predates the name tier IGNORES an unknown query parameter. It would answer
// 200 with the policy for every app in the environment, and the agent would report it as the policy
// for the one app it named — the wider answer wearing the narrower one's label, which is the failure
// this whole surface exists to prevent (issue #472, PR #483 for the write path). As a route it is a
// route the server does not have, so the ADR-0039 handshake refuses it with nothing shown.
func TestOlderControlPlaneRefusesRatherThanWidening(t *testing.T) {
	srv, rec := guardControlPlane(t, func(w http.ResponseWriter, r *http.Request) {
		// The shape of a control plane from before ADR-0085: the unscoped route only.
		if r.URL.Path != "/v1/guard" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.run", "disposition": "allow", "description": "run a one-off command"},
		}})
	})

	out, err := runAgentErr(t, srv, "guard", "--env", "prod", "--name", "website")
	if err == nil {
		t.Fatalf("an older control plane answered a name-scoped read: %s", out)
	}
	if out != "" {
		t.Errorf("a refused read printed to stdout: %q", out)
	}
	// The refusal names what would have been answered instead, so the agent can relay a remedy
	// rather than a bare 404.
	for _, want := range []string{"one app or add-on instance", "every app"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not say what was not answered (missing %q): %v", want, err)
		}
	}
	if rec.sawPrefix("GET /v1/guard?") || rec.sawPrefix("GET /v1/guard ") {
		t.Errorf("requests = %v; the name-scoped read fell back to the cluster-wide policy, which is "+
			"the wider answer wearing the narrower one's label", rec.seen())
	}
}

// TestAgentStillCannotSetAGuardrail is the invariant the whole surface rests on. `guard` gaining a
// scope must not gain a verb: the middle tier of ADR-0065 is trustworthy only because the agent
// cannot move its own guardrails, so `guard set` is absent from this binary and no amount of
// scoping brings it back (ADR-0021, ADR-0065 §2).
//
// TestGuardStaysReadOnly asserts the shape of the command; this asserts the behaviour, including
// that nothing reaches the control plane on the way to the refusal.
func TestAgentStillCannotSetAGuardrail(t *testing.T) {
	srv, rec := guardControlPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	for _, args := range [][]string{
		{"guard", "set", "app.deploy", "allow"},
		{"guard", "set", "--env", "prod", "--name", "website", "app.deploy", "allow"},
	} {
		out, err := runAgentErr(t, srv, args...)
		if err == nil {
			t.Fatalf("burrow-agent %v succeeded; the agent must not be able to set its own guardrails: %s", args, out)
		}
		if out != "" {
			t.Errorf("burrow-agent %v printed to stdout: %q", args, out)
		}
	}
	if len(rec.seen()) != 0 {
		t.Errorf("an attempted `guard set` reached the control plane: %v", rec.seen())
	}
}
