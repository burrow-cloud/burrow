// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestDeployDeniedForOneAppOnly is ADR-0085's motivating case, driven through the engine rather than
// through a message: two apps share an environment, one must not be deployable by the agent, and the
// other must stay operable because operating it is the point. Before this, the only way to say the
// first was to put that app in an environment of its own.
//
// It goes through Engine.Deploy deliberately. A disposition only the CLI honoured would not be a
// guardrail (ADR-0020): the control plane is where an operation is gated, so that is where the
// per-app disposition has to be resolved.
func TestDeployDeniedForOneAppOnly(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")

	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: cp.DefaultEnvironment, Name: "control-plane"}, cp.GuardrailAppDeploy, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(prod, control-plane, app.deploy, deny): %v", err)
	}

	// The neighbour in the same environment deploys exactly as before.
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "website", Image: "registry.example.com/website:1", Replicas: 1}); err != nil {
		t.Errorf("Deploy(website) = %v, want it to proceed: a deny for one app must not reach its neighbours", err)
	}

	// The named app is refused, and no confirmation satisfies a deny.
	_, err := e.Deploy(ctx, cp.DeployRequest{App: "control-plane", Image: "registry.example.com/control-plane:1", Replicas: 1, Confirm: true})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("Deploy(control-plane) = %v, want a GuardrailError", err)
	}
	if g.Code != cp.GuardrailAppDeploy || g.NeedsConfirmation {
		t.Errorf("control-plane deploy guardrail = %+v, want a plain deny on app.deploy", g)
	}
	if !strings.Contains(g.Message, `for the app "control-plane"`) {
		t.Errorf("refusal %q does not name the app it was set for; on an install where every other app deploys fine, a bare deny reads like a bug", g.Message)
	}
}

// TestNameScopedDispositionSurvivesTheEnvironmentTier confirms the tiers compose in the engine and
// not only in the policy type: an environment-wide relaxation does not lift a deny set for one app
// inside it, which is the direction that matters — the narrow rule is the protective one.
func TestNameScopedDispositionSurvivesTheEnvironmentTier(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: "staging"}, cp.GuardrailAppDelete, cp.DispositionAllow); err != nil {
		t.Fatalf("SetGuardrail(staging, app.delete, allow): %v", err)
	}
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: "staging", Name: "control-plane"}, cp.GuardrailAppDelete, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(staging, control-plane, app.delete, deny): %v", err)
	}
	for _, app := range []string{"website", "control-plane"} {
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: app, Env: "staging", Image: "registry.example.com/" + app + ":1", Replicas: 1}); err != nil {
			t.Fatalf("Deploy(%s): %v", app, err)
		}
	}

	if err := e.DeleteApp(ctx, "website", "staging", false); err != nil {
		t.Errorf("DeleteApp(website) = %v, want it to proceed under staging's allow", err)
	}
	err := e.DeleteApp(ctx, "control-plane", "staging", true)
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("DeleteApp(control-plane) = %v, want a GuardrailError", err)
	}
	if g.NeedsConfirmation {
		t.Errorf("control-plane delete = %+v, want a plain deny the environment's allow does not lift", g)
	}
}

// TestAddonGuardrailScopesToOneInstance confirms the same tier works for the add-on codes, where the
// name is the INSTANCE. It is what makes an addon.* guardrail settable per environment at all: those
// codes are not env-scopable, and an instance name carries its own environment (ADR-0067 §1), so
// naming the instance says which environment is meant without inventing a tier for it.
func TestAddonGuardrailScopesToOneInstance(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	instance, err := cp.AddonInstanceName(cp.AddonLogs, "staging")
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: "staging", Name: instance}, cp.GuardrailAddonInstall, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(staging, %s, addon.install, deny): %v", instance, err)
	}

	// The same add-on in the default environment is a different instance and is untouched.
	if _, err := e.InstallAddon(ctx, cp.AddonLogs, cp.DefaultEnvironment, cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Errorf("InstallAddon(prod) = %v, want it to proceed: the deny names one instance", err)
	}

	_, err = e.InstallAddon(ctx, cp.AddonLogs, "staging", cp.InstallAddonOptions{Confirm: true})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("InstallAddon(staging) = %v, want a GuardrailError", err)
	}
	if !strings.Contains(g.Message, `for the add-on instance "`+instance+`"`) {
		t.Errorf("refusal %q does not name the instance it was set for", g.Message)
	}
}

// TestSetGuardrailNameValidation covers every way naming a thing is refused, because each refusal is
// carrying an idea rather than rejecting an argument (ADR-0085 §1, §3).
func TestSetGuardrailNameValidation(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	// A name with no environment is refused rather than quietly widened to the environment tier:
	// `website.app.run` is the key an environment called `website` would produce, and nothing in the
	// lookup could tell the two apart.
	err := e.SetGuardrail(ctx, cp.GuardrailScope{Name: "website"}, cp.GuardrailAppRun, cp.DispositionDeny)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetGuardrail(name without env) = %v, want ErrInvalid", err)
	}
	if err != nil && !strings.Contains(err.Error(), "--env") {
		t.Errorf("refusal %q should name the missing flag", err)
	}

	// A guardrail whose effect is wider than one thing says how far it does reach, rather than
	// reporting an unsupported flag.
	err = e.SetGuardrail(ctx, cp.GuardrailScope{Env: "staging", Name: "website"}, cp.GuardrailDNSWrite, cp.DispositionAllow)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetGuardrail(dns.write, name) = %v, want ErrInvalid", err)
	}
	if err != nil && !strings.Contains(err.Error(), "outside the cluster") {
		t.Errorf("refusal %q should say why dns.write reaches further than one thing", err)
	}

	// A name that is not a DNS label would put a dot in the composed key, which is the one thing
	// that could make a key ambiguous.
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: "staging", Name: "web.site"}, cp.GuardrailAppRun, cp.DispositionDeny); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("SetGuardrail(dotted name) = %v, want ErrInvalid", err)
	}

	// The environment still has to exist, so a typo is caught the way it is without a name.
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: "ghost", Name: "website"}, cp.GuardrailAppRun, cp.DispositionDeny); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("SetGuardrail(unknown env, name) = %v, want ErrNotFound", err)
	}

	// Listing follows the same rule, so a name means the same thing on both verbs.
	if _, err := e.Guardrails(ctx, cp.GuardrailScope{Name: "website"}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("Guardrails(name without env) = %v, want ErrInvalid", err)
	}
}

// TestGuardrailsForNameReportsTheTierThatAnswered is ADR-0085 §4: a listing for one app says, per
// guardrail, whether the disposition was set for that app, for its environment, globally, or is the
// built-in default — so "why is this denied" is answerable without walking the fallback chain by
// hand.
func TestGuardrailsForNameReportsTheTierThatAnswered(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newRoutingEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: "staging"}, cp.GuardrailAppDelete, cp.DispositionAllow); err != nil {
		t.Fatalf("SetGuardrail(staging, app.delete): %v", err)
	}
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: "staging", Name: "website"}, cp.GuardrailAppRun, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail(staging, website, app.run): %v", err)
	}

	gs, err := e.Guardrails(ctx, cp.GuardrailScope{Env: "staging", Name: "website"})
	if err != nil {
		t.Fatalf("Guardrails(staging, website): %v", err)
	}
	want := map[cp.GuardrailCode]struct {
		disposition cp.Disposition
		source      string
	}{
		cp.GuardrailAppRun:    {cp.DispositionDeny, "name"},    // set for this app
		cp.GuardrailAppDelete: {cp.DispositionAllow, "env"},    // set for the environment
		cp.GuardrailAppDeploy: {cp.DispositionAllow, "global"}, // the shipped default policy
	}
	seen := 0
	for _, g := range gs {
		w, ok := want[g.Code]
		if !ok {
			continue
		}
		seen++
		if g.Disposition != w.disposition || g.Source != w.source {
			t.Errorf("%s = (%q, %q), want (%q, %q)", g.Code, g.Disposition, g.Source, w.disposition, w.source)
		}
	}
	if seen != len(want) {
		t.Errorf("the listing covered %d of the %d guardrails this test checks", seen, len(want))
	}
}
