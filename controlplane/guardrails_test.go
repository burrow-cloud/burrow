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
			err := c.policy.evaluateReplicas("test", c.replicas, c.confirmed)
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
