// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestBuildExecuted: a build the control plane accepts prints outcome "executed" carrying the built
// digest and the resulting deploy (release + rollback handle), exits 0, and calls the SAME Phase 4
// endpoint (POST /v1/apps/{app}/build) with the SourceRef, target image, and app the flags name — the
// verb reuses client.Build, adding no new endpoint (ADR-0053, ADR-0049).
func TestBuildExecuted(t *testing.T) {
	f := newFakeCP(t)
	var (
		gotPath   string
		gotMethod string
		gotBody   struct {
			Env         string `json:"env"`
			Source      struct{ Repo, Ref string }
			TargetImage string `json:"target_image"`
			Confirm     bool   `json:"confirm"`
		}
	)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"digest": "sha256:abc123",
			"deploy": map[string]any{
				"release":               map[string]any{"id": "r7", "app": "web", "image": "reg/web:1.4.0", "status": "deployed", "replicas": 2},
				"superseded_release_id": "r6",
			},
		})
	}
	out, code := runMutate(t, f, "build", "web",
		"--source", "https://github.com/user/app", "--ref", "v1.4.0", "--image", "reg/web:1.4.0")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeExecuted {
		t.Fatalf("outcome = %q, want executed: %q", oc.Outcome, out)
	}
	if oc.Operation != "build" {
		t.Errorf("operation = %q, want build", oc.Operation)
	}
	if code != exitCodeExecuted {
		t.Errorf("exit code = %d, want %d", code, exitCodeExecuted)
	}
	// The endpoint reused is the Phase 4 build handler, not a new one.
	if gotMethod != http.MethodPost || gotPath != "/v1/apps/web/build" {
		t.Errorf("called %s %s, want POST /v1/apps/web/build", gotMethod, gotPath)
	}
	// The flags map onto the SourceRef, target image, and app the request carries.
	if gotBody.Source.Repo != "https://github.com/user/app" || gotBody.Source.Ref != "v1.4.0" {
		t.Errorf("source = %+v, want {Repo:https://github.com/user/app Ref:v1.4.0}", gotBody.Source)
	}
	if gotBody.TargetImage != "reg/web:1.4.0" {
		t.Errorf("target_image = %q, want reg/web:1.4.0", gotBody.TargetImage)
	}
	// The result carries the built digest and the deploy that shipped it — what the agent composes over.
	result, _ := oc.Result.(map[string]any)
	if result == nil {
		t.Fatalf("build result missing: %q", out)
	}
	if result["digest"] != "sha256:abc123" {
		t.Errorf("result digest = %v, want sha256:abc123", result["digest"])
	}
	deploy, _ := result["deploy"].(map[string]any)
	if deploy == nil {
		t.Fatalf("build result carries no deploy: %v", result)
	}
	rel, _ := deploy["release"].(map[string]any)
	if rel == nil || rel["id"] != "r7" {
		t.Errorf("deploy release = %v, want release id r7", deploy["release"])
	}
}

// TestBuildHeldThenConfirm: because a build ends in a deploy, the app.deploy guardrail holds it for
// confirmation exactly as it holds a deploy — outcome held_for_confirmation, exit 2, and the binary
// does NOT self-confirm (the first request carries confirm=false). Re-running with --confirm reaches
// the control plane with confirm=true and executes.
func TestBuildHeldThenConfirm(t *testing.T) {
	f := newFakeCP(t)
	var sawConfirm []bool
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Confirm bool `json:"confirm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sawConfirm = append(sawConfirm, body.Confirm)
		if !body.Confirm {
			held(w, "deploy", "app.deploy", "building and deploying a new release to prod requires confirmation")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"digest": "sha256:def456",
			"deploy": map[string]any{"release": map[string]any{"id": "r8", "app": "web", "status": "deployed"}},
		})
	}

	// First invocation: no --confirm. Held for the human's approval.
	out, code := runMutate(t, f, "build", "web",
		"--source", "https://github.com/user/app", "--ref", "v2.0.0", "--image", "reg/web:2.0.0")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeHeld {
		t.Fatalf("outcome = %q, want held_for_confirmation: %q", oc.Outcome, out)
	}
	if oc.Code != "app.deploy" {
		t.Errorf("code = %q, want app.deploy", oc.Code)
	}
	if !oc.ConfirmRequired {
		t.Error("held build must set confirm_required")
	}
	if code != exitCodeHeld {
		t.Errorf("exit code = %d, want %d", code, exitCodeHeld)
	}

	// Second invocation: the human approved, so a human re-runs with --confirm. Executes.
	out, code = runMutate(t, f, "build", "web",
		"--source", "https://github.com/user/app", "--ref", "v2.0.0", "--image", "reg/web:2.0.0", "--confirm")
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

// runBuildExpectingRejection drives a build whose client-side validation should reject it before any
// call, and returns the plain (non-exit) error. It asserts the control plane was NEVER contacted — the
// verb validates the source ref and the target image up front, like the human `burrow app build` CLI.
func runBuildExpectingRejection(t *testing.T, args ...string) error {
	t.Helper()
	f := newFakeCP(t)
	called := false
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}
	var out, errb bytes.Buffer
	full := append(append([]string{}, args...), "--control-plane", f.srv.URL, "--token", "t")
	err := run(context.Background(), full, &out, &errb)
	if err == nil {
		t.Fatalf("run(%v) returned nil, want a validation error (stdout %q)", args, out.String())
	}
	if called {
		t.Errorf("run(%v) reached the control plane; a malformed build must be rejected before any call", args)
	}
	return err
}

// TestBuildValidatesSourceRefBeforeCall: a build missing its source repo or its ref is rejected
// client-side, before any request, reusing controlplane.SourceRef.Validate — the same pre-flight check
// the human CLI verb performs. A missing --image is likewise rejected up front.
func TestBuildValidatesSourceRefBeforeCall(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing source repo",
			args: []string{"build", "web", "--ref", "v1.0.0", "--image", "reg/web:1"},
			want: "repository",
		},
		{
			name: "missing ref",
			args: []string{"build", "web", "--source", "https://github.com/user/app", "--image", "reg/web:1"},
			want: "ref",
		},
		{
			name: "missing target image",
			args: []string{"build", "web", "--source", "https://github.com/user/app", "--ref", "v1.0.0"},
			want: "--image",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runBuildExpectingRejection(t, tc.args...)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestBuildGoodSourceAccepted is the positive counterpart: a well-formed source ref and target image
// pass client-side validation and reach the control plane (the executed path is covered by
// TestBuildExecuted). This guards against the validator rejecting a valid reference.
func TestBuildGoodSourceAccepted(t *testing.T) {
	f := newFakeCP(t)
	called := false
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"digest": "sha256:0",
			"deploy": map[string]any{"release": map[string]any{"id": "r1", "app": "web", "status": "deployed"}},
		})
	}
	out, code := runMutate(t, f, "build", "web",
		"--source", "git@github.com:user/app.git", "--ref", "9f3c2a1", "--image", "reg/web:9f3c2a1")
	if !called {
		t.Fatalf("a valid build did not reach the control plane: %q", out)
	}
	if oc := decodeOutcome(t, out); oc.Outcome != outcomeExecuted || code != exitCodeExecuted {
		t.Errorf("outcome = %q exit = %d, want executed 0", oc.Outcome, code)
	}
}
