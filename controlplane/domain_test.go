// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"testing"
)

func TestAppValidate(t *testing.T) {
	cases := []struct {
		name string
		app  string
		ok   bool
	}{
		{"simple", "web", true},
		{"with dashes and digits", "my-app-2", true},
		{"single char", "a", true},
		{"empty", "", false},
		{"uppercase", "Web", false},
		{"leading dash", "-web", false},
		{"trailing dash", "web-", false},
		{"underscore", "my_app", false},
		{"slash", "team/web", false},
		{"too long", string(make([]byte, 64)), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := App{Name: c.app}.Validate()
			if c.ok && err != nil {
				t.Fatalf("App{%q}.Validate() = %v, want nil", c.app, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("App{%q}.Validate() = nil, want error", c.app)
			}
		})
	}
}

func TestReleaseValidate(t *testing.T) {
	valid := Release{App: "web", Image: "registry.example.com/web:1", Replicas: 2, Status: ReleaseDeployed}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid release: unexpected error %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(r *Release)
		wantErr bool
	}{
		{"missing app", func(r *Release) { r.App = "" }, true},
		{"bad app name", func(r *Release) { r.App = "Web_1" }, true},
		{"missing image", func(r *Release) { r.Image = "" }, true},
		{"negative replicas", func(r *Release) { r.Replicas = -1 }, true},
		{"zero replicas ok", func(r *Release) { r.Replicas = 0 }, false},
		{"empty status ok", func(r *Release) { r.Status = "" }, false},
		{"bogus status", func(r *Release) { r.Status = "weird" }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := valid
			c.mutate(&r)
			err := r.Validate()
			if c.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestReleaseStatusValid(t *testing.T) {
	for _, s := range []ReleaseStatus{ReleasePending, ReleaseDeployed, ReleaseFailed, ReleaseSuperseded} {
		if !s.Valid() {
			t.Errorf("status %q should be valid", s)
		}
	}
	if ReleaseStatus("nonsense").Valid() {
		t.Errorf("unknown status should be invalid")
	}
}

func TestDefaultPolicyIsValid(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Validate(); err != nil {
		t.Fatalf("DefaultPolicy() is invalid: %v", err)
	}
	if p.disposition(context.Background(), GuardrailScope{}, GuardrailScaleToZero) != DispositionConfirm {
		t.Errorf("DefaultPolicy() should hold scale-to-zero for confirmation by default")
	}
}

// TestDefaultPolicyDeniesIrreversibleDeletes pins ADR-0065 §3's tier-2 defaults: app.delete and
// dns.delete ship as deny, not confirm. Both destroy something a human cannot restore afterwards —
// an app's release history, a public DNS record Burrow may not have created — and a confirmation is
// a real control only for someone who reads it. Each remains one deliberate `guard set` away.
func TestDefaultPolicyDeniesIrreversibleDeletes(t *testing.T) {
	p := DefaultPolicy()
	for _, code := range []GuardrailCode{GuardrailAppDelete, GuardrailDNSDelete} {
		if got := p.disposition(context.Background(), GuardrailScope{}, code); got != DispositionDeny {
			t.Errorf("DefaultPolicy() %s = %q, want %q (ADR-0065 §3)", code, got, DispositionDeny)
		}
	}

	// The default is a floor: an explicit disposition in the policy table still wins, so an
	// operator who deliberately chose confirm is not moved by the changed default.
	relaxed := p.With(GuardrailAppDelete, DispositionConfirm).With(GuardrailDNSDelete, DispositionAllow)
	if got := relaxed.disposition(context.Background(), GuardrailScope{}, GuardrailAppDelete); got != DispositionConfirm {
		t.Errorf("stored app.delete = %q, want the operator's confirm to win over the default", got)
	}
	if got := relaxed.disposition(context.Background(), GuardrailScope{}, GuardrailDNSDelete); got != DispositionAllow {
		t.Errorf("stored dns.delete = %q, want the operator's allow to win over the default", got)
	}

	// app.delete is env-scopable, so the deny is a per-environment floor a single environment can
	// be lifted off; dns.delete is not, and its deny is cluster-wide (ADR-0065 §3, ADR-0068).
	if !EnvScopable(GuardrailAppDelete) {
		t.Errorf("app.delete should be env-scopable, so its deny can be relaxed per environment")
	}
	if EnvScopable(GuardrailDNSDelete) {
		t.Errorf("dns.delete is expected to be cluster-wide: its operation carries no environment for an override to be read against")
	}
}

// TestPolicyValidate covers what a Policy can now be wrong about. Since ADR-0068 §2 it carries
// dispositions and nothing else — the numbers moved to OperationalConfig — so an invalid
// disposition is the only incoherence left, and an empty policy is valid rather than under-filled.
func TestPolicyValidate(t *testing.T) {
	if err := (Policy{}).Validate(); err != nil {
		t.Errorf("an empty policy should be valid (every guardrail reads as its default), got %v", err)
	}
	if err := (Policy{}.With(GuardrailAppDelete, Disposition("maybe"))).Validate(); err == nil {
		t.Errorf("an invalid disposition should be rejected")
	}
	if err := (Policy{}.With(GuardrailAppDelete, DispositionDeny)).Validate(); err != nil {
		t.Errorf("a valid disposition should pass, got %v", err)
	}
}
