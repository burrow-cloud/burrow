// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestARollbackWaitsUnlessTheCallerSaysNotTo is ADR-0093 §2 on the transport, where the default has
// to be right for callers that predate the parameter. The wait is what an unmodified request gets, an
// opt-out counts only as the literal string "true" — the rule ADR-0080 §1 gives the switches beside
// it — and a rollback that declined to wait comes back with no rollout rather than a claimed one.
func TestARollbackWaitsUnlessTheCallerSaysNotTo(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:2","replicas":1}`)

	// No parameter, and parameters that are not the literal "true": each waits and reports.
	for _, path := range []string{"/v1/apps/web/rollback", "/v1/apps/web/rollback?no_wait=1", "/v1/apps/web/rollback?no_wait=yes"} {
		before := len(k.Rollouts())
		rr := do(h, "POST", path, token, "")
		if rr.Code != 200 {
			t.Fatalf("POST %s = %d %s", path, rr.Code, rr.Body.String())
		}
		var res cp.RollbackResult
		if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
			t.Fatalf("decoding rollback result: %v", err)
		}
		if res.Rollout == nil {
			t.Errorf("POST %s returned no rollout: only an explicit no_wait=true declines the wait", path)
		}
		if got := len(k.Rollouts()) - before; got != 1 {
			t.Errorf("POST %s made %d rollout observations, want 1", path, got)
		}
	}

	before := len(k.Rollouts())
	rr := do(h, "POST", "/v1/apps/web/rollback?no_wait=true", token, "")
	if rr.Code != 200 {
		t.Fatalf("rollback with no_wait=true = %d %s", rr.Code, rr.Body.String())
	}
	var res cp.RollbackResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding rollback result: %v", err)
	}
	if res.Rollout != nil {
		t.Errorf("Rollout = %+v, want nil: nothing was observed, so nothing may be claimed", res.Rollout)
	}
	if got := len(k.Rollouts()) - before; got != 0 {
		t.Errorf("a rollback told not to wait made %d rollout observations", got)
	}
}
