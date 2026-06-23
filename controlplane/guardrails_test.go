// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"errors"
	"fmt"
	"testing"
)

func TestCheckReplicas(t *testing.T) {
	p := Policy{MaxReplicas: 10, AllowScaleToZero: false}
	pz := Policy{MaxReplicas: 10, AllowScaleToZero: true}

	cases := []struct {
		name     string
		policy   Policy
		replicas int32
		wantCode GuardrailCode // "" means allowed
	}{
		{"within limits", p, 3, ""},
		{"at ceiling", p, 10, ""},
		{"above ceiling", p, 11, GuardrailReplicaCeiling},
		{"zero disallowed", p, 0, GuardrailScaleToZero},
		{"zero allowed", pz, 0, ""},
		{"zero allowed still ceiling", pz, 11, GuardrailReplicaCeiling},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.policy.checkReplicas("test", c.replicas)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("checkReplicas(%d) = %v, want allowed", c.replicas, err)
				}
				return
			}
			g, ok := AsGuardrail(err)
			if !ok {
				t.Fatalf("checkReplicas(%d) = %v, want GuardrailError", c.replicas, err)
			}
			if g.Code != c.wantCode {
				t.Fatalf("code = %q, want %q", g.Code, c.wantCode)
			}
			if g.Operation != "test" {
				t.Fatalf("operation = %q, want test", g.Operation)
			}
		})
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
