// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAmbiguousEnvironmentRefusal confirms the HTTP layer surfaces the ADR-0047 §1 forcing function:
// once more than one environment is registered, a mutating request that names no environment is
// refused with the structured "ambiguous_environment" error, while a read-only request is unaffected
// (ADR-0047 §3).
func TestAmbiguousEnvironmentRefusal(t *testing.T) {
	h, _, d := newAPI(t)
	// Register a named environment alongside the implicit default so the target is ambiguous.
	if err := d.CreateEnvironment(context.Background(), "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// A mutating deploy with no env is refused with 422 + the machine-readable code, naming both
	// environments so the agent can re-issue with an explicit target.
	rr := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"registry.example.com/web:1","replicas":1}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("deploy status = %d, want 422 (body %s)", rr.Code, rr.Body.String())
	}
	var eb errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &eb); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if eb.Code != "ambiguous_environment" {
		t.Errorf("deploy error code = %q, want ambiguous_environment", eb.Code)
	}
	if !strings.Contains(eb.Error, "default") || !strings.Contains(eb.Error, "staging") {
		t.Errorf("error message does not name both environments: %q", eb.Error)
	}

	// A mutating delete with no env is refused the same way.
	if rr := do(h, "DELETE", "/v1/apps/web", token, ""); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("delete status = %d, want 422 (body %s)", rr.Code, rr.Body.String())
	}

	// A read-only apps listing with no env is unaffected — the survey path stays frictionless.
	if rr := do(h, "GET", "/v1/apps", token, ""); rr.Code != http.StatusOK {
		t.Errorf("apps listing status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
}
