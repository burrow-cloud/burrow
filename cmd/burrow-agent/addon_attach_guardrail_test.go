// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ADR-0095 puts `addon attach` in ADR-0065 §3's TIER 3 — compiled into this binary and held for
// confirmation — and these are the two halves that makes real on the agent surface: a held attach
// reaches the agent as an OUTCOME it can relay rather than an error, and --confirm exists so the
// agent can re-issue the call once a human has approved what the hold described.

// TestAttachHoldIsWhatTheAgentSees. The agent's job on a hold is to surface it, so the envelope has
// to carry the code, the confirm_required flag, and the sentence naming what the attach would do.
func TestAttachHoldIsWhatTheAgentSees(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		held(w, "addon attach", "addon.attach", `attaching "web" to the postgres instance "burrow-postgres" in environment prod (creates a database and a login role on it, writes the connection string into DATABASE_URL, and restarts the app) requires confirmation to proceed`)
	}
	out, code := runMutate(t, f, "addon", "attach", "postgres", "web")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeHeld || oc.Code != "addon.attach" {
		t.Fatalf("outcome = %q code = %q, want held addon.attach", oc.Outcome, oc.Code)
	}
	if !oc.ConfirmRequired {
		t.Error("a hold must set confirm_required: the agent's next step is to ask a human, then re-run with --confirm")
	}
	if code != exitCodeHeld {
		t.Errorf("exit code = %d, want %d", code, exitCodeHeld)
	}
	if !strings.Contains(out, "restarts the app") {
		t.Errorf("the consequence did not survive into the envelope, so there is nothing for the agent to relay:\n%s", out)
	}
}

// TestAttachConfirmRidesTheBody. --confirm is the only way past the hold, and it travels in the body
// rather than the route: a control plane that predates the guardrail has none to satisfy, so it does
// what it always did rather than something different from what was asked.
func TestAttachConfirmRidesTheBody(t *testing.T) {
	f := newFakeCP(t)
	var gotPath, gotBody string
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "addon": "postgres", "environment": "prod", "secret_key": "DATABASE_URL"})
	}
	out, code := runMutate(t, f, "addon", "attach", "postgres", "web", "--confirm")
	if oc := decodeOutcome(t, out); oc.Outcome != outcomeExecuted {
		t.Fatalf("outcome = %q (exit %d), want executed", oc.Outcome, code)
	}
	if gotPath != "/v1/addons/attach" {
		t.Errorf("path = %q, want the unnarrowed attach route: the confirmation narrows nothing", gotPath)
	}
	if !strings.Contains(gotBody, `"confirm":true`) {
		t.Errorf("the confirmation did not reach the control plane: %s", gotBody)
	}
}

// TestAttachWithoutConfirmSaysSo is the other side of the same wire check: the flag is not sent by
// default, so an agent that never passes it cannot accidentally satisfy the hold.
func TestAttachWithoutConfirmSaysSo(t *testing.T) {
	f := newFakeCP(t)
	var gotBody string
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "addon": "postgres", "environment": "prod", "secret_key": "DATABASE_URL"})
	}
	runMutate(t, f, "addon", "attach", "postgres", "web")
	if strings.Contains(gotBody, `"confirm":true`) {
		t.Errorf("an attach with no --confirm claimed a confirmation: %s", gotBody)
	}
}
