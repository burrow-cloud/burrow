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
		denied(w, "scale", "app.replica_ceiling", "requested 99 replicas exceeds the policy ceiling of 5")
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
		t.Error("denied outcome must not set confirm_required")
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

// TestMutatingVerbsPresent confirms the Phase 2a compute verbs are compiled in: each resolves to a
// valid outcome envelope rather than an unknown-command error. It is the positive counterpart to
// TestAdminVerbsAbsent.
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
		{"deploy", "web", "--image", "img:1"},
		{"rollback", "web"},
		{"scale", "web", "3"},
		{"autoscale", "web"},
		{"run", "web", "--", "echo", "hi"},
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
