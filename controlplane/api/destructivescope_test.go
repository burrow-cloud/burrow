// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/api"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// newProvisionedAPI is newAPI with a database provisioner wired in, which the detach path needs to
// get as far as resolving an environment at all.
func newProvisionedAPI(t *testing.T) (http.Handler, *fake.Database) {
	t.Helper()
	d := fake.NewDatabase()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock:       fake.NewClock(time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)),
		IDs:         fake.NewIDs(),
		Resolver:    fake.NewResolver(),
		Credentials: fake.NewCredentials(),
		DNS:         fake.NewDNSFactory(),

		DatabaseProvisioner: fake.NewProvisioner(),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, d
}

// Issue #485 moved four destructive narrowings out of a request parameter and into the route, so that
// a control plane that cannot express the scope refuses the call instead of performing it wider (the
// client half of that is in client/destructivescope_test.go).
//
// These are the server's half, and it is the OTHER guarantee: burrowd is the compatibility anchor
// (ADR-0039 §2), so a newer control plane keeps serving OLDER clients on the OLD routes with the OLD
// meanings. A client in the field must keep working, and its request must not quietly start meaning
// something new.

// TestEnvironmentRoutesCarryTheScopeAndTheOldFormsStillWork covers the three environment cases in
// one place, because they are one property.
//
// The lever is ADR-0035's own refusal: with more than one environment registered, a MUTATING call
// that names none is refused as ambiguous rather than guessed at. So a call on the new route that is
// NOT refused that way is a call whose environment reached the engine — which is exactly the thing an
// older control plane failed to do with a dropped parameter or a dropped body field. The old forms
// are then checked to answer identically, because burrowd is the compatibility anchor and a client in
// the field must keep working (ADR-0039 §2).
func TestEnvironmentRoutesCarryTheScopeAndTheOldFormsStillWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unnarrowed, viaRoute and viaOldForm are the same call three ways: naming no environment,
		// naming it in the path, and naming it the way a client already in the field does.
		method     string
		unnarrowed string
		viaRoute   string
		viaOldForm string
		body       string
		oldBody    string
	}{
		{
			name: "delete an app", method: "DELETE",
			unnarrowed: "/v1/apps/web", viaRoute: "/v1/apps/web/env/staging", viaOldForm: "/v1/apps/web?env=staging",
		},
		// The detach names no old form, and the empty string is what says so rather than an omission.
		// Its legacy route now means the DESTRUCTIVE disposition (ADR-0090, see the test below), so
		// there is no older shape that answers identically to the keeping one — which is the whole
		// point of moving the disposition into the route.
		{
			name: "detach an app from an add-on", method: "POST",
			unnarrowed: "/v1/addons/detach/data/keep", viaRoute: "/v1/addons/detach/data/keep/env/staging",
			body: `{"addon":"postgres","app":"web"}`,
		},
		{
			name: "restore an app's database", method: "POST",
			unnarrowed: "/v1/addons/restore", viaRoute: "/v1/addons/restore/env/staging", viaOldForm: "/v1/addons/restore",
			body: `{"addon":"postgres","app":"web","backup":"b-1"}`, oldBody: `{"addon":"postgres","app":"web","backup":"b-1","env":"staging"}`,
		},
		// A statement is the sharpest case of the four (ADR-0087): a dropped environment does not
		// merely read the wrong instance, it RUNS the caller's SQL against the app of that name on
		// the default environment's, and the statement may write.
		{
			name: "run a statement against an app's database", method: "POST",
			unnarrowed: "/v1/addons/sql", viaRoute: "/v1/addons/sql/env/staging", viaOldForm: "/v1/addons/sql",
			body: `{"addon":"postgres","app":"web","statement":"select 1"}`, oldBody: `{"addon":"postgres","app":"web","statement":"select 1","env":"staging"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, d := newProvisionedAPI(t)
			d.SetPolicy(cp.DefaultPolicy().
				With(cp.GuardrailAppDelete, cp.DispositionAllow).
				With(cp.GuardrailAddonDetach, cp.DispositionAllow).
				With(cp.GuardrailAddonRestore, cp.DispositionAllow).
				With(cp.GuardrailAddonSQL, cp.DispositionAllow))
			if err := d.CreateEnvironment(context.Background(), "staging", "burrow-apps-staging"); err != nil {
				t.Fatalf("CreateEnvironment: %v", err)
			}

			// Naming no environment at all is ambiguous with two registered — the refusal that makes
			// the rest of this test mean something.
			if rr := do(h, tc.method, tc.unnarrowed, token, tc.body); !strings.Contains(rr.Body.String(), "ambiguous_environment") {
				t.Fatalf("unnarrowed call = %d %s, want the ambiguous-environment refusal", rr.Code, rr.Body.String())
			}
			// Through the route it is not ambiguous, so the path's environment reached the engine.
			viaRoute := do(h, tc.method, tc.viaRoute, token, tc.body)
			if strings.Contains(viaRoute.Body.String(), "ambiguous_environment") {
				t.Fatalf("the route's environment did not reach the engine: %d %s", viaRoute.Code, viaRoute.Body.String())
			}
			// And an older client's form is served, identically, where one exists that means the same
			// thing.
			if tc.viaOldForm != "" {
				viaOldForm := do(h, tc.method, tc.viaOldForm, token, tc.oldBody)
				if viaOldForm.Code != viaRoute.Code || viaOldForm.Body.String() != viaRoute.Body.String() {
					t.Errorf("an older client's form = %d %s, want the same answer as the route form %d %s",
						viaOldForm.Code, viaOldForm.Body.String(), viaRoute.Code, viaRoute.Body.String())
				}
			}
			// An environment nobody registered is the engine's own 404 rather than a missing route,
			// which is what keeps a typo from reading as version skew on the client side.
			if rr := do(h, tc.method, strings.Replace(tc.viaRoute, "staging", "ghost", 1), token, tc.body); !strings.Contains(rr.Body.String(), "unknown environment") {
				t.Errorf("an unregistered environment = %d %s, want the engine's unknown-environment answer", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestRemoveAddonDataRoutesDoWhatTheParameterDid asserts the two removal routes carry exactly the
// dispositions `delete_data` used to: /data/keep leaves the volume and says which one survived,
// /data/delete destroys it.
func TestRemoveAddonDataRoutesDoWhatTheParameterDid(t *testing.T) {
	for _, tc := range []struct {
		route       string
		wantDeleted string
	}{
		{"/v1/addons/burrow-postgres/data/keep?confirm=true", `"data_deleted":false`},
		{"/v1/addons/burrow-postgres/data/delete?confirm=true", `"data_deleted":true`},
	} {
		t.Run(tc.route, func(t *testing.T) {
			h, _, _ := newProviderAPI(t)
			if rr := do(h, "POST", "/v1/addons", token, `{"type":"postgres","confirm":true}`); rr.Code != 200 {
				t.Fatalf("install addon = %d %s", rr.Code, rr.Body.String())
			}
			rr := do(h, "DELETE", tc.route, token, "")
			if rr.Code != http.StatusOK {
				t.Fatalf("remove addon = %d %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.wantDeleted) {
				t.Errorf("removal reports the wrong data disposition, want %s: %s", tc.wantDeleted, rr.Body.String())
			}
		})
	}
}

// TestRemoveAddonLegacyRouteKeepsItsMeaningForOlderClients is the deliberate asymmetry in this
// change, and the reason it is deliberate.
//
// The other three narrowings have an unnarrowed form that still means what it always meant — an
// absent environment is the default environment, on every version. This one does not: `delete_data`
// INVERTED the default (issue #323), so an empty removal means "keep" to a client built after it and
// meant "destroy" to one built before. The server cannot tell those two apart, and the two readings
// differ by whether a database still exists afterwards.
//
// So the legacy route is left exactly as it is, reading an absent parameter as KEEP. A client built
// after the inversion gets precisely what it asked for. One built before it gets a NARROWER outcome
// than it expected — the volume survives, and the response names it — which is recoverable, where
// guessing the other way is not. What the route must never do is start meaning something new.
func TestRemoveAddonLegacyRouteKeepsItsMeaningForOlderClients(t *testing.T) {
	h, _, _ := newProviderAPI(t)
	if rr := do(h, "POST", "/v1/addons", token, `{"type":"postgres","confirm":true}`); rr.Code != 200 {
		t.Fatalf("install addon = %d %s", rr.Code, rr.Body.String())
	}

	// No disposition at all: served, and the data survives.
	rr := do(h, "DELETE", "/v1/addons/burrow-postgres?confirm=true", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("an older client's removal = %d %s, want it still served", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"data_deleted":false`) {
		t.Errorf("an older client's removal destroyed the data: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"retained_data_volume":"burrow-postgres-1"`) {
		t.Errorf("the removal does not name the volume that survived, which is what makes the conservative reading recoverable: %s", rr.Body.String())
	}
}

// TestRemoveAddonRejectsAnyOtherDataDisposition keeps the two routes literal. A value that is neither
// "keep" nor "delete" must be no route at all — a typo has to 404, never fall through to whichever
// disposition a wildcard route would have defaulted to.
func TestRemoveAddonRejectsAnyOtherDataDisposition(t *testing.T) {
	h, _, _ := newProviderAPI(t)
	if rr := do(h, "POST", "/v1/addons", token, `{"type":"postgres","confirm":true}`); rr.Code != 200 {
		t.Fatalf("install addon = %d %s", rr.Code, rr.Body.String())
	}
	for _, path := range []string{
		"/v1/addons/burrow-postgres/data/destroy?confirm=true",
		"/v1/addons/burrow-postgres/data?confirm=true",
		"/v1/addons/burrow-postgres/data/delete/now?confirm=true",
	} {
		if rr := do(h, "DELETE", path, token, ""); rr.Code != http.StatusNotFound {
			t.Errorf("DELETE %s = %d, want 404: only the two literal dispositions are routes", path, rr.Code)
		}
	}
	// And the add-on is still there, so none of those was served by something.
	if rr := do(h, "GET", "/v1/addons", token, ""); !strings.Contains(rr.Body.String(), "burrow-postgres") {
		t.Errorf("an unrecognised disposition removed the add-on: %s", rr.Body.String())
	}
}

// TestDetachAddonDataRoutesCarryTheDisposition asserts the two detach routes mean what they say: one
// keeps the app's database and one destroys it, and the answer states which happened (ADR-0090 §2).
func TestDetachAddonDataRoutesCarryTheDisposition(t *testing.T) {
	for _, tc := range []struct {
		route       string
		wantDeleted string
	}{
		{"/v1/addons/detach/data/keep", `"data_deleted":false`},
		{"/v1/addons/detach/data/delete", `"data_deleted":true`},
	} {
		t.Run(tc.route, func(t *testing.T) {
			h, d := newProvisionedAPI(t)
			d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailAddonDetach, cp.DispositionAllow))
			rr := do(h, "POST", tc.route, token, `{"addon":"postgres","app":"web"}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("detach = %d %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.wantDeleted) {
				t.Errorf("the detach reports the wrong data disposition, want %s: %s", tc.wantDeleted, rr.Body.String())
			}
		})
	}
}

// TestDetachAddonLegacyRouteKeepsItsMeaningForOlderClients is the removal's asymmetry on the smaller
// verb, and it lands the OTHER way round.
//
// ADR-0090 inverted this default too: an empty detach means "keep" to a client built after it and
// meant "drop the database" to one built before, and the server cannot tell them apart. Here the
// conservative reading is not available, because the two readings differ by what the CLIENT told its
// user. A pre-ADR-0090 `burrow` prints "destroying its data" at the confirmation prompt and reports a
// destroyed database afterwards; serving it a detach that quietly kept the rows is precisely the
// failure ADR-0090 §5 exists to prevent — somebody scrubbing an app's data before handing over a
// cluster, reading the prompt, and believing it was gone — arriving by version skew rather than by
// wording. So the legacy route keeps its old meaning exactly, and no current client can reach it.
func TestDetachAddonLegacyRouteKeepsItsMeaningForOlderClients(t *testing.T) {
	for _, route := range []string{"/v1/addons/detach", "/v1/addons/detach/env/staging"} {
		t.Run(route, func(t *testing.T) {
			h, d := newProvisionedAPI(t)
			d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailAddonDetach, cp.DispositionAllow))
			if err := d.CreateEnvironment(context.Background(), "staging", "burrow-apps-staging"); err != nil {
				t.Fatalf("CreateEnvironment: %v", err)
			}
			rr := do(h, "POST", route, token, `{"addon":"postgres","app":"web","env":"staging"}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("an older client's detach = %d %s, want it still served", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), `"data_deleted":true`) {
				t.Errorf("an older client's detach kept the database its own prompt said it destroyed: %s", rr.Body.String())
			}
		})
	}
}

// TestDetachAddonRejectsAnyOtherDataDisposition keeps the two routes literal, for the removal's
// reason: a typo has to 404, never fall through to whichever disposition a wildcard would default to.
func TestDetachAddonRejectsAnyOtherDataDisposition(t *testing.T) {
	h, d := newProvisionedAPI(t)
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailAddonDetach, cp.DispositionAllow))
	for _, path := range []string{"/v1/addons/detach/data/destroy", "/v1/addons/detach/data/", "/v1/addons/detach/data/keep/keep"} {
		if rr := do(h, "POST", path, token, `{"addon":"postgres","app":"web"}`); rr.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d %s, want 404 — an unrecognised disposition must be no route at all", path, rr.Code, rr.Body.String())
		}
	}
}
