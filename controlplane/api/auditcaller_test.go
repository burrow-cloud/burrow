// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/burrow-cloud/burrow/client"
	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The audit trail's end of per-caller identity (ADR-0084 §9), exercised over HTTP rather than by
// handing the engine a context.
//
// The engine tests (controlplane/caller_test.go) prove the seam records what is put on the context.
// These prove the thing that has to be true in production: that a token presented on a request
// reaches the row. Everything between is in the path — authentication, the middleware, the handler
// passing r.Context() along, the engine's seam, the wire encoding, the client's struct — and a break
// anywhere in it turns a real actor back into the constant without failing anything else.

// newAuditIdentityAPI is newIdentityAPI with deploys allowed, so a test can get an audited operation
// past the guardrail and read the row it wrote.
func newAuditIdentityAPI(t *testing.T) (http.Handler, *fake.Database) {
	t.Helper()
	h, d := newIdentityAPI(t, "install-abc")
	d.SetPolicy(cp.Policy{}.With(cp.GuardrailAppDeploy, cp.DispositionAllow))
	return h, d
}

// deployAudit deploys app as the holder of tok and returns the audit rows for it, decoded through
// the CLIENT's struct. Decoding through the client is deliberate: it is the shape a reviewer
// actually reads, and a json tag that disagreed across the API would drop the field in silence.
func deployAudit(t *testing.T, h http.Handler, tok, app string) []client.AuditEntry {
	t.Helper()
	if rr := do(h, "POST", "/v1/apps/"+app+"/deploy", tok, `{"image":"registry.example.com/`+app+`:1","replicas":1}`); rr.Code != http.StatusOK {
		t.Fatalf("deploy %s = %d %s", app, rr.Code, rr.Body.String())
	}
	rec := do(h, "GET", "/v1/audit?app="+app, tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit %s = %d %s", app, rec.Code, rec.Body.String())
	}
	var out struct {
		Entries []client.AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode audit into the client struct: %v", err)
	}
	if len(out.Entries) == 0 {
		t.Fatalf("no audit rows for %s", app)
	}
	return out.Entries
}

// TestAuditNamesThePrincipalAndTheKindOverTheWire: a person signs in, their agent is issued a
// credential of its own, and each deploys. Every row names ADA — one principal — and the caller
// column says which of her two credentials acted.
//
// That pair is why there are two columns. "Ada deployed this" and "Ada's agent deployed this" are
// different facts with different follow-ups, and a trail that recorded one string for both, or
// recorded the kind and lost the person, answers neither.
func TestAuditNamesThePrincipalAndTheKindOverTheWire(t *testing.T) {
	h, _ := newAuditIdentityAPI(t)

	rec := claim(h, token, "ada")
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	person := decodeCredential(t, rec)

	rec = agentCredential(h, person.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent credential: %d %s", rec.Code, rec.Body.String())
	}
	agent := decodeCredential(t, rec)

	for _, tc := range []struct {
		app, tok, wantCaller string
	}{
		{app: "web", tok: person.Token, wantCaller: string(cp.CredentialKindUser)},
		{app: "api", tok: agent.Token, wantCaller: string(cp.CredentialKindAgent)},
	} {
		for i, e := range deployAudit(t, h, tc.tok, tc.app) {
			if e.Principal != "ada" {
				t.Errorf("%s row[%d] principal = %q, want ada — the trail records who acted", tc.app, i, e.Principal)
			}
			if e.Caller != tc.wantCaller {
				t.Errorf("%s row[%d] caller = %q, want %q — the trail records what kind of credential they held",
					tc.app, i, e.Caller, tc.wantCaller)
			}
		}
	}
}

// TestAuditOnTheSharedTokenNamesNobodyOverTheWire: the install's shared token names nobody, so the
// rows say so. This is the case that must not improve itself into a guess.
//
// The install here HAS a principal — ada claimed it — and she is its only one, so attributing a
// shared-token action to her would look like an enrichment and be a false statement about who was at
// the keyboard. The shared token is exactly the credential anybody may be holding: the operator, a
// script somebody wrote, a client too old to sign in. `shared-agent` / `control-plane` is the true
// answer, and it is the same answer these rows carried before credentials existed, so an install
// that issues none sees no change at all.
func TestAuditOnTheSharedTokenNamesNobodyOverTheWire(t *testing.T) {
	h, _ := newAuditIdentityAPI(t)
	if rec := claim(h, token, "ada"); rec.Code != http.StatusOK {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}

	for i, e := range deployAudit(t, h, token, "web") {
		if e.Principal != "shared-agent" {
			t.Errorf("row[%d] principal = %q, want shared-agent: the shared token names nobody, and the install's "+
				"only principal was not necessarily the one holding it", i, e.Principal)
		}
		if e.Caller != "control-plane" {
			t.Errorf("row[%d] caller = %q, want control-plane: there is no credential row to read a kind from", i, e.Caller)
		}
	}
}
