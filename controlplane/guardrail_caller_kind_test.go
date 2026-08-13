// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"testing"
)

// Who a guardrail holds (ADR-0097): the AGENT, and nobody else. A person and a CI machine are
// allowed everything, always, because their Kubernetes access already decides what they can do — a
// refusal here would be undone by kubectl a second later.
//
// These tests are in-package because dispositionSource is where the decision lives, and reaching it
// through Engine.Deploy would assert it through three layers of something else.

// callerOfKind builds the context a request from a caller of that kind arrives on, the way the API
// layer's authentication does. An empty kind returns a bare context, which is the shared install
// token and the internal reconcile: no Caller at all.
func callerOfKind(kind CredentialKind) context.Context {
	if kind == "" {
		return context.Background()
	}
	return ContextWithCaller(context.Background(), Caller{PrincipalID: "p-1", PrincipalName: "ada", Kind: kind})
}

// denyEverywhere is a policy that refuses one code at every tier, so a caller who IS held has no way
// through and a caller who is not is visibly unaffected by all of it.
func denyEverywhere(code GuardrailCode) Policy {
	return Policy{Dispositions: map[GuardrailCode]Disposition{
		code:                                   DispositionDeny,
		envPolicyKey("prod", code):             DispositionDeny,
		namePolicyKey("prod", "website", code): DispositionDeny,
	}}
}

// TestAPersonIsNotHeldByAnyDisposition is the record's whole point. Every tier says deny; the person
// is allowed anyway, because refusing them buys nothing their cluster access has not already decided.
func TestAPersonIsNotHeldByAnyDisposition(t *testing.T) {
	p := denyEverywhere(GuardrailAppDeploy)
	scope := GuardrailScope{Env: "prod", Name: "website"}

	if got := p.disposition(callerOfKind(CredentialKindUser), scope, GuardrailAppDeploy); got != DispositionAllow {
		t.Errorf("a person's %s = %q, want allow — a guardrail holds the agent, not the person", GuardrailAppDeploy, got)
	}
}

// TestACIMachineIsNotHeldEither. A machine runs a script somebody wrote and reviewed; the risk a
// guardrail bounds is a model choosing an action nobody asked for. An operator who wants CI held to
// the same line issues it an agent credential.
func TestACIMachineIsNotHeldEither(t *testing.T) {
	p := denyEverywhere(GuardrailAppDeploy)

	if got := p.disposition(callerOfKind(CredentialKindMachine), GuardrailScope{Env: "prod"}, GuardrailAppDeploy); got != DispositionAllow {
		t.Errorf("a machine's %s = %q, want allow", GuardrailAppDeploy, got)
	}
}

// TestTheAgentIsHeld, which is the other half: exempting people must not exempt the caller the whole
// mechanism exists for.
func TestTheAgentIsHeld(t *testing.T) {
	p := denyEverywhere(GuardrailAppDeploy)

	if got := p.disposition(callerOfKind(CredentialKindAgent), GuardrailScope{Env: "prod", Name: "website"}, GuardrailAppDeploy); got != DispositionDeny {
		t.Errorf("the agent's %s = %q, want deny", GuardrailAppDeploy, got)
	}
}

// TestAnUnknownKindIsStillHeld is the load-bearing case, and the one that is easy to get backwards.
//
// On an install without per-caller credentials NOBODY has a kind — including the agent, which reaches
// burrowd with the shared install token. Reading "unknown" as a person would switch every guardrail
// off for exactly the installs that have only an agent to hold, which is most of them.
func TestAnUnknownKindIsStillHeld(t *testing.T) {
	p := denyEverywhere(GuardrailAppDeploy)

	if got := p.disposition(callerOfKind(""), GuardrailScope{Env: "prod"}, GuardrailAppDeploy); got != DispositionDeny {
		t.Errorf("an unknown caller's %s = %q, want deny — a shared-token install must keep its guardrails", GuardrailAppDeploy, got)
	}
}

// TestTheTargetTiersStillNarrow. Removing the caller axis must not disturb the target ladder: name
// beats environment beats global, and the built-in default is the floor.
func TestTheTargetTiersStillNarrow(t *testing.T) {
	code := GuardrailAppDeploy
	p := Policy{Dispositions: map[GuardrailCode]Disposition{
		code:                          DispositionDeny,
		envPolicyKey("staging", code): DispositionConfirm,
		namePolicyKey("staging", "website", code): DispositionAllow,
	}}
	agent := callerOfKind(CredentialKindAgent)

	for _, tc := range []struct {
		name  string
		scope GuardrailScope
		want  Disposition
	}{
		{"the named app wins", GuardrailScope{Env: "staging", Name: "website"}, DispositionAllow},
		{"another app in the environment", GuardrailScope{Env: "staging", Name: "api"}, DispositionConfirm},
		{"another environment falls to global", GuardrailScope{Env: "qa"}, DispositionDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.disposition(agent, tc.scope, code); got != tc.want {
				t.Errorf("%s = %q, want %q", code, got, tc.want)
			}
		})
	}
}

// TestAnUndispositionedCodeDeniesTheAgentAndAllowsThePerson keeps the fail-safe where it matters. A
// guardrail code nobody has considered is not handed to a model — and it costs a person nothing,
// because a person was never going to be refused.
func TestAnUndispositionedCodeDeniesTheAgentAndAllowsThePerson(t *testing.T) {
	empty := Policy{}

	if got := empty.disposition(callerOfKind(CredentialKindAgent), GuardrailScope{}, GuardrailAppDeploy); got != DispositionDeny {
		t.Errorf("the agent's undispositioned code = %q, want deny", got)
	}
	if got := empty.disposition(callerOfKind(CredentialKindUser), GuardrailScope{}, GuardrailAppDeploy); got != DispositionAllow {
		t.Errorf("a person's undispositioned code = %q, want allow", got)
	}
}
