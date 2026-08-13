// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// The lock over HTTP (cloud ADR-0060). Two things are asserted here that no other layer can:
// the endpoints exist and take no confirmation, and a locked refusal crosses the wire with its own
// code — not the guardrail one.

// TestLockEndpointsLockAndUnlockAnApp: PUT locks, DELETE unlocks, and neither takes a body or a
// confirm. The delete in between refuses with 422 and the `locked` code.
func TestLockEndpointsLockAndUnlockAnApp(t *testing.T) {
	h, k, d := newAPI(t)
	d.SetPolicy(cp.Policy{}.With(cp.GuardrailAppDelete, cp.DispositionAllow))
	if err := k.ApplyWorkload(t.Context(), cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}

	rr := do(h, "PUT", "/v1/apps/web/lock", token, "")
	if rr.Code != 200 {
		t.Fatalf("lock = %d %s", rr.Code, rr.Body.String())
	}
	var res struct {
		Subject string `json:"subject"`
		Locked  bool   `json:"locked"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding the lock result: %v", err)
	}
	if res.Subject != "app" || !res.Locked || !res.Changed {
		t.Errorf("lock result = %+v, want a changed app lock", res)
	}

	// The refusal: 422 with the `locked` code, and NOT the guardrail shape. needs_confirmation must
	// be absent — a client that saw it would re-issue the same call with confirm=true and be refused
	// identically, which is the retry loop the code exists to prevent.
	rr = do(h, "DELETE", "/v1/apps/web?confirm=true", token, "")
	if rr.Code != 422 {
		t.Fatalf("delete of a locked app = %d %s, want 422", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"code":"locked"`) {
		t.Errorf("refusal body %s does not carry the locked code", body)
	}
	if strings.Contains(body, "needs_confirmation") {
		t.Errorf("refusal body %s carries needs_confirmation; a lock is not satisfied by confirming", body)
	}
	if !strings.Contains(body, "burrow unlock web") {
		t.Errorf("refusal body %s does not name the unlock command", body)
	}

	// Unlocking takes no confirmation of its own: the unlock IS the deliberate act.
	if rr := do(h, "DELETE", "/v1/apps/web/lock", token, ""); rr.Code != 200 {
		t.Fatalf("unlock = %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(h, "DELETE", "/v1/apps/web?confirm=true", token, ""); rr.Code != 200 {
		t.Fatalf("delete after unlock = %d %s", rr.Code, rr.Body.String())
	}
}

// TestLockEndpointRefusesAnUnknownApp: locking a name nobody deployed is 404, so a typo does not
// leave somebody believing a thing is protected.
func TestLockEndpointRefusesAnUnknownApp(t *testing.T) {
	h, _, _ := newAPI(t)
	if rr := do(h, "PUT", "/v1/apps/nosuch/lock", token, ""); rr.Code != 404 {
		t.Errorf("locking an unknown app = %d %s, want 404", rr.Code, rr.Body.String())
	}
}
