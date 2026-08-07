// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// `addon attach --as` on the AGENT surface, under ADR-0065 §1's two tests (issue #462). It fails
// neither: its effect stays inside the app's own Secret in the environment the attach already targets
// (scope), and the one irreversible thing it could do — overwrite a value nobody can read back — is
// refused by the control plane rather than performed (reversibility). The agent is also the party that
// knows which variable the app it is deploying reads; withholding the name leaves it writing a
// start-up wrapper to copy one variable to another, which is the workaround the flag removes.

// TestAgentAttachSendsTheChosenVariable asserts the flag reaches the wire as the route segment rather
// than being accepted and dropped.
func TestAgentAttachSendsTheChosenVariable(t *testing.T) {
	f := newFakeCP(t)
	var gotPath string
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "addon": "postgres", "secret_key": "PG_DSN"})
	}

	out, code := runMutate(t, f, "addon", "attach", "postgres", "web", "--as", "PG_DSN")
	if code != exitCodeExecuted {
		t.Fatalf("exit code = %d, want 0: %s", code, out)
	}
	if gotPath != "/v1/addons/attach/env-key/PG_DSN" {
		t.Errorf("path = %q, want the chosen variable in the route", gotPath)
	}
	if !strings.Contains(out, "PG_DSN") {
		t.Errorf("the result does not report the key it wrote, which is all the agent gets back: %s", out)
	}
}

// TestAgentAttachWithoutTheFlagIsUnchanged: the default did not move, and neither did the wire shape.
func TestAgentAttachWithoutTheFlagIsUnchanged(t *testing.T) {
	f := newFakeCP(t)
	var gotPath string
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "addon": "postgres", "secret_key": "DATABASE_URL"})
	}

	if _, code := runMutate(t, f, "addon", "attach", "postgres", "web"); code != exitCodeExecuted {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if gotPath != "/v1/addons/attach" {
		t.Errorf("path = %q, want the unnarrowed attach", gotPath)
	}
}
