// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// `guard set --binds` and what a bound disposition does once it is written (ADR-0094), driven through
// the engine rather than through the policy type. A disposition only the CLI honoured would not be a
// guardrail (ADR-0020): the control plane is where an operation is gated, so that is where a binding
// has to be resolved.

// signedIn records a principal on the install, which is what makes it one that ISSUES per-caller
// credentials and therefore one a binding can bind. It is the fake's equivalent of somebody having
// run `burrow auth login`.
func signedIn(t *testing.T, d *fake.Database) {
	t.Helper()
	if err := d.CreatePrincipal(context.Background(), cp.Principal{ID: "p-1", Name: "ada", Admin: true, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
}

// asKind is the context a request from a caller holding that kind of credential arrives on, the way
// the API layer's authentication builds it.
func asKind(kind cp.CredentialKind) context.Context {
	return cp.ContextWithCaller(context.Background(), cp.Caller{PrincipalID: "p-1", PrincipalName: "ada", Kind: kind})
}

// TestABoundDenyLeavesTheOtherKindsAlone is the case the record was written for (ADR-0094's Context):
// the managed control plane runs as an ordinary Burrow app, its deploy must not be reachable by an
// agent, and the operator repairing it must be unaffected. Before the caller axis those two could not
// both be true, so the protection was not applied at all.
func TestABoundDenyLeavesTheOtherKindsAlone(t *testing.T) {
	ctx := context.Background()
	e, _, d := newRoutingEngine(t, "burrow-apps")
	signedIn(t, d)

	scope := cp.GuardrailScope{Env: cp.DefaultEnvironment, Name: "burrowd-cloud"}
	if err := e.SetGuardrail(ctx, scope, cp.CredentialKindAgent, cp.GuardrailAppDeploy, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(--binds agent): %v", err)
	}

	// The agent is refused, and no confirmation satisfies a deny.
	_, err := e.Deploy(asKind(cp.CredentialKindAgent), cp.DeployRequest{App: "burrowd-cloud", Image: "reg.example.com/cp:1", Replicas: 1, Confirm: true})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("the agent's deploy = %v, want a GuardrailError", err)
	}
	if g.NeedsConfirmation {
		t.Error("a deny was reported as a hold, so the agent would retry with confirm and be refused again")
	}
	if !strings.Contains(g.Message, `binds "agent" credentials`) || !strings.Contains(g.Message, "an operator can run it with their own") {
		t.Errorf("the refusal does not tell the agent what to relay: %s", g.Message)
	}

	// The same person's own credential deploys the same app, which is the whole of the record.
	if _, err := e.Deploy(asKind(cp.CredentialKindUser), cp.DeployRequest{App: "burrowd-cloud", Image: "reg.example.com/cp:1", Replicas: 1}); err != nil {
		t.Errorf("the operator's deploy = %v, want it to proceed: an agent-bound deny must not hold against the human repairing the platform", err)
	}
}

// TestAnAdminsAgentIsStillBound is ADR-0094 §5, and it is what #553's limit needed resolving. A
// sign-in issues two credentials to ONE principal, so anything keyed on the principal cannot separate
// a person from their own agent. Keying on the KIND does: same principal, same name, two answers, and
// the difference is which token was presented rather than who presented it.
func TestAnAdminsAgentIsStillBound(t *testing.T) {
	ctx := context.Background()
	e, _, d := newRoutingEngine(t, "burrow-apps")
	signedIn(t, d)

	if err := e.SetGuardrail(ctx, cp.GuardrailScope{}, cp.CredentialKindAgent, cp.GuardrailAppDelete, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(--binds agent, app.delete, deny): %v", err)
	}
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{}, "", cp.GuardrailAppDelete, cp.DispositionAllow); err != nil {
		t.Fatalf("SetGuardrail(app.delete, allow): %v", err)
	}

	// One principal, admin, two credentials. The admin bit is deliberately absent from Caller, so
	// nothing here can exempt them: the answer turns on the kind alone.
	admin := cp.Caller{PrincipalID: "p-1", PrincipalName: "ada", Kind: cp.CredentialKindAgent, CredentialID: "c-agent"}
	gs, err := e.Guardrails(cp.ContextWithCaller(ctx, admin), cp.GuardrailScope{})
	if err != nil {
		t.Fatalf("Guardrails: %v", err)
	}
	if got := dispositionOf(t, gs, cp.GuardrailAppDelete); got != cp.DispositionDeny {
		t.Errorf("the admin's AGENT reads app.delete = %q, want deny: an admin exemption would exempt the agent most worth binding", got)
	}
	admin.Kind, admin.CredentialID = cp.CredentialKindUser, "c-user"
	gs, err = e.Guardrails(cp.ContextWithCaller(ctx, admin), cp.GuardrailScope{})
	if err != nil {
		t.Fatalf("Guardrails: %v", err)
	}
	if got := dispositionOf(t, gs, cp.GuardrailAppDelete); got != cp.DispositionAllow {
		t.Errorf("the same admin's OWN credential reads app.delete = %q, want allow", got)
	}
}

// TestBindingIsRefusedOnAnInstallWithNoCredentials is ADR-0094 §4's refusal. On an install nobody has
// signed in to, every request carries the shared token and no request has a kind, so a kind-bound
// disposition would bind NOTHING. A protection that silently protects nothing is the worst available
// outcome, so it is refused where it is asked for rather than discovered during the incident the deny
// existed to prevent.
func TestBindingIsRefusedOnAnInstallWithNoCredentials(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")

	err := e.SetGuardrail(ctx, cp.GuardrailScope{}, cp.CredentialKindAgent, cp.GuardrailAppDelete, cp.DispositionDeny)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("SetGuardrail(--binds agent) on an unsigned-in install = %v, want ErrInvalid", err)
	}
	for _, want := range []string{"no per-caller credentials", "would bind nobody", "burrow auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	// Nothing was written. The unbound disposition remains available and remains the honest answer
	// for a shared-token install: blunt, and it holds.
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{}, "", cp.GuardrailAppDelete, cp.DispositionDeny); err != nil {
		t.Fatalf("the unbound set was refused too: %v", err)
	}
}

// TestAnUnknownKindIsRefusedAtSetTime keeps the closed set closed. The three kinds are recorded at
// issuance (ADR-0084 §3) and nothing else can ever be on the other side of a credential row, so a
// fourth value would be a key nothing will ever match — the same "looks configured and is not" trap
// the refusal above exists for, one axis over.
func TestAnUnknownKindIsRefusedAtSetTime(t *testing.T) {
	ctx := context.Background()
	e, _, d := newRoutingEngine(t, "burrow-apps")
	signedIn(t, d)

	for _, kind := range []cp.CredentialKind{"robot", "USER", "admin"} {
		err := e.SetGuardrail(ctx, cp.GuardrailScope{}, kind, cp.GuardrailAppDelete, cp.DispositionDeny)
		if !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("SetGuardrail(--binds %q) = %v, want ErrInvalid", kind, err)
		}
	}
}

// TestABindingComposesWithEveryTargetTier: the caller axis is a fourth axis rather than a replacement
// for the other three, so a binding can be written at the global, environment and name tiers alike,
// and each lands under the key ADR-0094 §2 specifies.
func TestABindingComposesWithEveryTargetTier(t *testing.T) {
	ctx := context.Background()
	e, _, d := newRoutingEngine(t, "burrow-apps")
	signedIn(t, d)
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	writes := []struct {
		scope cp.GuardrailScope
		code  cp.GuardrailCode
		want  cp.GuardrailCode
	}{
		{cp.GuardrailScope{}, cp.GuardrailAppDelete, "agent:app.delete"},
		{cp.GuardrailScope{Env: "staging"}, cp.GuardrailAppDelete, "agent:staging.app.delete"},
		{cp.GuardrailScope{Env: "staging", Name: "website"}, cp.GuardrailAppRun, "agent:staging.website.app.run"},
		{cp.GuardrailScope{Env: cp.DefaultEnvironment, Name: "burrowd-cloud"}, cp.GuardrailAppDeploy, "agent:prod.burrowd-cloud.app.deploy"},
	}
	for _, w := range writes {
		if err := e.SetGuardrail(ctx, w.scope, cp.CredentialKindAgent, w.code, cp.DispositionDeny); err != nil {
			t.Fatalf("SetGuardrail(%+v, %s): %v", w.scope, w.code, err)
		}
	}
	p, err := d.Policy(ctx)
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	for _, w := range writes {
		if got := p.Dispositions[w.want]; got != cp.DispositionDeny {
			t.Errorf("no deny stored under %q (got %q); the stored key is a persistence format and a change to it orphans every row an operator set", w.want, got)
		}
	}
}

// TestAnInstallThatNeverBindsIsUnchanged is the mitigation ADR-0094's Consequences lean on: the
// default is unchanged, so a disposition set without --binds behaves as every disposition behaves
// today, for callers who have a kind and callers who do not.
func TestAnInstallThatNeverBindsIsUnchanged(t *testing.T) {
	ctx := context.Background()
	e, _, d := newRoutingEngine(t, "burrow-apps")
	signedIn(t, d)

	if err := e.SetGuardrail(ctx, cp.GuardrailScope{}, "", cp.GuardrailAppDeploy, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail: %v", err)
	}
	req := cp.DeployRequest{App: "website", Image: "reg.example.com/website:1", Replicas: 1, Confirm: true}
	for _, ctx := range []context.Context{context.Background(), asKind(cp.CredentialKindUser), asKind(cp.CredentialKindAgent), asKind("robot")} {
		if _, err := e.Deploy(ctx, req); err == nil {
			t.Error("an unbound deny let a deploy through; an unbound disposition binds everyone")
		} else if g, ok := cp.AsGuardrail(err); !ok {
			t.Errorf("deploy = %v, want a GuardrailError", err)
		} else if strings.Contains(g.Message, "binds") {
			t.Errorf("an unbound refusal claims a binding: %s", g.Message)
		}
	}
}

func dispositionOf(t *testing.T, gs []cp.GuardrailInfo, code cp.GuardrailCode) cp.Disposition {
	t.Helper()
	for _, g := range gs {
		if g.Code == code {
			return g.Disposition
		}
	}
	t.Fatalf("%s missing from the listing", code)
	return ""
}
