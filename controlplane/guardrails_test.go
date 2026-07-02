// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
	"testing"
)

func TestEvaluateReplicas(t *testing.T) {
	deny := Policy{MaxReplicas: 10} // ceiling + scale-to-zero default to deny
	allowZero := Policy{MaxReplicas: 10}.With(GuardrailScaleToZero, DispositionAllow)
	confirmZero := Policy{MaxReplicas: 10}.With(GuardrailScaleToZero, DispositionConfirm)

	cases := []struct {
		name        string
		policy      Policy
		replicas    int32
		confirmed   bool
		wantCode    GuardrailCode // "" means allowed
		wantConfirm bool
	}{
		{"within limits", deny, 3, false, "", false},
		{"at ceiling", deny, 10, false, "", false},
		{"above ceiling denied", deny, 11, false, GuardrailReplicaCeiling, false},
		{"zero denied", deny, 0, false, GuardrailScaleToZero, false},
		{"zero allowed", allowZero, 0, false, "", false},
		{"zero allowed still ceiling", allowZero, 11, false, GuardrailReplicaCeiling, false},
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
// (default allow) checked first, then the replica ceiling bound. It proves the composition order —
// the categorical gate takes precedence over a within-policy deploy, but an allowed deploy still
// cannot exceed the ceiling — and that env-scoping locks down a single environment while others stay
// permissive (ADR-0007, ADR-0020, ADR-0035 phase 2c).
func TestEvaluateDeploy(t *testing.T) {
	deny := DefaultPolicy().With(GuardrailAppDeploy, DispositionDeny)
	confirm := DefaultPolicy().With(GuardrailAppDeploy, DispositionConfirm)
	// prod locked to confirm, default env inherits the allow default.
	envScoped := DefaultPolicy().With(GuardrailCode("prod."+string(GuardrailAppDeploy)), DispositionConfirm)

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
		{"allowed deploy still bounded by ceiling", DefaultPolicy(), "", 51, false, GuardrailReplicaCeiling, false},
		{"env-scoped prod holds for confirmation", envScoped, "prod", 3, false, GuardrailAppDeploy, true},
		{"env-scoped default env stays allow", envScoped, "", 3, false, "", false},
		{"env-scoped staging inherits allow", envScoped, "staging", 3, false, "", false},
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
// empty and reserved "default" envs reproduce the global lookup exactly.
func TestDispositionEnvFallback(t *testing.T) {
	// Global app.delete = allow; prod overrides it to deny. Staging has no override.
	p := Policy{MaxReplicas: 10}.
		With(GuardrailAppDelete, DispositionAllow).
		With(GuardrailCode("prod."+string(GuardrailAppDelete)), DispositionDeny)

	cases := []struct {
		name string
		env  string
		want Disposition
	}{
		{"prod env-specific override wins", "prod", DispositionDeny},
		{"staging falls back to global", "staging", DispositionAllow},
		{"empty env is the global lookup", "", DispositionAllow},
		{"default env is the global lookup", DefaultEnvironment, DispositionAllow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.disposition(c.env, GuardrailAppDelete); got != c.want {
				t.Errorf("disposition(%q, app.delete) = %q, want %q", c.env, got, c.want)
			}
		})
	}

	// A guardrail with no override anywhere reads as the deny-when-unset default, in any env.
	if got := p.disposition("prod", GuardrailRollback); got != DispositionDeny {
		t.Errorf("unset guardrail in prod = %q, want deny (the safe default)", got)
	}
}

// TestDispositionSource confirms the source label tracks where the effective disposition came from,
// which drives the env-aware `guard list` (ADR-0035 phase 2c).
func TestDispositionSource(t *testing.T) {
	p := Policy{MaxReplicas: 10}.
		With(GuardrailAppDelete, DispositionAllow).
		With(GuardrailCode("prod."+string(GuardrailAppDelete)), DispositionDeny)

	if d, src := p.dispositionSource("prod", GuardrailAppDelete); d != DispositionDeny || src != "env" {
		t.Errorf("prod app.delete = (%q, %q), want (deny, env)", d, src)
	}
	if d, src := p.dispositionSource("staging", GuardrailAppDelete); d != DispositionAllow || src != "global" {
		t.Errorf("staging app.delete = (%q, %q), want (allow, global)", d, src)
	}
	if d, src := p.dispositionSource("prod", GuardrailRollback); d != DispositionDeny || src != "default" {
		t.Errorf("prod app.rollback (unset) = (%q, %q), want (deny, default)", d, src)
	}
}

// TestGuardrailsForMarksSource confirms the env-scoped listing carries a Source per guardrail while
// the global listing leaves it empty (ADR-0035 phase 2c).
func TestGuardrailsForMarksSource(t *testing.T) {
	p := DefaultPolicy().With(GuardrailCode("prod."+string(GuardrailAppDelete)), DispositionDeny)

	for _, g := range p.GuardrailsFor("prod") {
		if g.Source == "" {
			t.Errorf("env listing left Source empty for %s", g.Code)
		}
		if g.Code == GuardrailAppDelete && (g.Disposition != DispositionDeny || g.Source != "env") {
			t.Errorf("prod app.delete = (%q, %q), want (deny, env)", g.Disposition, g.Source)
		}
	}
	for _, g := range p.Guardrails() {
		if g.Source != "" {
			t.Errorf("global listing should leave Source empty, got %q for %s", g.Source, g.Code)
		}
	}
}

// TestEnvScopable confirms only the app-level guardrails can be scoped to an environment; the
// cluster-level ones (addon.*, dns.*) are global (ADR-0035 phase 2c).
func TestEnvScopable(t *testing.T) {
	scopable := []GuardrailCode{GuardrailAppDeploy, GuardrailAppDelete, GuardrailRollback, GuardrailExposePublic, GuardrailScaleToZero, GuardrailReplicaCeiling}
	global := []GuardrailCode{GuardrailDNSWrite, GuardrailDNSDelete, GuardrailAddonInstall, GuardrailAddonRemove, GuardrailAddonDetach, GuardrailAddonRestore}
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

func TestAsGuardrailWrapped(t *testing.T) {
	base := &GuardrailError{Operation: "deploy", Code: GuardrailReplicaCeiling, Requested: 99, Limit: 50, Message: "too many"}
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
