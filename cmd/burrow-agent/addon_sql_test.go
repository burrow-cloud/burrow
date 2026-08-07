// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAddonSQLDenialIsWhatTheAgentSees pins the half of ADR-0087 §5 that decides whether the verb is
// on this binary at all. A denied statement is an OUTCOME the agent can read and relay — "denied",
// with the code — and not an error or an unknown command. An agent that meets a dead end routes
// around the control channel; one that is told the capability exists and is closed asks for it.
func TestAddonSQLDenialIsWhatTheAgentSees(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		denied(w, "addon sql", "addon.sql", "running a statement against \"web\"'s database in environment prod is denied by the current guardrail policy")
	}
	out, code := runMutate(t, f, "addon", "sql", "postgres", "web", "-c", "select 1")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeDenied || oc.Code != "addon.sql" {
		t.Fatalf("outcome = %q code = %q, want denied addon.sql", oc.Outcome, oc.Code)
	}
	if oc.ConfirmRequired {
		t.Error("a deny must not set confirm_required: no --confirm opens it (ADR-0087 §5)")
	}
	if code != exitCodeDenied {
		t.Errorf("exit code = %d, want %d", code, exitCodeDenied)
	}
}

// TestAddonSQLDatabaseErrorIsExecuted pins ADR-0087 §4 on the agent surface: a statement the database
// refuses is an EXECUTED outcome carrying the error and its SQLSTATE, not a failed call. The
// distinction is the one an agent acts on — 42P01 is a missing table it can create, while a transport
// failure is not something it should retry the same way.
func TestAddonSQLDatabaseErrorIsExecuted(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"addon": "postgres", "app": "web", "environment": "prod",
			"columns": []string{}, "rows": []any{}, "row_count": 0,
			"error": map[string]any{"message": `relation "users" does not exist`, "sqlstate": "42P01"},
		})
	}
	out, code := runMutate(t, f, "addon", "sql", "postgres", "web", "-c", "select * from users")
	oc := decodeOutcome(t, out)
	if oc.Outcome != outcomeExecuted {
		t.Fatalf("outcome = %q, want executed: the call worked, the statement did not", oc.Outcome)
	}
	if code != exitCodeExecuted {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "42P01") {
		t.Errorf("the SQLSTATE did not survive into the envelope:\n%s", out)
	}
}

// TestAddonSQLSendsTheStatementVerbatim asserts what crosses the channel is the statement and the
// pair that names the database, and nothing that classifies it (ADR-0087 §6). No secret goes with it:
// the credential is minted and spent server-side.
func TestAddonSQLSendsTheStatementVerbatim(t *testing.T) {
	const stmt = "WITH deleted AS (DELETE FROM users RETURNING *) SELECT * FROM deleted"
	f := newFakeCP(t)
	var body map[string]any
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"addon": "postgres", "app": "web", "row_count": 0})
	}
	if _, code := runMutate(t, f, "addon", "sql", "postgres", "web", "-c", stmt); code != exitCodeExecuted {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if body["statement"] != stmt {
		t.Errorf("statement sent = %v, want it verbatim", body["statement"])
	}
	if body["addon"] != "postgres" || body["app"] != "web" {
		t.Errorf("body = %v, want the add-on type and app that name the database", body)
	}
}
