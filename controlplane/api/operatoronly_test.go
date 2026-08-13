// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// An agent may not rewrite its own limits (ADR-0099), over HTTP.
//
// The engine tests prove the rule. These prove the thing that has to be true in production: that
// EVERY SHAPE of the route the rule covers actually reaches it. The guardrail write has five — the
// global one, the name tier as a route, the older query form, and the two that carry a binding — and
// a rule that held on four of them would be a rule with a door left in it.

// guardWrites is every shape of "set a disposition" burrowd serves. They are listed here rather than
// derived, so adding a sixth shape to the mux and not to this list shows up as a test that does not
// cover it rather than as a silent gap.
var guardWrites = []struct {
	name, path string
}{
	{name: "global", path: "/v1/guard/app.delete"},
	{name: "one environment", path: "/v1/guard/app.delete?env=prod"},
	{name: "one app, in the route", path: "/v1/guard/name/website/app.deploy?env=prod"},
	{name: "one app, in the query", path: "/v1/guard/app.deploy?env=prod&name=website"},
	{name: "bound to a kind", path: "/v1/guard/binds/agent/app.delete"},
	{name: "bound to a kind, for one app", path: "/v1/guard/binds/agent/name/website/app.deploy?env=prod"},
}

// agentToken returns the token of the AGENT belonging to the person holding person: a real
// credential of kind `agent`, issued through the real route the way one is issued in production.
//
// That person is the install's admin, deliberately. The admin bit is a property of the principal, so
// it carries into the agent's credential — which is exactly why the kind is what these routes check.
func agentToken(t *testing.T, h http.Handler, person string) string {
	t.Helper()
	rec := agentCredential(h, person)
	if rec.Code != http.StatusOK {
		t.Fatalf("issuing the agent's credential: %d %s", rec.Code, rec.Body.String())
	}
	return decodeCredential(t, rec).Token
}

// mustRefuseAsOperatorOnly asserts the response is the refusal this record introduces: 403, its own
// code, and no confirmation to reach for.
func mustRefuseAsOperatorOnly(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s = %d %s, want 403", what, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"operator_only"`) {
		t.Errorf("%s answered %q, want the operator_only code", what, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"needs_confirmation":true`) {
		t.Errorf("%s answered %q with needs_confirmation; nothing here is satisfied by a confirmation", what, rec.Body.String())
	}
}

// TestAnAgentIsRefusedAtEveryGuardWriteShape is the first door (ADR-0099 §1), at the surface.
func TestAnAgentIsRefusedAtEveryGuardWriteShape(t *testing.T) {
	h, _, d := newAPI(t)
	agent := agentToken(t, h, operatorToken(t, h))
	before := storedPolicy(t, d)

	for _, w := range guardWrites {
		rec := do(h, "PUT", w.path, agent, `{"disposition":"allow"}`)
		mustRefuseAsOperatorOnly(t, "an agent setting a disposition ("+w.name+")", rec)

		// AND A CONFIRMATION DOES NOT OPEN IT. `confirm` is the flow an agent has learned from every
		// other held operation, so the same call carrying one has to answer identically — this is not
		// a guardrail, and there is nothing for a confirmation to satisfy.
		sep := "?"
		if strings.Contains(w.path, "?") {
			sep = "&"
		}
		rec = do(h, "PUT", w.path+sep+"confirm=true", agent, `{"disposition":"allow"}`)
		mustRefuseAsOperatorOnly(t, "an agent setting a disposition with --confirm ("+w.name+")", rec)
	}

	if p := storedPolicy(t, d); !reflect.DeepEqual(p.Dispositions, before.Dispositions) {
		t.Errorf("the refused writes changed the policy: %+v, want it as it was: %+v", p.Dispositions, before.Dispositions)
	}
}

// TestASharedTokenIsRefusedAtEveryGuardWriteShape is the load-bearing case (ADR-0099 §3), and the one
// that is easy to get backwards: on an install nobody has signed in to, the caller has no kind, and
// reading that as a person would leave the policy writable on precisely the installs whose only
// caller is an agent.
//
// This is a deliberate behaviour change: `burrow guard set` from a machine that has never run
// `burrow auth login` refuses, and the refusal says so.
func TestASharedTokenIsRefusedAtEveryGuardWriteShape(t *testing.T) {
	h, _, d := newAPI(t)
	before := storedPolicy(t, d)

	for _, w := range guardWrites {
		rec := do(h, "PUT", w.path, token, `{"disposition":"allow"}`)
		mustRefuseAsOperatorOnly(t, "the shared install token setting a disposition ("+w.name+")", rec)
		if !strings.Contains(rec.Body.String(), "auth login") {
			t.Errorf("refusal (%s) = %q, want it to say how the operator proceeds", w.name, rec.Body.String())
		}
	}

	if p := storedPolicy(t, d); !reflect.DeepEqual(p.Dispositions, before.Dispositions) {
		t.Errorf("the refused writes changed the policy: %+v, want it as it was: %+v", p.Dispositions, before.Dispositions)
	}
}

// TestAPersonMayStillWriteThePolicy: the lever has to stay where it was for the caller who holds it.
//
// It walks the same shapes as the refusals, which is what makes that list trustworthy: a path with a
// typo in it would answer 404 here rather than quietly proving nothing above.
func TestAPersonMayStillWriteThePolicy(t *testing.T) {
	h, _, d := newAPI(t)
	op := operatorToken(t, h)

	for _, w := range guardWrites {
		if rec := do(h, "PUT", w.path, op, `{"disposition":"allow"}`); rec.Code != http.StatusOK {
			t.Fatalf("a person setting a disposition (%s) = %d %s", w.name, rec.Code, rec.Body.String())
		}
	}
	if got := storedPolicy(t, d).Dispositions["app.delete"]; got != "allow" {
		t.Errorf("stored app.delete = %q, want allow", got)
	}
}

// TestAnAgentMayStillReadThePolicyOverTheWire. Reading is open to everybody, at every shape of the
// listing, because an agent that can see what binds it can explain a refusal to its person.
func TestAnAgentMayStillReadThePolicyOverTheWire(t *testing.T) {
	h, _, _ := newAPI(t)
	agent := agentToken(t, h, operatorToken(t, h))

	for _, path := range []string{"/v1/guard", "/v1/guard/name/website?env=prod", "/v1/guard?env=prod&name=website"} {
		if rec := do(h, "GET", path, agent, ""); rec.Code != http.StatusOK {
			t.Errorf("an agent reading %s = %d %s, want 200", path, rec.Code, rec.Body.String())
		}
	}
}

// TestAnAgentIsRefusedAtEveryIdentityRoute is the second door (ADR-0099 §2). An agent that can create
// a principal can create a principal that is not held — and the credential an invitation is exchanged
// for is of kind `user`, for which every disposition resolves to allow.
func TestAnAgentIsRefusedAtEveryIdentityRoute(t *testing.T) {
	h, _, _ := newAPI(t)
	agent := agentToken(t, h, operatorToken(t, h))

	for _, r := range []struct {
		name, path, body string
	}{
		{name: "creating an invitation", path: "/v1/auth/invitations", body: `{"name":"shadow","admin":true}`},
		{name: "exchanging an invitation", path: "/v1/auth/redeem"},
		{name: "issuing a credential", path: "/v1/auth/agent"},
	} {
		rec := do(h, "POST", r.path, agent, r.body)
		mustRefuseAsOperatorOnly(t, "an agent "+r.name, rec)
	}
}

// TestTheRoundTripIsClosed is the whole record in one path: an agent tries to mint itself a `user`
// credential and then relax the disposition that holds it. Both halves refuse, and the deny is still
// there afterwards.
func TestTheRoundTripIsClosed(t *testing.T) {
	h, _, d := newAPI(t)
	op := operatorToken(t, h)
	agent := agentToken(t, h, op)

	if rec := do(h, "PUT", "/v1/guard/app.delete", op, `{"disposition":"deny"}`); rec.Code != http.StatusOK {
		t.Fatalf("the operator setting the deny = %d %s", rec.Code, rec.Body.String())
	}

	// Become somebody else.
	rec := do(h, "POST", "/v1/auth/invitations", agent, `{"name":"the-agent-itself","admin":true}`)
	mustRefuseAsOperatorOnly(t, "the agent inviting a principal", rec)

	// Or change the rule.
	rec = do(h, "PUT", "/v1/guard/app.delete", agent, `{"disposition":"allow"}`)
	mustRefuseAsOperatorOnly(t, "the agent relaxing the deny", rec)

	if got := storedPolicy(t, d).Dispositions["app.delete"]; got != "deny" {
		t.Fatalf("stored app.delete = %q, want deny — the agent got out", got)
	}
}
