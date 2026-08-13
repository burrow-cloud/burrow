// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// An agent may not rewrite its own limits (ADR-0099). The engine's end of it: a credential of kind
// `agent` cannot write the guardrail policy and cannot mint an identity, and neither can a caller
// with no kind at all.

// asKind returns the context a request from a caller holding that kind of credential arrives on, the
// way the API layer's authentication builds it. An empty kind returns a bare context, which is what
// the shared install token and an internal reconcile produce: no Caller at all.
func asKind(kind cp.CredentialKind) context.Context {
	if kind == "" {
		return context.Background()
	}
	return cp.ContextWithCaller(context.Background(), cp.Caller{
		PrincipalID: "p-1", PrincipalName: "ada", Kind: kind,
	})
}

// callerOfKind returns the Caller an authenticated request of that kind carries, for the identity
// methods, which are handed one rather than reading the context.
func callerOfKind(kind cp.CredentialKind) cp.Caller {
	return cp.Caller{PrincipalID: "p-1", PrincipalName: "ada", Kind: kind}
}

// mustOperatorOnly asserts err is the refusal this record introduces, and that it is not any of the
// things a caller might mistake it for.
func mustOperatorOnly(t *testing.T, what string, err error) {
	t.Helper()
	o, ok := cp.AsOperatorOnly(err)
	if !ok {
		t.Fatalf("%s = %v, want an OperatorOnlyError", what, err)
	}
	// NOT A GUARDRAIL REFUSAL, which is the distinction the whole record rests on: a guardrail
	// refusal means "policy says this caller may not", and re-issuing it with confirm=true or as a
	// different caller can succeed. This one is not governed by any disposition.
	if _, isGuardrail := cp.AsGuardrail(err); isGuardrail {
		t.Fatalf("%s produced a guardrail refusal; an agent relaxing its own guardrail must not be told to confirm: %v", what, err)
	}
	if !strings.Contains(o.Error(), "--confirm does not satisfy it") {
		t.Errorf("%s refusal = %q, want it to say a confirmation does not satisfy it", what, o.Error())
	}
	if !strings.Contains(o.Error(), "neither is an agent's to change") {
		t.Errorf("%s refusal = %q, want it to name the reason", what, o.Error())
	}
}

// TestAnAgentMayNotSetADispositionAtAnyScope is the first door (ADR-0099 §1). Every shape of the
// write is covered, because the route has several and a rule enforced per shape is a rule the next
// shape forgets: the global policy, one environment, one app, and the compatibility form that
// carries a binding.
func TestAnAgentMayNotSetADispositionAtAnyScope(t *testing.T) {
	e, _, d, _ := newEngine(t, cp.Policy{})
	ctx := asKind(cp.CredentialKindAgent)

	for _, tc := range []struct {
		name  string
		scope cp.GuardrailScope
		binds cp.CredentialKind
	}{
		{name: "global", scope: cp.GuardrailScope{}},
		{name: "one environment", scope: cp.GuardrailScope{Env: "staging"}},
		{name: "one app", scope: cp.GuardrailScope{Env: cp.DefaultEnvironment, Name: "website"}},
		{name: "bound to a kind", scope: cp.GuardrailScope{}, binds: cp.CredentialKindAgent},
		{name: "bound, for one app", scope: cp.GuardrailScope{Env: cp.DefaultEnvironment, Name: "website"}, binds: cp.CredentialKindAgent},
	} {
		err := e.SetGuardrail(ctx, tc.scope, tc.binds, cp.GuardrailAppDelete, cp.DispositionAllow)
		mustOperatorOnly(t, "an agent setting a disposition ("+tc.name+")", err)
	}

	// Nothing was written by any of them.
	if p := storedPolicy(t, d); len(p.Dispositions) != 0 {
		t.Errorf("the refused writes stored %+v, want nothing", p.Dispositions)
	}
}

// TestAnUnknownKindMayNotSetADispositionEither is the case that is easy to get backwards, and the one
// that matters most (ADR-0099 §3).
//
// On an install nobody has signed in to, every request carries the shared install token and NOBODY
// has a kind — the agent included. Reading unknown as a person would leave the policy writable on
// exactly the installs that have only an agent to hold, which is most of them.
func TestAnUnknownKindMayNotSetADispositionEither(t *testing.T) {
	e, _, _, _ := newEngine(t, permissive())

	err := e.SetGuardrail(asKind(""), cp.GuardrailScope{}, "", cp.GuardrailAppDelete, cp.DispositionAllow)
	mustOperatorOnly(t, "a caller with no kind setting a disposition", err)
	if !strings.Contains(err.Error(), "auth login") {
		t.Errorf("refusal = %q, want it to say how an operator on a shared-token install proceeds", err)
	}
}

// TestAPersonAndAMachineMaySetADisposition, which is the other half: the rule must refuse the caller
// it is aimed at without taking the lever away from the operator who holds it. A CI machine keeps it
// for the reason it is exempt from dispositions (ADR-0097 §1) — it runs a script somebody wrote.
func TestAPersonAndAMachineMaySetADisposition(t *testing.T) {
	for _, kind := range []cp.CredentialKind{cp.CredentialKindUser, cp.CredentialKindMachine} {
		e, _, d, _ := newEngine(t, cp.Policy{})
		if err := e.SetGuardrail(asKind(kind), cp.GuardrailScope{}, "", cp.GuardrailAppDelete, cp.DispositionAllow); err != nil {
			t.Fatalf("a %s setting a disposition: %v", kind, err)
		}
		if got := storedPolicy(t, d).Dispositions[cp.GuardrailAppDelete]; got != cp.DispositionAllow {
			t.Errorf("a %s's write stored %q under app.delete, want allow", kind, got)
		}
	}
}

// TestAnAgentMayStillReadThePolicy. Only the WRITES refuse: an agent that can see what binds it can
// explain a refusal to its person, which is the whole reason the listing exists (ADR-0099 §1).
func TestAnAgentMayStillReadThePolicy(t *testing.T) {
	e, _, _, _ := newEngine(t, cp.Policy{}.With(cp.GuardrailAppDelete, cp.DispositionDeny))

	gs, err := e.Guardrails(asKind(cp.CredentialKindAgent), cp.GuardrailScope{})
	if err != nil {
		t.Fatalf("an agent reading the policy: %v", err)
	}
	for _, g := range gs {
		if g.Code == cp.GuardrailAppDelete && g.Disposition == cp.DispositionDeny {
			return
		}
	}
	t.Errorf("the agent's listing = %+v, want it to show the deny that holds it", gs)
}

// TestAnAgentMayNotMintIdentity is the second door (ADR-0099 §2), and it does not need the first.
// The admin bit lives on the principal, so an admin's agent credential is an admin's credential; the
// KIND is what distinguishes the caller here.
func TestAnAgentMayNotMintIdentity(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)
	agent := callerOfKind(cp.CredentialKindAgent)

	_, _, err := e.InvitePrincipal(ctx, agent, "shadow", true)
	mustOperatorOnly(t, "an agent creating an invitation", err)

	_, _, err = e.RedeemInvitation(ctx, cp.Caller{
		PrincipalID: agent.PrincipalID, PrincipalName: agent.PrincipalName,
		Kind: cp.CredentialKindAgent, Enrollment: true,
	})
	mustOperatorOnly(t, "an agent exchanging an invitation", err)

	_, err = e.IssueCredential(ctx, agent, cp.IssueCredentialRequest{
		PrincipalID: agent.PrincipalID, Kind: cp.CredentialKindUser,
	})
	mustOperatorOnly(t, "an agent issuing a credential", err)

	_, err = e.CreatePrincipal(ctx, agent, "shadow", true)
	mustOperatorOnly(t, "an agent recording a principal", err)
}

// TestAnAgentCannotMintItselfAWayOutOfADisposition closes the round trip the record describes, end to
// end and on one engine: an admin signs in, provisions their agent, and denies app.delete. The agent
// then tries both doors — become somebody else, or change the rule — and neither opens.
//
// It is written as one test on purpose. Each half is proved above; what this asserts is that they
// compose, which is the property an attacker needs and the one a reader wants to see.
func TestAnAgentCannotMintItselfAWayOutOfADisposition(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newIdentityEngine(t)

	admin, adminCred, err := e.ClaimFirstPrincipal(ctx, "operator")
	if err != nil {
		t.Fatalf("ClaimFirstPrincipal: %v", err)
	}
	person, err := e.AuthenticateCredential(ctx, adminCred.Token)
	if err != nil {
		t.Fatalf("AuthenticateCredential: %v", err)
	}
	_, agentCred, err := e.IssueAgentCredential(ctx, person)
	if err != nil {
		t.Fatalf("IssueAgentCredential: %v", err)
	}
	agent, err := e.AuthenticateCredential(ctx, agentCred.Token)
	if err != nil {
		t.Fatalf("authenticating the agent's credential: %v", err)
	}
	if agent.Kind != cp.CredentialKindAgent {
		t.Fatalf("the agent's credential resolved to kind %q, want agent", agent.Kind)
	}
	// The agent belongs to an ADMIN, which is the point: the admin bit is on the principal, so it
	// carries into the agent's credential and cannot be what decides this.
	if !admin.Admin {
		t.Fatal("the first principal is not an admin, so this test is not exercising the case it names")
	}

	// The operator sets the deny, from their own credential.
	if err := e.SetGuardrail(cp.ContextWithCaller(ctx, person), cp.GuardrailScope{}, "", cp.GuardrailAppDelete, cp.DispositionDeny); err != nil {
		t.Fatalf("the operator setting the deny: %v", err)
	}

	// Door one: change what you are. An invitation would come back as a `user` credential, for which
	// every disposition resolves to allow.
	_, _, err = e.InvitePrincipal(ctx, agent, "the-agent-itself", true)
	mustOperatorOnly(t, "the agent inviting a principal", err)

	// Door two: change the rule.
	agentCtx := cp.ContextWithCaller(ctx, agent)
	mustOperatorOnly(t, "the agent relaxing the deny",
		e.SetGuardrail(agentCtx, cp.GuardrailScope{}, "", cp.GuardrailAppDelete, cp.DispositionAllow))

	// The deny still holds, and the agent can still read that it does.
	gs, err := e.Guardrails(agentCtx, cp.GuardrailScope{})
	if err != nil {
		t.Fatalf("the agent reading the policy: %v", err)
	}
	for _, g := range gs {
		if g.Code == cp.GuardrailAppDelete && g.Disposition != cp.DispositionDeny {
			t.Fatalf("app.delete resolved to %q for the agent, want deny — the agent got out", g.Disposition)
		}
	}
}

// storedPolicy reads what the engine actually wrote, rather than what a listing resolves to. A
// listing answers for the caller asking, and this has to be a statement about the STORE.
func storedPolicy(t *testing.T, d *fake.Database) cp.Policy {
	t.Helper()
	p, err := d.Policy(context.Background())
	if err != nil {
		t.Fatalf("reading the stored policy: %v", err)
	}
	return p
}
