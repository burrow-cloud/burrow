// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestEvaluateReplicas covers the one thing a guardrail has an opinion about in a replica count:
// zero. The CEILING is no longer evaluated here at all — it is an operational limit whose breach is
// a validation failure, checked ahead of the guardrails (ADR-0068 §2) and covered by TestLimits*.
func TestEvaluateReplicas(t *testing.T) {
	deny := Policy{} // scale-to-zero defaults to deny
	allowZero := Policy{}.With(GuardrailScaleToZero, DispositionAllow)
	confirmZero := Policy{}.With(GuardrailScaleToZero, DispositionConfirm)

	cases := []struct {
		name        string
		policy      Policy
		replicas    int32
		confirmed   bool
		wantCode    GuardrailCode // "" means allowed
		wantConfirm bool
	}{
		{"a positive count is not a guardrail concern", deny, 3, false, "", false},
		{"a large positive count is still not one", deny, 5000, false, "", false},
		{"zero denied", deny, 0, false, GuardrailScaleToZero, false},
		{"zero allowed", allowZero, 0, false, "", false},
		{"zero needs confirmation", confirmZero, 0, false, GuardrailScaleToZero, true},
		{"zero confirmed proceeds", confirmZero, 0, true, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.policy.evaluateReplicas("", "test", c.replicas, c.confirmed)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("evaluateReplicas(%d, confirmed=%v) = %v, want allowed", c.replicas, c.confirmed, err)
				}
				return
			}
			g, ok := AsGuardrail(err)
			if !ok {
				t.Fatalf("evaluateReplicas(%d) = %v, want GuardrailError", c.replicas, err)
			}
			if g.Code != c.wantCode {
				t.Fatalf("code = %q, want %q", g.Code, c.wantCode)
			}
			if g.NeedsConfirmation != c.wantConfirm {
				t.Fatalf("NeedsConfirmation = %v, want %v", g.NeedsConfirmation, c.wantConfirm)
			}
			if g.Operation != "test" {
				t.Fatalf("operation = %q, want test", g.Operation)
			}
		})
	}
}

// TestEvaluateDeploy exercises the composed deploy gate: the categorical app.deploy guardrail
// (default allow), then the scale-to-zero gate on the resolved count. It proves the composition
// order and that env-scoping locks down a single environment while others stay permissive
// (ADR-0007, ADR-0020, ADR-0035 phase 2c). The replica ceiling is not part of it: a bound is checked
// before any of this runs (ADR-0068 §2).
func TestEvaluateDeploy(t *testing.T) {
	deny := DefaultPolicy().With(GuardrailAppDeploy, DispositionDeny)
	confirm := DefaultPolicy().With(GuardrailAppDeploy, DispositionConfirm)
	// staging locked to confirm; the default environment (`prod`) inherits the allow default, since
	// its policy IS the global policy (ADR-0067 §2).
	envScoped := DefaultPolicy().With(GuardrailCode("staging."+string(GuardrailAppDeploy)), DispositionConfirm)

	cases := []struct {
		name        string
		policy      Policy
		env         string
		replicas    int32
		confirmed   bool
		wantCode    GuardrailCode // "" means allowed
		wantConfirm bool
	}{
		{"default allow proceeds", DefaultPolicy(), "", 3, false, "", false},
		{"deny refuses", deny, "", 3, false, GuardrailAppDeploy, false},
		{"confirm needs confirmation", confirm, "", 3, false, GuardrailAppDeploy, true},
		{"confirm confirmed proceeds", confirm, "", 3, true, "", false},
		{"a count above the old ceiling is no longer a guardrail matter", DefaultPolicy(), "", 51, false, "", false},
		{"env-scoped staging holds for confirmation", envScoped, "staging", 3, false, GuardrailAppDeploy, true},
		{"env-scoped default env stays allow", envScoped, "", 3, false, "", false},
		{"the default environment prod is the global policy", envScoped, DefaultEnvironment, 3, false, "", false},
		{"env-scoped dev inherits allow", envScoped, "dev", 3, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.policy.evaluateDeploy(c.env, c.replicas, c.confirmed)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("evaluateDeploy(env=%q, %d, confirmed=%v) = %v, want allowed", c.env, c.replicas, c.confirmed, err)
				}
				return
			}
			g, ok := AsGuardrail(err)
			if !ok {
				t.Fatalf("evaluateDeploy(env=%q, %d) = %v, want GuardrailError", c.env, c.replicas, err)
			}
			if g.Code != c.wantCode {
				t.Fatalf("code = %q, want %q", g.Code, c.wantCode)
			}
			if g.NeedsConfirmation != c.wantConfirm {
				t.Fatalf("NeedsConfirmation = %v, want %v", g.NeedsConfirmation, c.wantConfirm)
			}
			if g.Operation != "deploy" {
				t.Fatalf("operation = %q, want deploy", g.Operation)
			}
		})
	}
}

// TestDispositionEnvFallback exercises the env to global to default lookup order (ADR-0035 phase 2c):
// an env-specific override wins; absent one, a named env falls back to the global override; and the
// empty env and the default environment `prod` reproduce the global lookup exactly (ADR-0067 §2).
func TestDispositionEnvFallback(t *testing.T) {
	// Global app.delete = allow; staging overrides it to deny. Dev has no override.
	p := Policy{}.
		With(GuardrailAppDelete, DispositionAllow).
		With(GuardrailCode("staging."+string(GuardrailAppDelete)), DispositionDeny)

	cases := []struct {
		name string
		env  string
		want Disposition
	}{
		{"staging env-specific override wins", "staging", DispositionDeny},
		{"dev falls back to global", "dev", DispositionAllow},
		{"empty env is the global lookup", "", DispositionAllow},
		{"the default environment prod is the global lookup", DefaultEnvironment, DispositionAllow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.disposition(c.env, GuardrailAppDelete); got != c.want {
				t.Errorf("disposition(%q, app.delete) = %q, want %q", c.env, got, c.want)
			}
		})
	}

	// A guardrail with no override anywhere reads as the deny-when-unset default, in any env.
	if got := p.disposition("staging", GuardrailRollback); got != DispositionDeny {
		t.Errorf("unset guardrail in staging = %q, want deny (the safe default)", got)
	}
}

// TestDispositionSource confirms the source label tracks where the effective disposition came from,
// which drives the env-aware `guard list` (ADR-0035 phase 2c).
func TestDispositionSource(t *testing.T) {
	p := Policy{}.
		With(GuardrailAppDelete, DispositionAllow).
		With(GuardrailCode("staging."+string(GuardrailAppDelete)), DispositionDeny)

	if d, src := p.dispositionSource("staging", GuardrailAppDelete); d != DispositionDeny || src != "env" {
		t.Errorf("staging app.delete = (%q, %q), want (deny, env)", d, src)
	}
	if d, src := p.dispositionSource("dev", GuardrailAppDelete); d != DispositionAllow || src != "global" {
		t.Errorf("dev app.delete = (%q, %q), want (allow, global)", d, src)
	}
	if d, src := p.dispositionSource("staging", GuardrailRollback); d != DispositionDeny || src != "default" {
		t.Errorf("staging app.rollback (unset) = (%q, %q), want (deny, default)", d, src)
	}
}

// TestGuardrailsForMarksSource confirms the env-scoped listing carries a Source per guardrail while
// the global listing leaves it empty (ADR-0035 phase 2c).
func TestGuardrailsForMarksSource(t *testing.T) {
	p := DefaultPolicy().With(GuardrailCode("staging."+string(GuardrailAppDelete)), DispositionDeny)

	for _, g := range p.GuardrailsFor("staging") {
		if g.Source == "" {
			t.Errorf("env listing left Source empty for %s", g.Code)
		}
		if g.Code == GuardrailAppDelete && (g.Disposition != DispositionDeny || g.Source != "env") {
			t.Errorf("staging app.delete = (%q, %q), want (deny, env)", g.Disposition, g.Source)
		}
	}
	for _, g := range p.Guardrails() {
		if g.Source != "" {
			t.Errorf("global listing should leave Source empty, got %q for %s", g.Source, g.Code)
		}
	}
}

// TestEnvScopable confirms which guardrails can be scoped to an environment: the app-level ones,
// whose operations always carry an environment, and not the cluster-level ones (addon.*, dns.*),
// whose dispositions are looked up with no environment at all (ADR-0035 phase 2c).
func TestEnvScopable(t *testing.T) {
	scopable := []GuardrailCode{GuardrailAppDeploy, GuardrailAppDelete, GuardrailRollback, GuardrailExposePublic, GuardrailScaleToZero, GuardrailAppRun, GuardrailAutoscale}
	global := []GuardrailCode{GuardrailDNSWrite, GuardrailDNSDelete, GuardrailAddonInstall, GuardrailAddonRemove, GuardrailAddonDetach, GuardrailAddonRestore, GuardrailBucketCreate}
	for _, c := range scopable {
		if !EnvScopable(c) {
			t.Errorf("EnvScopable(%q) = false, want true", c)
		}
	}
	for _, c := range global {
		if EnvScopable(c) {
			t.Errorf("EnvScopable(%q) = true, want false (cluster-level)", c)
		}
	}
}

// TestEnvScopableIsDeclaredNotInferred pins ADR-0068 §5: scopability is a property a code declares,
// not one read off its name. The check that used to stand here was strings.HasPrefix(code, "app."),
// so a code named outside that prefix could not be scoped however much it deserved to be, and one
// named inside it was scopable whether or not its operation carried an environment. Both halves are
// asserted: a made-up `app.` code is NOT scopable because it declares nothing, and every scopable
// code is one the catalogue lists.
func TestEnvScopableIsDeclaredNotInferred(t *testing.T) {
	if EnvScopable(GuardrailCode("app.not_a_real_code")) {
		t.Error("an unknown code beginning with `app.` is scopable, so scopability is still inferred from the name")
	}
	if EnvScopable(GuardrailCode("")) {
		t.Error("the empty code is scopable")
	}
	for _, g := range knownGuardrails {
		if g.envScoped && !KnownGuardrail(g.code) {
			t.Errorf("%q declares itself scopable but is not a known guardrail", g.code)
		}
	}
}

// TestReplicaCeilingIsNotAGuardrail pins ADR-0068 §2: the ceiling is a bound, not a code whose
// disposition an operator can set. A guardrail set that still carried it would let `guard set
// app.replica_ceiling allow` turn the limit off, which is the whole failure the record corrects.
func TestReplicaCeilingIsNotAGuardrail(t *testing.T) {
	if KnownGuardrail(GuardrailCode(LimitReplicaCeiling)) {
		t.Errorf("%q is still a guardrail code; a limit you can disposition away is not a limit (ADR-0068 §2)", LimitReplicaCeiling)
	}
	for _, g := range DefaultPolicy().Guardrails() {
		if string(g.Code) == string(LimitReplicaCeiling) {
			t.Errorf("the default policy still lists %q", g.Code)
		}
	}
}

func TestAsGuardrailWrapped(t *testing.T) {
	base := &GuardrailError{Operation: "deploy", Code: GuardrailScaleToZero, Requested: 99, Limit: 50, Message: "too many"}
	wrapped := fmt.Errorf("deploy web: %w", base)

	g, ok := AsGuardrail(wrapped)
	if !ok {
		t.Fatalf("AsGuardrail did not see through the wrap")
	}
	if g.Requested != 99 || g.Limit != 50 {
		t.Fatalf("g = %+v, want requested 99 limit 50", g)
	}
	if AsGuardrailMiss := errors.As(wrapped, new(*GuardrailError)); !AsGuardrailMiss {
		t.Fatalf("errors.As should also find it")
	}

	if _, ok := AsGuardrail(errors.New("plain")); ok {
		t.Fatalf("AsGuardrail matched a non-guardrail error")
	}
}

// TestDenyRefusalSteersTowardScoping covers what a refused caller is told. A deny default is a floor
// rather than a fixed setting (ADR-0065 §3), and the failure mode worth designing against is an
// operator meeting one refusal and reaching for a global `guard set app.delete allow` — relaxing
// production to unblock a sandbox. So an env-scopable code's refusal leads with the --env form and
// names the environment it was refused in, while a cluster-level code's refusal offers the global
// form and says plainly how far it reaches, because dns.delete declares itself un-scopable: its
// operation carries no environment for an override to be read against.
func TestDenyRefusalSteersTowardScoping(t *testing.T) {
	p := DefaultPolicy()

	err := p.evaluateGuardrail("staging", "app delete", GuardrailAppDelete, true, "deleting the app")
	g, ok := AsGuardrail(err)
	if !ok {
		t.Fatalf("app.delete under the default policy = %v, want a GuardrailError", err)
	}
	for _, want := range []string{"floor, not a fixed setting", "guard set --env staging app.delete confirm", "relaxing it everywhere"} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("app.delete refusal %q missing %q", g.Message, want)
		}
	}

	// With no named environment there is no name to print, so the placeholder keeps the --env shape.
	err = p.evaluateGuardrail("", "app delete", GuardrailAppDelete, true, "deleting the app")
	g, _ = AsGuardrail(err)
	if !strings.Contains(g.Message, "guard set --env <env> app.delete confirm") {
		t.Errorf("unscoped app.delete refusal %q should still show the --env form", g.Message)
	}

	err = p.evaluateGuardrail("", "domain remove", GuardrailDNSDelete, true, "removing the DNS record")
	g, ok = AsGuardrail(err)
	if !ok {
		t.Fatalf("dns.delete under the default policy = %v, want a GuardrailError", err)
	}
	for _, want := range []string{"guard set dns.delete confirm", "whole cluster", "cannot be scoped to one environment"} {
		if !strings.Contains(g.Message, want) {
			t.Errorf("dns.delete refusal %q missing %q", g.Message, want)
		}
	}
	if strings.Contains(g.Message, "--env") {
		t.Errorf("dns.delete refusal %q offers a --env form it cannot honour", g.Message)
	}
}
