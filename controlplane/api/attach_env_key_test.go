// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// Issue #462 puts the attachment's variable name in the ROUTE rather than in the body, and #485's
// reasoning is why: a body field is dropped by a control plane that does not know it, which writes
// DATABASE_URL over whatever the app kept there and answers 200 with a name nobody asked for.
//
// These are the server's half. burrowd is the compatibility anchor (ADR-0039 §2), so the unnarrowed
// route must keep meaning exactly what it always meant, and the new route must carry the name.

// TestAttachRouteCarriesTheVariableName asserts the name reaches the engine from the path — read off
// the result, which reports the key it wrote.
func TestAttachRouteCarriesTheVariableName(t *testing.T) {
	h, _ := newProvisionedAPI(t)

	rr := do(h, "POST", "/v1/addons/attach/env-key/PG_DSN", token, `{"addon":"postgres","app":"web","confirm":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("attach via the route = %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"secret_key":"PG_DSN"`) {
		t.Errorf("the route's variable name did not reach the engine: %s", rr.Body.String())
	}
}

// TestAttachUnnarrowedRouteStillMeansDatabaseURL is the promise to every client already in the field:
// the old call is served and writes what it always wrote.
func TestAttachUnnarrowedRouteStillMeansDatabaseURL(t *testing.T) {
	h, _ := newProvisionedAPI(t)

	rr := do(h, "POST", "/v1/addons/attach", token, `{"addon":"postgres","app":"web","confirm":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("attach on the unnarrowed route = %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"secret_key":"DATABASE_URL"`) {
		t.Errorf("an older client's attach changed meaning: %s", rr.Body.String())
	}
}

// TestAttachRefusesTheNameAsABodyField keeps the name in ONE place. There is no `env_key` body field
// beside the route — a request carrying one is a request this control plane cannot honour as sent, and
// `decode` refuses it by name rather than dropping it and attaching under the default.
func TestAttachRefusesTheNameAsABodyField(t *testing.T) {
	h, _ := newProvisionedAPI(t)

	rr := do(h, "POST", "/v1/addons/attach", token, `{"addon":"postgres","app":"web","env_key":"PG_DSN"}`)
	if rr.Code == http.StatusOK {
		t.Fatalf("a body field was accepted beside the route: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown_field") {
		t.Errorf("the refusal is not the structured unknown-field one: %d %s", rr.Code, rr.Body.String())
	}
}

// TestAttachRefusesAMalformedVariableName: a path segment is not a licence to write any key. The
// engine validates the name, and the refusal is its own rather than a 404 that would read as version
// skew on the client side.
func TestAttachRefusesAMalformedVariableName(t *testing.T) {
	h, _ := newProvisionedAPI(t)

	rr := do(h, "POST", "/v1/addons/attach/env-key/not%20a%20key", token, `{"addon":"postgres","app":"web"}`)
	if rr.Code == http.StatusOK {
		t.Fatalf("a malformed variable name was accepted: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not a valid environment variable name") {
		t.Errorf("the refusal does not say what is wrong with the name: %d %s", rr.Code, rr.Body.String())
	}
}
