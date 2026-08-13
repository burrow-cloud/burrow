// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mutatingControlPlane stands up an httptest.Server whose deploy/rollback/scale/autoscale/run handlers
// answer with whatever the test wires, so the confirm flow can be exercised without a cluster. Each
// handler consults the fields set on the returned *fakeCP, so one server serves every case.
type fakeCP struct {
	srv     *httptest.Server
	handler func(w http.ResponseWriter, r *http.Request)
}

func newFakeCP(t *testing.T) *fakeCP {
	t.Helper()
	f := &fakeCP{}
	mux := http.NewServeMux()
	// One catch-all for the app verbs; the test's handler decides the response per path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if f.handler == nil {
			http.Error(w, "no handler", http.StatusInternalServerError)
			return
		}
		f.handler(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// held writes the control plane's held-for-confirmation response for code: a 422 with
// needs_confirmation set, exactly as writeEngineError does for a disposition-confirm hold (ADR-0020).
func held(w http.ResponseWriter, op, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": "guardrail holds " + op + " for confirmation: " + msg, "code": code, "needs_confirmation": true,
	})
}

// denied writes the control plane's outright-denial response for code: a 422 with the guardrail code
// and needs_confirmation unset (a plain deny).
func denied(w http.ResponseWriter, op, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": "guardrail refused " + op + ": " + msg, "code": code,
	})
}

// runMutate drives run against the fake control plane and returns stdout, the returned error, and the
// resolved exit code (0 when no error). It never t.Fatals on a non-nil error, because held and denied
// outcomes deliberately return an *exitError.
func runMutate(t *testing.T, f *fakeCP, args ...string) (string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	// The connection flags must land BEFORE any `--` separator, or cobra treats them as the command's
	// own arguments (as `run <app> -- cmd...` would). Insert them at the separator when present.
	conn := []string{"--control-plane", f.srv.URL, "--token", "t"}
	full := make([]string, 0, len(args)+len(conn))
	dash := -1
	for i, a := range args {
		if a == "--" {
			dash = i
			break
		}
	}
	if dash >= 0 {
		full = append(full, args[:dash]...)
		full = append(full, conn...)
		full = append(full, args[dash:]...)
	} else {
		full = append(full, args...)
		full = append(full, conn...)
	}
	err := run(context.Background(), full, &out, &errb)
	code := 0
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			code = ee.code
		} else {
			t.Fatalf("run(%v): unexpected non-exit error: %v (stderr %s)", args, err, errb.String())
		}
	}
	return out.String(), code
}

func decodeOutcome(t *testing.T, s string) outcome {
	t.Helper()
	var oc outcome
	if err := json.Unmarshal([]byte(s), &oc); err != nil {
		t.Fatalf("outcome is not valid JSON: %v (%q)", err, s)
	}
	return oc
}

// TestExecutedOutcome: a deploy the control plane accepts prints outcome "executed" with the result
// and exits 0.
func TestExecutedOutcome(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"release": map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed"}})
	}
	out, code := runMutate(t, f, "deploy", "web", "--image", "img:1")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeExecuted {
		t.Errorf("outcome = %q, want executed", oc.Outcome)
	}
	if oc.Operation != "deploy" {
		t.Errorf("operation = %q, want deploy", oc.Operation)
	}
	if code != exitCodeExecuted {
		t.Errorf("exit code = %d, want %d", code, exitCodeExecuted)
	}
	if oc.Result == nil {
		t.Error("executed outcome carries no result")
	}
}

// TestHeldThenConfirm is the crux of the confirm flow: a held deploy prints outcome
// "held_for_confirmation" with the guardrail code and confirm_required, and exits 2 — and the binary
// does NOT self-confirm (the first request carries confirm=false). Re-running with --confirm reaches
// the control plane with confirm=true and executes.
func TestHeldThenConfirm(t *testing.T) {
	f := newFakeCP(t)
	var sawConfirm []bool
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Confirm bool `json:"confirm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sawConfirm = append(sawConfirm, body.Confirm)
		if !body.Confirm {
			held(w, "deploy", "app.deploy", "deploying a new release to prod requires confirmation to proceed")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"release": map[string]any{"id": "r2", "app": "web", "status": "deployed"}})
	}

	// First invocation: no --confirm. Held.
	out, code := runMutate(t, f, "deploy", "web", "--image", "img:1")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeHeld {
		t.Fatalf("outcome = %q, want held_for_confirmation", oc.Outcome)
	}
	if oc.Code != "app.deploy" {
		t.Errorf("code = %q, want app.deploy", oc.Code)
	}
	if !oc.ConfirmRequired {
		t.Error("held outcome must set confirm_required")
	}
	if oc.Message == "" {
		t.Error("held outcome must carry a human-readable message")
	}
	if code != exitCodeHeld {
		t.Errorf("exit code = %d, want %d", code, exitCodeHeld)
	}

	// Second invocation: the human approved, so a human re-runs with --confirm. Executes.
	out, code = runMutate(t, f, "deploy", "web", "--image", "img:1", "--confirm")
	oc = decodeOutcome(t, out)
	if oc.Outcome != outcomeExecuted {
		t.Fatalf("after --confirm, outcome = %q, want executed", oc.Outcome)
	}
	if code != exitCodeExecuted {
		t.Errorf("after --confirm, exit code = %d, want 0", code)
	}

	// The binary never self-confirmed: the first request carried confirm=false, the second true.
	if len(sawConfirm) != 2 || sawConfirm[0] != false || sawConfirm[1] != true {
		t.Errorf("confirm flags the control plane saw = %v, want [false true]", sawConfirm)
	}
}

// TestDeniedOutcome: a guardrail deny prints outcome "denied" with the code and exits 3, distinct from
// held so the agent knows no --confirm will help.
func TestDeniedOutcome(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		denied(w, "delete", "app.delete", "deleting the app is denied by the current guardrail policy")
	}
	out, code := runMutate(t, f, "delete", "web", "--confirm")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeDenied {
		t.Fatalf("outcome = %q, want denied", oc.Outcome)
	}
	if oc.Code != "app.delete" {
		t.Errorf("code = %q, want app.delete", oc.Code)
	}
	if oc.ConfirmRequired {
		t.Error("denied outcome must not set confirm_required")
	}
	if code != exitCodeDenied {
		t.Errorf("exit code = %d, want %d", code, exitCodeDenied)
	}
}

// TestOperationalLimitOutcome: crossing an operational limit is a refusal no --confirm opens, so it
// classifies as "denied" and exits 3 rather than falling through to a plain "error"
// ([ADR-0068](../../docs/adr/0068-operational-limits-are-configuration.md) §2). The distinction the
// agent needs is between "retrying will not help" and "something went wrong"; a limit is the first,
// and the message the control plane wrote names the operator command that raises it.
func TestOperationalLimitOutcome(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "scale refused: 99 replicas exceeds the replica ceiling of 5, set for the cluster. " +
				"A limit is a bound a human sets, not a guardrail that can be dispositioned away: raise it " +
				"for one environment with `burrow cluster config set --env <env> app.replica_ceiling <value>`.",
			"code":      "app.replica_ceiling",
			"requested": 99,
			"limit":     5,
		})
	}
	out, code := runMutate(t, f, "scale", "web", "99")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeDenied {
		t.Fatalf("outcome = %q, want denied", oc.Outcome)
	}
	if oc.Code != "app.replica_ceiling" {
		t.Errorf("code = %q, want app.replica_ceiling", oc.Code)
	}
	if oc.ConfirmRequired {
		t.Error("a limit refusal must not offer a confirmation: nothing about it is confirmable")
	}
	if !strings.Contains(oc.Message, "burrow cluster config set") {
		t.Errorf("message = %q, want the operator command that raises the limit", oc.Message)
	}
	if code != exitCodeDenied {
		t.Errorf("exit code = %d, want %d", code, exitCodeDenied)
	}
}

// TestErrorOutcome: a plain failure (a not-found app, code "not_found") is classified as "error", not
// denied — its code is not a guardrail — and exits 1.
func TestErrorOutcome(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "app \"web\" not found", "code": "not_found"})
	}
	out, code := runMutate(t, f, "rollback", "web")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeError {
		t.Fatalf("outcome = %q, want error", oc.Outcome)
	}
	if oc.Message == "" {
		t.Error("error outcome must carry a message")
	}
	if code != exitCodeError {
		t.Errorf("exit code = %d, want %d", code, exitCodeError)
	}
}

// TestRunNonZeroExitIsExecuted: a one-off command that exits non-zero is a NORMAL executed outcome
// carrying the RunResult (ADR-0048), not an error — outcome "executed", exit 0, and the RunResult's
// own exit_code is the command's.
func TestRunNonZeroExitIsExecuted(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "exit_code": 3, "stdout": "migration failed"})
	}
	out, code := runMutate(t, f, "run", "web", "--", "npm", "run", "migrate")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeExecuted {
		t.Fatalf("outcome = %q, want executed (a non-zero exit is a normal result)", oc.Outcome)
	}
	if code != exitCodeExecuted {
		t.Errorf("burrow-agent exit code = %d, want 0 — the command's exit code rides in the result", code)
	}
	// The command's own non-zero exit code is preserved inside the result.
	result, _ := oc.Result.(map[string]any)
	if result == nil {
		t.Fatalf("run result missing: %q", out)
	}
	if ec, _ := result["exit_code"].(float64); ec != 3 {
		t.Errorf("result exit_code = %v, want 3", result["exit_code"])
	}
}

// TestMutatingVerbsPresent confirms the mutating verbs are compiled in — the Phase 2a compute verbs
// and the Phase 2b routing/add-on/config/delete verbs — each resolving to a valid outcome envelope
// rather than an unknown-command error. It is the positive counterpart to TestAdminVerbsAbsent.
func TestMutatingVerbsPresent(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		// A generic accepting response; every verb decodes something valid from it.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "exit_code": 0,
			"release": map[string]any{"id": "r1", "app": "web", "status": "deployed"},
		})
	}
	present := [][]string{
		// Phase 2a compute verbs, plus the ADR-0053 in-cluster build front-end.
		{"deploy", "web", "--image", "img:1"},
		{"build", "web", "--source", "https://github.com/user/app", "--ref", "v1.2.3", "--image", "img:1"},
		{"rollback", "web"},
		{"scale", "web", "3"},
		{"autoscale", "web"},
		{"run", "web", "--", "echo", "hi"},
		// Phase 2b routing verbs.
		{"publish", "web", "--host", "web.example.com", "--port", "8080"},
		{"unpublish", "web"},
		{"domain", "add", "web.example.com", "--address", "203.0.113.5"},
		{"domain", "remove", "web.example.com"},
		// Phase 2b add-on operations. `addon remove` is deliberately not here: it is ADR-0065 §2
		// tier 1, absent from this binary, and TestAddonRemoveStructurallyAbsent asserts that.
		{"addon", "install", "logs"},
		{"addon", "attach", "postgres", "web"},
		{"addon", "backup", "postgres", "web"},
		// Phase 2b config writes, secret-key removal, and the guarded delete.
		{"config", "set", "web", "K=V"},
		{"config", "unset", "web", "K"},
		{"secret", "unset", "web", "K"},
		{"delete", "web"},
	}
	for _, args := range present {
		out, _ := runMutate(t, f, args...)
		oc := decodeOutcome(t, out)
		if oc.Outcome == "" {
			t.Errorf("run(%v) produced no outcome envelope: %q", args, out)
		}
		if oc.Outcome == outcomeError {
			t.Errorf("run(%v) errored, want the verb present and executing: %q", args, out)
		}
	}
}

// TestDeleteHeldThenConfirm exercises the guarded destructive delete through the confirm flow: without
// --confirm the app.delete guardrail holds it (outcome held_for_confirmation, exit 2) and the binary
// does NOT self-confirm; after the human approves, re-running with --confirm reaches the control plane
// with confirm=true and executes. Delete carries confirm as a query parameter (?confirm=true), so the
// handler reads it there, not from the body.
func TestDeleteHeldThenConfirm(t *testing.T) {
	f := newFakeCP(t)
	var sawConfirm []bool
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		confirm := r.URL.Query().Get("confirm") == "true"
		sawConfirm = append(sawConfirm, confirm)
		if !confirm {
			held(w, "delete", "app.delete", "deleting the app \"web\" (its workload, routing, and release history) requires confirmation")
			return
		}
		w.WriteHeader(http.StatusOK)
	}

	// First invocation: no --confirm. Held for the human's approval.
	out, code := runMutate(t, f, "delete", "web")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeHeld {
		t.Fatalf("outcome = %q, want held_for_confirmation", oc.Outcome)
	}
	if oc.Code != "app.delete" {
		t.Errorf("code = %q, want app.delete", oc.Code)
	}
	if !oc.ConfirmRequired {
		t.Error("held delete must set confirm_required")
	}
	if code != exitCodeHeld {
		t.Errorf("exit code = %d, want %d", code, exitCodeHeld)
	}

	// Second invocation: the human approved, so a human re-runs with --confirm. Executes.
	out, code = runMutate(t, f, "delete", "web", "--confirm")
	oc = decodeOutcome(t, out)
	if oc.Outcome != outcomeExecuted {
		t.Fatalf("after --confirm, outcome = %q, want executed", oc.Outcome)
	}
	if code != exitCodeExecuted {
		t.Errorf("after --confirm, exit code = %d, want 0", code)
	}

	// The binary never self-confirmed: the first request carried confirm=false, the second true.
	if len(sawConfirm) != 2 || sawConfirm[0] != false || sawConfirm[1] != true {
		t.Errorf("confirm flags the control plane saw = %v, want [false true]", sawConfirm)
	}
}

// TestPublishHeldThenConfirm covers a guarded routing verb (app.expose_public) end to end: held
// without --confirm, executed with it. Publish carries confirm in the request body, like the compute
// verbs, and one confirmation covers every link it composes.
func TestPublishHeldThenConfirm(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Confirm bool `json:"confirm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Confirm {
			held(w, "publish", "app.expose_public", "exposing \"web\" to the public internet requires confirmation")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "host": "web.example.com", "reachable": true, "url": "https://web.example.com"})
	}

	out, code := runMutate(t, f, "publish", "web", "--host", "web.example.com", "--port", "8080")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeHeld || oc.Code != "app.expose_public" {
		t.Fatalf("outcome = %q code = %q, want held_for_confirmation app.expose_public", oc.Outcome, oc.Code)
	}
	if code != exitCodeHeld {
		t.Errorf("exit code = %d, want %d", code, exitCodeHeld)
	}

	out, code = runMutate(t, f, "publish", "web", "--host", "web.example.com", "--port", "8080", "--confirm")
	oc = decodeOutcome(t, out)
	if oc.Outcome != outcomeExecuted {
		t.Fatalf("after --confirm, outcome = %q, want executed", oc.Outcome)
	}
	if code != exitCodeExecuted {
		t.Errorf("after --confirm, exit code = %d, want 0", code)
	}
}

// TestDomainRemoveDenied covers a guarded verb resolving to denied (not held): a dns.delete guardrail
// set to deny prints outcome "denied", exit 3, with no confirm_required — no --confirm will help. It
// confirms the Phase 2b routing verbs reuse the exact classification the compute verbs do.
func TestDomainRemoveDenied(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		denied(w, "domain remove", "dns.delete", "deleting public DNS records is denied in this environment")
	}
	out, code := runMutate(t, f, "domain", "remove", "web.example.com")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeDenied || oc.Code != "dns.delete" {
		t.Fatalf("outcome = %q code = %q, want denied dns.delete", oc.Outcome, oc.Code)
	}
	if oc.ConfirmRequired {
		t.Error("denied outcome must not set confirm_required")
	}
	if code != exitCodeDenied {
		t.Errorf("exit code = %d, want %d", code, exitCodeDenied)
	}
}

// TestUnguardedVerbsExecute covers the Phase 2b verbs that are NOT guarded — unpublish, addon attach,
// addon backup, secret unset — each executing straight through the envelope with the result the
// control plane returns. `config set` and `config unset` left this list with ADR-0098 and are
// covered by TestConfigWriteHeldThenConfirm.
func TestUnguardedVerbsExecute(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "addon": "postgres", "secret_key": "DATABASE_URL",
			"backup": map[string]any{"id": "b1", "app": "web", "status": "completed"},
		})
	}
	cases := [][]string{
		{"unpublish", "web"},
		{"addon", "attach", "postgres", "web"},
		{"addon", "backup", "postgres", "web"},
		{"secret", "unset", "web", "OLD_KEY"},
	}
	for _, args := range cases {
		out, code := runMutate(t, f, args...)
		oc := decodeOutcome(t, out)
		if oc.Outcome != outcomeExecuted {
			t.Errorf("run(%v) outcome = %q, want executed: %q", args, oc.Outcome, out)
		}
		if code != exitCodeExecuted {
			t.Errorf("run(%v) exit code = %d, want 0", args, code)
		}
	}
}

// TestConfigWriteHeldThenConfirm covers a config write through the confirm flow (ADR-0098). Without
// --confirm the app.config guardrail holds it (held_for_confirmation, exit 2) and the binary does not
// self-confirm; a human re-runs it with --confirm and it executes. `config set` carries confirm in
// the body and `config unset` in the query, since a DELETE has none, so both shapes are exercised.
func TestConfigWriteHeldThenConfirm(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		confirm := r.URL.Query().Get("confirm") == "true"
		if r.Method == http.MethodPost {
			var body struct {
				Confirm bool `json:"confirm"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			confirm = body.Confirm
		}
		if !confirm {
			held(w, "set config", "app.config", "setting the config var LOG_LEVEL on \"web\" (which rolls the running app) requires confirmation")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "LOG_LEVEL"})
	}

	for _, args := range [][]string{
		{"config", "set", "web", "LOG_LEVEL=debug"},
		{"config", "unset", "web", "LOG_LEVEL"},
	} {
		out, code := runMutate(t, f, args...)
		oc := decodeOutcome(t, out)
		if oc.Outcome != outcomeHeld || oc.Code != "app.config" {
			t.Fatalf("run(%v) outcome = %q code = %q, want held_for_confirmation app.config", args, oc.Outcome, oc.Code)
		}
		if !oc.ConfirmRequired {
			t.Errorf("run(%v) held write must set confirm_required", args)
		}
		if code != exitCodeHeld {
			t.Errorf("run(%v) exit code = %d, want %d", args, code, exitCodeHeld)
		}

		out, code = runMutate(t, f, append(args, "--confirm")...)
		if oc := decodeOutcome(t, out); oc.Outcome != outcomeExecuted {
			t.Fatalf("run(%v --confirm) outcome = %q, want executed", args, oc.Outcome)
		}
		if code != exitCodeExecuted {
			t.Errorf("run(%v --confirm) exit code = %d, want 0", args, code)
		}
	}
}

// TestExposeStillReachesPublish pins the alias. `expose` was this surface's routing verb and did
// strictly less than publish — routing only, reported as executed with an http:// URL that a browser
// on an HSTS-preloaded domain refuses to open (issue #476). An agent that was told the old verb must
// land on the whole operation rather than on `unknown command`, which ADR-0065 §5 calls the dead end
// that invites an agent to route around the control channel.
func TestExposeStillReachesPublish(t *testing.T) {
	f := newFakeCP(t)
	var gotPath string
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "host": "web.example.com", "reachable": true, "url": "https://web.example.com"})
	}

	out, code := runMutate(t, f, "expose", "web", "--host", "web.example.com", "--port", "8080", "--confirm")
	if oc := decodeOutcome(t, out); oc.Outcome != outcomeExecuted || oc.Operation != "publish" {
		t.Fatalf("outcome = %q operation = %q, want executed publish", oc.Outcome, oc.Operation)
	}
	if code != exitCodeExecuted {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotPath != "/v1/apps/web/publish" {
		t.Errorf("path = %q, want the publish operation", gotPath)
	}

	// unexpose is the same story on the removing side.
	if out, _ := runMutate(t, f, "unexpose", "web"); decodeOutcome(t, out).Operation != "unpublish" {
		t.Errorf("unexpose ran %q, want unpublish", decodeOutcome(t, out).Operation)
	}
}

// TestADeployTellsTheAgentWhatTheRolloutDid is issue #546 at the surface that matters most. A human
// eventually looks at a pod list; the agent has the result and nothing else, so a rollout that never
// became ready has to be IN the result, structurally, alongside the reason and the pod's own
// explanation (ADR-0092 §2).
//
// The outcome stays `executed`, deliberately. Those four values answer "did the operation run, or
// did a guardrail stop it" (ADR-0049, ADR-0020), and this operation ran — the same way a
// `burrow-agent run` whose command exits non-zero is an executed run with the exit code in its
// result. Folding a rollout verdict into that vocabulary would make a guardrail hold and a wedged
// rollout the same kind of answer, which they are not.
func TestADeployTellsTheAgentWhatTheRolloutDid(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release": map[string]any{"id": "r1", "app": "web", "image": "img:2", "status": "deployed"},
			"rollout": map[string]any{
				"settled": false,
				"reason":  "CrashLoopBackOff",
				"detail":  "0 of 1 replicas updated, 0 ready",
				"issue":   `container "web" is crash-looping (exit 1): listing tenants is forbidden`,
			},
		})
	}
	out, _ := runMutate(t, f, "deploy", "web", "--image", "img:2")

	var envelope struct {
		Outcome string `json:"outcome"`
		Result  struct {
			Rollout *struct {
				Settled bool   `json:"settled"`
				Reason  string `json:"reason"`
				Issue   string `json:"issue"`
			} `json:"rollout"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("outcome is not valid JSON: %v (%q)", err, out)
	}
	if envelope.Outcome != outcomeExecuted {
		t.Errorf("outcome = %q, want executed: the operation ran, and the rollout's verdict is in the result", envelope.Outcome)
	}
	if envelope.Result.Rollout == nil {
		t.Fatalf("the result carries no rollout, so nothing in this envelope says the image is not serving: %s", out)
	}
	if envelope.Result.Rollout.Settled {
		t.Error("rollout reads as settled")
	}
	if envelope.Result.Rollout.Reason != "CrashLoopBackOff" {
		t.Errorf("reason = %q, want CrashLoopBackOff", envelope.Result.Rollout.Reason)
	}
	if !strings.Contains(envelope.Result.Rollout.Issue, "listing tenants is forbidden") {
		t.Errorf("issue = %q, want the pod's own reason", envelope.Result.Rollout.Issue)
	}
}

// TestTheAgentCannotDeclineToWait keeps the escape hatch off the agent surface (ADR-0092 §3). The
// agent's whole exposure here is being told an operation worked when it did not, so the flag that
// restores that answer is one only a human runs — the same line ADR-0080 §3 draws around
// `--skip-hooks`.
func TestTheAgentCannotDeclineToWait(t *testing.T) {
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"deploy", "web", "--image", "img:1", "--wait=false"}, &out, &errb)
	if err == nil {
		t.Fatal("`burrow-agent deploy --wait=false` succeeded; answering before the rollout is a human's call")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error = %v, want an unknown-flag rejection", err)
	}
	// Absent rather than ignored, for the reason the same rule gives for --skip-hooks: a flag the
	// binary accepts and drops reads as having worked.
	if f := newDeployCmd().Flags().Lookup("wait"); f != nil {
		t.Error("the agent's deploy command defines a wait flag")
	}
}

// TestARollbackTellsTheAgentWhatTheRolloutDid is issue #548 at the surface that matters most. The
// agent has the result and nothing else, and a rollback is the operation where being told it worked
// when it did not costs the most: the release still serving is the one the rollback was moving away
// from (ADR-0093 §2).
//
// The outcome stays `executed` for the reason the deploy's does — the operation ran, and the
// rollout's verdict belongs in the result rather than in the four values that answer whether a
// guardrail stopped it.
func TestARollbackTellsTheAgentWhatTheRolloutDid(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release":                   map[string]any{"id": "r3", "app": "web", "image": "img:1", "status": "deployed"},
			"rolled_back_to_release_id": "r1",
			"superseded_release_id":     "r2",
			"rollout": map[string]any{
				"settled": false,
				"reason":  "CrashLoopBackOff",
				"issue":   `container "web" is crash-looping (exit 1): migration 0041 is already applied`,
			},
		})
	}
	out, _ := runMutate(t, f, "rollback", "web")

	var envelope struct {
		Outcome string `json:"outcome"`
		Result  struct {
			SupersededReleaseID string `json:"superseded_release_id"`
			Rollout             *struct {
				Settled bool   `json:"settled"`
				Reason  string `json:"reason"`
				Issue   string `json:"issue"`
			} `json:"rollout"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("outcome is not valid JSON: %v (%q)", err, out)
	}
	if envelope.Outcome != outcomeExecuted {
		t.Errorf("outcome = %q, want executed: the operation ran, and the rollout's verdict is in the result", envelope.Outcome)
	}
	if envelope.Result.Rollout == nil {
		t.Fatalf("the result carries no rollout, so nothing in this envelope says the recovery did not take: %s", out)
	}
	if envelope.Result.Rollout.Settled || envelope.Result.Rollout.Reason != "CrashLoopBackOff" {
		t.Errorf("rollout = %+v, want the unsettled observation", envelope.Result.Rollout)
	}
	if !strings.Contains(envelope.Result.Rollout.Issue, "migration 0041 is already applied") {
		t.Errorf("issue = %q, want the pod's own reason", envelope.Result.Rollout.Issue)
	}
	// The release still serving. Without it the agent cannot say which image is up, and rolling back
	// again would return to exactly this release.
	if envelope.Result.SupersededReleaseID != "r2" {
		t.Errorf("superseded_release_id = %q, want r2 — the release the rollback was moving away from", envelope.Result.SupersededReleaseID)
	}
}

// TestTheAgentCannotDeclineToWaitOnARollback keeps the escape hatch off the agent surface for the
// rollback too (ADR-0093 §2). It is the same line ADR-0092 §3 draws around the deploy's flag and
// ADR-0080 §3 draws around `--skip-hooks`.
func TestTheAgentCannotDeclineToWaitOnARollback(t *testing.T) {
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"rollback", "web", "--wait=false"}, &out, &errb)
	if err == nil {
		t.Fatal("`burrow-agent rollback --wait=false` succeeded; answering before the rollout is a human's call")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error = %v, want an unknown-flag rejection", err)
	}
	if f := newRollbackCmd().Flags().Lookup("wait"); f != nil {
		t.Error("the agent's rollback command defines a wait flag")
	}
}
