// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestHookEndpoints exercises the lifecycle-hook surface end to end over HTTP (ADR-0072 §1): one
// route with the phase in the path, a listing that omits the phases with no hook, and an unset that
// returns the app to today's behaviour.
func TestHookEndpoints(t *testing.T) {
	h, _, _ := newAPI(t)

	// Nothing set: an empty listing, not a 404.
	rr := do(h, "GET", "/v1/apps/web/hooks", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"hooks":[]`) {
		t.Fatalf("hook list with none set = %d %s", rr.Code, rr.Body.String())
	}

	rr = do(h, "PUT", "/v1/apps/web/hooks/pre-deploy", token, `{"command":["./manage.py","migrate"]}`)
	if rr.Code != 200 {
		t.Fatalf("hook set = %d %s", rr.Code, rr.Body.String())
	}
	var hook cp.Hook
	if err := json.Unmarshal(rr.Body.Bytes(), &hook); err != nil {
		t.Fatalf("decoding hook: %v", err)
	}
	if hook.App != "web" || hook.Phase != cp.HookPreDeploy || len(hook.Command) != 2 {
		t.Fatalf("hook = %+v, want the app, the phase and the argv", hook)
	}

	rr = do(h, "GET", "/v1/apps/web/hooks", token, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "pre-deploy") || strings.Contains(rr.Body.String(), "pre-rollback") {
		t.Fatalf("hook list = %d %s, want only the phase that has a hook", rr.Code, rr.Body.String())
	}

	rr = do(h, "DELETE", "/v1/apps/web/hooks/pre-deploy", token, "")
	if rr.Code != 200 {
		t.Fatalf("hook unset = %d %s", rr.Code, rr.Body.String())
	}
	// Unsetting a phase with no hook succeeds: the state it asks for already holds.
	if rr := do(h, "DELETE", "/v1/apps/web/hooks/pre-rollback", token, ""); rr.Code != 200 {
		t.Fatalf("unset of an absent hook = %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(h, "GET", "/v1/apps/web/hooks", token, ""); !strings.Contains(rr.Body.String(), `"hooks":[]`) {
		t.Fatalf("hook list after unset = %s", rr.Body.String())
	}
}

// TestHookSetRejectsAnUnrunnablePhase asserts a phase nothing fires is refused as a bad request
// rather than stored as a setting that never runs (ADR-0009). `post-rollback` is the interesting
// one: it is what a reader expects to exist, and it does not — a rollback fires `post-deploy`, told
// it was a rollback (ADR-0072 §4).
func TestHookSetRejectsAnUnrunnablePhase(t *testing.T) {
	h, _, _ := newAPI(t)
	for _, phase := range []string{"during-deploy", "post-rollback"} {
		rr := do(h, "PUT", "/v1/apps/web/hooks/"+phase, token, `{"command":["./x"]}`)
		if rr.Code != 400 {
			t.Errorf("set %s = %d %s, want 400", phase, rr.Code, rr.Body.String())
		}
	}
}

// TestFailedPreDeployHookIsAStructuredDeployRefusal asserts a hook failure comes back as an
// unprocessable request carrying the phase, the exit code and the command's own output — not a 500,
// and not a confirmation prompt, because there is nothing to confirm (ADR-0072 §3).
func TestFailedPreDeployHookIsAStructuredDeployRefusal(t *testing.T) {
	h, k, _ := newAPI(t)
	if rr := do(h, "PUT", "/v1/apps/web/hooks/pre-deploy", token, `{"command":["./migrate"]}`); rr.Code != 200 {
		t.Fatalf("hook set = %d %s", rr.Code, rr.Body.String())
	}
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: "migration 003 failed\n"})

	rr := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	if rr.Code != 422 {
		t.Fatalf("deploy with a failing hook = %d %s, want 422", rr.Code, rr.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	if e.Code != "hook_failed" {
		t.Errorf("code = %q, want hook_failed so a caller can branch on the cause", e.Code)
	}
	if e.NeedsConfirmation {
		t.Error("needs_confirmation is set, but a failed hook is not something a confirmation opens")
	}
	for _, want := range []string{"pre-deploy", "exit code 1", "migration 003 failed"} {
		if !strings.Contains(e.Error, want) {
			t.Errorf("error = %q, want it to mention %q", e.Error, want)
		}
	}
	if _, ok := k.Spec("web"); ok {
		t.Error("a workload was applied: a failed pre-deploy hook must abort the deploy")
	}
}

// TestSkippingAPreRollbackHookIsExplicitOverTheWire is ADR-0080 on the transport. Three things have
// to hold at this layer and none of them is visible from the engine test: the abort still happens
// when nothing asked for a skip, the skip happens only for the literal `skip_hooks=true`, and the
// refusal an agent receives carries the command a human runs — since the agent's binary has no flag
// that would produce this request at all (§3).
func TestSkippingAPreRollbackHookIsExplicitOverTheWire(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:2","replicas":1}`)
	if rr := do(h, "PUT", "/v1/apps/web/hooks/pre-rollback", token, `{"command":["./migrate","down"]}`); rr.Code != 200 {
		t.Fatalf("hook set = %d %s", rr.Code, rr.Body.String())
	}
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: "could not connect\n"})

	// No parameter, and a parameter that is not the literal "true": the hook runs and the abort stands.
	for _, path := range []string{"/v1/apps/web/rollback", "/v1/apps/web/rollback?skip_hooks=1", "/v1/apps/web/rollback?skip_hooks=yes"} {
		rr := do(h, "POST", path, token, "")
		if rr.Code != 422 {
			t.Fatalf("POST %s = %d %s, want 422: only an explicit skip_hooks=true skips a safety step", path, rr.Code, rr.Body.String())
		}
		var e errBody
		if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
			t.Fatalf("decoding error body: %v", err)
		}
		if e.Code != "hook_failed" {
			t.Errorf("code = %q, want hook_failed", e.Code)
		}
		if e.NeedsConfirmation {
			t.Error("needs_confirmation is set on a blocked rollback; the way past it is an operator command, " +
				"not a confirmation the same caller can re-issue (ADR-0080 §3)")
		}
		if !strings.Contains(e.Error, "burrow app rollback web --skip-hooks") {
			t.Errorf("the refusal does not name the command a human runs: %q", e.Error)
		}
		if spec, _ := k.Spec("web"); spec.Image != "img:2" {
			t.Fatalf("cluster image = %q, want img:2 still serving after the abort", spec.Image)
		}
	}

	rr := do(h, "POST", "/v1/apps/web/rollback?skip_hooks=true", token, "")
	if rr.Code != 200 {
		t.Fatalf("rollback with skip_hooks=true = %d %s", rr.Code, rr.Body.String())
	}
	var res cp.RollbackResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding rollback result: %v", err)
	}
	if spec, _ := k.Spec("web"); spec.Image != "img:1" {
		t.Errorf("cluster image = %q, want img:1: the rollback must have landed", spec.Image)
	}
	if !strings.Contains(strings.Join(res.Hints, "\n"), "SKIPPED") {
		t.Errorf("the response does not report the skip: %+v", res.Hints)
	}
}
