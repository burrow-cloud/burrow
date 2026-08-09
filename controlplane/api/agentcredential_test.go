// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The HTTP tests for the agent's own credential (ADR-0084 §3).
//
// The property: the agent and the person hold DIFFERENT credentials belonging to the SAME principal,
// so revoking one leaves the other working, and the row records which of the two acted.

// agentCredential POSTs the agent-credential route as a caller holding tok.
func agentCredential(h http.Handler, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/auth/agent", nil)
	req.Header.Set("X-Burrow-Token", tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTheAgentGetsItsOwnCredential: a signed-in person's agent is issued a separate row of kind
// `agent`, under the same principal, and it authenticates.
func TestTheAgentGetsItsOwnCredential(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")
	person := signedInAdmin(t, h)

	rec := agentCredential(h, person.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent credential: %d %s", rec.Code, rec.Body.String())
	}
	agent := decodeCredential(t, rec)

	if agent.Token == person.Token {
		t.Fatal("the agent was handed the person's own token; revoking either would stop both")
	}
	if agent.CredentialID == person.CredentialID {
		t.Error("the agent's credential is the person's row, so it cannot be revoked on its own")
	}
	if agent.PrincipalID != person.PrincipalID {
		t.Errorf("agent principal = %q, want the person's %q: it is their agent, not a second identity",
			agent.PrincipalID, person.PrincipalID)
	}
	if agent.Kind != "agent" {
		t.Errorf("agent credential kind = %q, want agent — the kind is what a caller-aware guardrail will read", agent.Kind)
	}
	if agent.InstallID != "install-abc" {
		t.Errorf("agent install_id = %q, want install-abc", agent.InstallID)
	}

	// Both work, and each on its own.
	for name, tok := range map[string]string{"person": person.Token, "agent": agent.Token} {
		if rec := getAs(h, "/v1/apps", tok); rec.Code != http.StatusOK {
			t.Errorf("the %s credential does not authenticate: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// TestTheSharedTokenGetsTheAgentNothing: the install's shared token names nobody, so there is no
// principal for an agent credential to belong to. Issuing one against the install would recreate the
// shared credential the whole record exists to end.
func TestTheSharedTokenGetsTheAgentNothing(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")

	rec := agentCredential(h, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent credential with the shared install token = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "auth login") {
		t.Errorf("refusal = %q, want it to say to sign in first", rec.Body.String())
	}
}

// TestTheAgentCredentialRouteTakesNoKind pins the boundary the kind rests on. The route has no body
// at all, so there is nowhere for a caller to ask for the kind whose guardrails suit it, and the
// decoder refuses one that tries (ADR-0084 §3).
func TestTheAgentCredentialRouteTakesNoKind(t *testing.T) {
	h, _ := newIdentityAPI(t, "install-abc")
	person := signedInAdmin(t, h)

	agent := decodeCredential(t, agentCredential(h, person.Token))
	if agent.Kind != "agent" {
		t.Fatalf("kind = %q, want agent", agent.Kind)
	}
	// Asking again from the AGENT's own credential gets an agent credential too: the path is the
	// kind, so a caller holding one cannot trade up to a `user` credential through this route.
	second := decodeCredential(t, agentCredential(h, agent.Token))
	if second.Kind != "agent" {
		t.Errorf("an agent asking through its own credential got kind %q, want agent", second.Kind)
	}
}
