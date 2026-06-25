// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import "testing"

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
	if p.MaxReplicas <= 0 {
		t.Errorf("DefaultPolicy().MaxReplicas = %d, want positive", p.MaxReplicas)
	}
	if p.disposition(GuardrailScaleToZero) != DispositionConfirm {
		t.Errorf("DefaultPolicy() should hold scale-to-zero for confirmation by default")
	}
}

func TestPolicyValidate(t *testing.T) {
	if err := (Policy{MaxReplicas: 0}).Validate(); err == nil {
		t.Errorf("MaxReplicas 0 should be invalid")
	}
	if err := (Policy{MaxReplicas: -3}).Validate(); err == nil {
		t.Errorf("negative MaxReplicas should be invalid")
	}
	if err := (Policy{MaxReplicas: 10}).Validate(); err != nil {
		t.Errorf("MaxReplicas 10 should be valid, got %v", err)
	}
}
