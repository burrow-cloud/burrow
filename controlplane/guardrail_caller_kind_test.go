// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"strings"
	"testing"
)

// The caller axis (ADR-0094): a disposition can bind one KIND of credential and leave every other
// caller reading the disposition underneath it.
//
// These tests are in-package because the ordering they pin is not observable from outside without
// building a whole install: dispositionSource is where the record's seven lookups live, and a test
// that reached them through Engine.Deploy would be asserting the ladder through three layers of
// something else.

// callerOfKind builds the context a request from a caller of that kind arrives on, the way the API
// layer's authentication does. An empty kind returns a bare context, which is the shared install
// token and the internal reconcile: no Caller at all.
func callerOfKind(kind CredentialKind) context.Context {
	if kind == "" {
		return context.Background()
	}
	return ContextWithCaller(context.Background(), Caller{PrincipalID: "p-1", PrincipalName: "ada", Kind: kind})
}

// ladder is the policy with EVERY step of ADR-0094 §3's lookup order populated, for one code, in one
// environment, for one app. Peeling keys off the front of it in order is what proves the order.
var ladder = []GuardrailCode{
	"agent:staging.website.app.deploy", // 1. K:<env>.<name>.<code>
	"staging.website.app.deploy",       // 2.   <env>.<name>.<code>
	"agent:staging.app.deploy",         // 3. K:<env>.<code>
	"staging.app.deploy",               // 4.   <env>.<code>
	"agent:app.deploy",                 // 5. K:<code>
	"app.deploy",                       // 6.   <code>
	// 7. the built-in default — deny — is not a key and cannot be peeled off, which is the property
	// that makes the fall-through terminate closed.
}

// answeredBy names the (tier, binding) pair each rung of the ladder produces, so a test can say which
// KEY answered rather than only what it answered with. Every pair is distinct, including the default
// at the bottom, so one assertion identifies one rung unambiguously.
type answer struct {
	source string
	binds  CredentialKind
}

var ladderAnswers = []answer{
	{sourceName, CredentialKindAgent},
	{sourceName, ""},
	{sourceEnv, CredentialKindAgent},
	{sourceEnv, ""},
	{sourceGlobal, CredentialKindAgent},
	{sourceGlobal, ""},
}

// policyFrom builds a policy holding the named keys, all set to allow so the built-in deny at the
// bottom of the ladder is distinguishable from every rung above it.
func policyFrom(keys []GuardrailCode) Policy {
	p := Policy{}
	for _, k := range keys {
		p = p.With(k, DispositionAllow)
	}
	return p
}

// TestTheCallerKindNarrowsWithinEachTier walks ADR-0094 §3's seven lookups from the top, removing one
// key at a time, and asserts that the next one down answers. This is the heart of the record: TARGET
// NARROWS BEFORE CALLER, so within each tier the kind-bound key answers first and the unbound key is
// that tier's answer for everyone else, and the ladder never jumps a rung.
func TestTheCallerKindNarrowsWithinEachTier(t *testing.T) {
	ctx := callerOfKind(CredentialKindAgent)
	scope := GuardrailScope{Env: "staging", Name: "website"}
	for i := range ladder {
		p := policyFrom(ladder[i:])
		d, src, binds := p.dispositionSource(ctx, scope, GuardrailAppDeploy)
		if d != DispositionAllow {
			t.Errorf("step %d (%s): disposition = %q, want allow", i+1, ladder[i], d)
		}
		if want := ladderAnswers[i]; src != want.source || binds != want.binds {
			t.Errorf("step %d (%s): answered by (%q, %q), want (%q, %q)", i+1, ladder[i], src, binds, want.source, want.binds)
		}
	}
	// Nothing left: step 7, the built-in default, which binds every kind.
	d, src, binds := policyFrom(nil).dispositionSource(ctx, scope, GuardrailAppDeploy)
	if d != DispositionDeny || src != sourceDefault || binds != "" {
		t.Errorf("step 7 = (%q, %q, %q), want (deny, default, unbound)", d, src, binds)
	}
}

// TestAnUnboundCallerReadsOnlyUnboundKeys is ADR-0094 §4, and it is the compatibility guarantee: an
// install nobody has signed in to behaves exactly as it does today.
//
// A caller of an unknown kind lands here too, and deliberately. Treating an unknown kind as `agent`
// is the tempting safe-by-default reading and it is backwards: on a shared-token install EVERY caller
// is unknown, so the operator would be treated as their own agent on every request — reinstating,
// for every install that has not signed in, the defect the record exists to remove.
func TestAnUnboundCallerReadsOnlyUnboundKeys(t *testing.T) {
	scope := GuardrailScope{Env: "staging", Name: "website"}
	// The rungs an unbound caller can reach, and the rung each one answers from.
	unbound := []struct {
		from int    // index into ladder: the keys still present
		want answer // the rung that answers
	}{
		{0, answer{sourceName, ""}},   // 1 is bound and skipped; 2 answers
		{2, answer{sourceEnv, ""}},    // 3 is bound and skipped; 4 answers
		{4, answer{sourceGlobal, ""}}, // 5 is bound and skipped; 6 answers
	}
	for _, kind := range []CredentialKind{"", "robot", "USER"} {
		ctx := callerOfKind(kind)
		for _, c := range unbound {
			p := policyFrom(ladder[c.from:])
			d, src, binds := p.dispositionSource(ctx, scope, GuardrailAppDeploy)
			if d != DispositionAllow || src != c.want.source || binds != "" {
				t.Errorf("kind %q from step %d = (%q, %q, %q), want (allow, %q, unbound)", kind, c.from+1, d, src, binds, c.want.source)
			}
		}
		// Only bound keys exist, so nothing matches and the default denies.
		only := policyFrom([]GuardrailCode{"agent:staging.website.app.deploy", "agent:app.deploy"})
		if d, src, _ := only.dispositionSource(ctx, scope, GuardrailAppDeploy); d != DispositionDeny || src != sourceDefault {
			t.Errorf("kind %q against a policy of bound keys = (%q, %q), want (deny, default)", kind, d, src)
		}
	}
}

// TestTheTargetNarrowsBeforeTheCaller pins the ordering the record calls out as the one that must not
// be inverted (ADR-0094 §3). A cluster-wide binding on the agent does NOT beat a disposition somebody
// set deliberately for one app: ADR-0085 §1 established that the narrowest TARGET wins, and reversing
// it for the caller axis would let a wide setting silently override a narrow one.
func TestTheTargetNarrowsBeforeTheCaller(t *testing.T) {
	p := Policy{}.
		With("agent:app.deploy", DispositionDeny).         // agents, everywhere
		With("prod.website.app.deploy", DispositionAllow). // one app, deliberately, for everyone
		With(GuardrailAppDeploy, DispositionConfirm)       // everyone, everywhere

	ctx := callerOfKind(CredentialKindAgent)
	d, src, binds := p.dispositionSource(ctx, GuardrailScope{Env: "prod", Name: "website"}, GuardrailAppDeploy)
	if d != DispositionAllow || src != sourceName || binds != "" {
		t.Errorf("the agent on the named app = (%q, %q, %q), want (allow, name, unbound): a cluster-wide binding must not beat a per-app disposition", d, src, binds)
	}
	// With no per-app row the cluster-wide binding is what the agent reaches, and the human still
	// reads the global confirm underneath it.
	q := Policy{}.With("agent:app.deploy", DispositionDeny).With(GuardrailAppDeploy, DispositionConfirm)
	if d, _, binds := q.dispositionSource(ctx, GuardrailScope{Env: "prod", Name: "website"}, GuardrailAppDeploy); d != DispositionDeny || binds != CredentialKindAgent {
		t.Errorf("the agent with no per-app row = (%q, %q), want (deny, agent)", d, binds)
	}
	if d, _, binds := q.dispositionSource(callerOfKind(CredentialKindUser), GuardrailScope{Env: "prod", Name: "website"}, GuardrailAppDeploy); d != DispositionConfirm || binds != "" {
		t.Errorf("the operator = (%q, %q), want (confirm, unbound)", d, binds)
	}
}

// TestANonMatchingBindingFallsThroughRatherThanAllowing is the property that keeps a narrow binding
// from punching a hole through a wider deny (ADR-0094 §3). A key that does not bind the caller's kind
// is NOT an answer — resolution continues to the next lookup — so setting `agent:` on one app tightens
// that app for agents and leaves every other tier reading exactly as it did.
//
// Falling through to `allow` instead would mean that an `agent:` binding on one app handed the human
// an allow for it regardless of what the environment or the global policy said.
func TestANonMatchingBindingFallsThroughRatherThanAllowing(t *testing.T) {
	p := Policy{}.
		With("agent:prod.website.app.deploy", DispositionAllow).
		With(GuardrailAppDeploy, DispositionDeny)

	scope := GuardrailScope{Env: "prod", Name: "website"}
	d, src, binds := p.dispositionSource(callerOfKind(CredentialKindUser), scope, GuardrailAppDeploy)
	if d != DispositionDeny || src != sourceGlobal || binds != "" {
		t.Errorf("the operator = (%q, %q, %q), want (deny, global, unbound): a binding for another kind is not an answer", d, src, binds)
	}
	if d, src, binds := p.dispositionSource(callerOfKind(CredentialKindAgent), scope, GuardrailAppDeploy); d != DispositionAllow || src != sourceName || binds != CredentialKindAgent {
		t.Errorf("the agent = (%q, %q, %q), want (allow, name, agent)", d, src, binds)
	}
}

// TestTheAxisRelaxesAsWellAsItTightens is ADR-0094 §3's closing paragraph and ADR-0065 §3's point
// that a deny default is a floor rather than a fixed setting: a global deny with a `user:` allow
// under it in one environment is a human-only relaxation, and the agent keeps reading the deny.
func TestTheAxisRelaxesAsWellAsItTightens(t *testing.T) {
	p := Policy{}.
		With(GuardrailAppDelete, DispositionDeny).
		With("user:dev.app.delete", DispositionAllow)

	scope := GuardrailScope{Env: "dev"}
	if d, src, binds := p.dispositionSource(callerOfKind(CredentialKindUser), scope, GuardrailAppDelete); d != DispositionAllow || src != sourceEnv || binds != CredentialKindUser {
		t.Errorf("the operator in dev = (%q, %q, %q), want (allow, env, user)", d, src, binds)
	}
	if d, _, binds := p.dispositionSource(callerOfKind(CredentialKindAgent), scope, GuardrailAppDelete); d != DispositionDeny || binds != "" {
		t.Errorf("the agent in dev = (%q, %q), want (deny, unbound)", d, binds)
	}
	// And nothing about production moved.
	if d, _, _ := p.dispositionSource(callerOfKind(CredentialKindUser), GuardrailScope{Env: DefaultEnvironment}, GuardrailAppDelete); d != DispositionDeny {
		t.Errorf("the operator in prod = %q, want deny: a relaxation in one environment must not reach another", d)
	}
}

// TestBoundPolicyKeyShape pins the stored key, which is a persistence format: it is what is written
// into guardrail_policy, and a change to it silently orphans every row an operator has set.
//
// The colon is unambiguous by construction (ADR-0094 §2). Environment, application and add-on
// instance names are DNS-1123 labels and guardrail codes are dotted lowercase identifiers, so no
// colon can appear in any other segment — which is why no key needs escaping in either direction.
func TestBoundPolicyKeyShape(t *testing.T) {
	cases := []struct {
		kind CredentialKind
		key  GuardrailCode
		want GuardrailCode
	}{
		{CredentialKindAgent, GuardrailAppDelete, "agent:app.delete"},
		{CredentialKindAgent, namePolicyKey("prod", "burrowd-cloud", GuardrailAppDeploy), "agent:prod.burrowd-cloud.app.deploy"},
		{CredentialKindUser, envPolicyKey("staging", GuardrailAppRun), "user:staging.app.run"},
		{CredentialKindMachine, GuardrailAddonSQL, "machine:addon.sql"},
	}
	for _, c := range cases {
		if got := boundPolicyKey(c.kind, c.key); got != c.want {
			t.Errorf("boundPolicyKey(%q, %q) = %q, want %q", c.kind, c.key, got, c.want)
		}
	}
	// One colon, and it is the only one: nothing on the right of it can contribute another, so a
	// reader (or a future migration) can split on the first colon and be right every time.
	for _, c := range cases {
		if n := strings.Count(string(c.want), ":"); n != 1 {
			t.Errorf("%q holds %d colons, want exactly 1", c.want, n)
		}
	}
}

// TestABoundRefusalSaysWhoIsNotBound is ADR-0094 §6. The reader of a refusal is usually an agent
// relaying to a human, and the relayable next step matters: "ask someone to run it" leaves the
// guardrail in place, where "ask someone to relax the policy" turns the protection off for everyone
// while the work happens.
func TestABoundRefusalSaysWhoIsNotBound(t *testing.T) {
	p := Policy{}.With("agent:prod.burrowd-cloud.app.deploy", DispositionDeny)
	err := p.evaluateGuardrail(callerOfKind(CredentialKindAgent), GuardrailScope{Env: "prod", Name: "burrowd-cloud"},
		"deploy", GuardrailAppDeploy, true, "deploying `burrowd-cloud`")
	g, ok := AsGuardrail(err)
	if !ok {
		t.Fatalf("evaluateGuardrail = %v, want a GuardrailError", err)
	}
	for _, want := range []string{
		`for the app "burrowd-cloud"`,
		`this disposition binds "agent" credentials`,
		"an operator can run it with their own",
	} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("the refusal does not say %q: %s", want, g.Message)
		}
	}
	// The relax hint is REPLACED rather than appended: printing both would bury the cheaper answer
	// under the one that costs the protection.
	if strings.Contains(g.Message, "guard set") {
		t.Errorf("a bound refusal still offers the relax lever, burying the cheaper next step: %s", g.Message)
	}
	// An unbound refusal is unchanged: it has no kind to name and keeps the lever it always had.
	q := Policy{}.With("prod.burrowd-cloud.app.deploy", DispositionDeny)
	unbound, _ := AsGuardrail(q.evaluateGuardrail(callerOfKind(CredentialKindAgent), GuardrailScope{Env: "prod", Name: "burrowd-cloud"},
		"deploy", GuardrailAppDeploy, true, "deploying `burrowd-cloud`"))
	if strings.Contains(unbound.Message, "binds") {
		t.Errorf("an unbound refusal claims a binding: %s", unbound.Message)
	}
	if !strings.Contains(unbound.Message, "guard set") {
		t.Errorf("an unbound refusal lost its relax lever: %s", unbound.Message)
	}
}

// TestTheListingAnswersForTheCallerWhoAsked is ADR-0094 §6's read side. ADR-0065 §7 requires the
// agent to be able to see what it cannot do, and a listing that showed the human's answer to the
// agent would be worse than no listing: the agent would plan an operation the policy has already
// refused.
func TestTheListingAnswersForTheCallerWhoAsked(t *testing.T) {
	p := Policy{}.
		With(GuardrailAppDelete, DispositionAllow).
		With("agent:app.delete", DispositionDeny)

	find := func(gs []GuardrailInfo, code GuardrailCode) GuardrailInfo {
		t.Helper()
		for _, g := range gs {
			if g.Code == code {
				return g
			}
		}
		t.Fatalf("%s missing from the listing", code)
		return GuardrailInfo{}
	}
	agent := find(p.Guardrails(callerOfKind(CredentialKindAgent)), GuardrailAppDelete)
	if agent.Disposition != DispositionDeny || agent.Binds != CredentialKindAgent {
		t.Errorf("the agent's listing = (%q, binds %q), want (deny, agent)", agent.Disposition, agent.Binds)
	}
	operator := find(p.Guardrails(callerOfKind(CredentialKindUser)), GuardrailAppDelete)
	if operator.Disposition != DispositionAllow || operator.Binds != "" {
		t.Errorf("the operator's listing = (%q, binds %q), want (allow, unbound)", operator.Disposition, operator.Binds)
	}
	// A binding is reported in the GLOBAL listing too, where Source deliberately is not: a binding is
	// a fact about the disposition itself rather than a consequence of how narrowly it was asked for.
	if agent.Source != "" {
		t.Errorf("the global listing reported a Source (%q); only the binding belongs there", agent.Source)
	}
}
