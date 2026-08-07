// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// Issue #485 moved the remaining scope narrowings out of a request parameter and into the route, so
// that a control plane which cannot express the scope refuses the call instead of performing it
// wider. The client half of that is in client/scopesweep_test.go.
//
// This is the SERVER's half, and it is the other guarantee: burrowd is the compatibility anchor
// (ADR-0039 §2), so a newer control plane keeps serving OLDER clients on the OLD routes with the OLD
// meanings. A client in the field must keep working, and its request must not quietly start meaning
// something new.

// TestEveryRemainingWriteTakesItsEnvironmentFromTheRoute covers the environment cases in one place,
// because they are one property.
//
// The lever is ADR-0035's own refusal: with more than one environment registered, a MUTATING call
// that names none is refused as ambiguous rather than guessed at. So a call on the new route that is
// NOT refused that way is a call whose environment reached the engine — exactly the thing an older
// control plane failed to do with a dropped parameter or a dropped body field. The old form is then
// checked to answer identically, because a client in the field must keep working.
func TestEveryRemainingWriteTakesItsEnvironmentFromTheRoute(t *testing.T) {
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
			name: "deploy", method: "POST",
			unnarrowed: "/v1/apps/web/deploy", viaRoute: "/v1/apps/web/deploy/env/staging", viaOldForm: "/v1/apps/web/deploy",
			body:    `{"image":"ghcr.io/x:1","replicas":1,"confirm":true}`,
			oldBody: `{"image":"ghcr.io/x:1","replicas":1,"confirm":true,"env":"staging"}`,
		},
		{
			name: "rollback", method: "POST",
			unnarrowed: "/v1/apps/web/rollback?confirm=true", viaRoute: "/v1/apps/web/rollback/env/staging?confirm=true",
			viaOldForm: "/v1/apps/web/rollback?confirm=true&env=staging",
		},
		{
			name: "scale", method: "POST",
			unnarrowed: "/v1/apps/web/scale", viaRoute: "/v1/apps/web/scale/env/staging", viaOldForm: "/v1/apps/web/scale",
			body: `{"replicas":2,"confirm":true}`, oldBody: `{"replicas":2,"confirm":true,"env":"staging"}`,
		},
		{
			name: "expose", method: "POST",
			unnarrowed: "/v1/apps/web/expose", viaRoute: "/v1/apps/web/expose/env/staging", viaOldForm: "/v1/apps/web/expose",
			body:    `{"host":"s.example.com","port":8080,"confirm":true}`,
			oldBody: `{"host":"s.example.com","port":8080,"confirm":true,"env":"staging"}`,
		},
		{
			name: "unexpose", method: "POST",
			unnarrowed: "/v1/apps/web/unexpose", viaRoute: "/v1/apps/web/unexpose/env/staging",
			viaOldForm: "/v1/apps/web/unexpose?env=staging",
		},
		{
			name: "secret set", method: "POST",
			unnarrowed: "/v1/apps/web/secrets", viaRoute: "/v1/apps/web/secrets/env/staging", viaOldForm: "/v1/apps/web/secrets",
			body: `{"key":"K","value":"V"}`, oldBody: `{"key":"K","value":"V","env":"staging"}`,
		},
		{
			name: "secret unset", method: "DELETE",
			unnarrowed: "/v1/apps/web/secrets/K", viaRoute: "/v1/apps/web/secrets/K/env/staging",
			viaOldForm: "/v1/apps/web/secrets/K?env=staging",
		},
		{
			name: "addon install", method: "POST",
			unnarrowed: "/v1/addons", viaRoute: "/v1/addons/env/staging", viaOldForm: "/v1/addons",
			body: `{"type":"postgres","confirm":true}`, oldBody: `{"type":"postgres","confirm":true,"env":"staging"}`,
		},
		{
			name: "addon backup", method: "POST",
			unnarrowed: "/v1/addons/backup", viaRoute: "/v1/addons/backup/env/staging", viaOldForm: "/v1/addons/backup",
			body: `{"addon":"postgres","app":"web"}`, oldBody: `{"addon":"postgres","app":"web","env":"staging"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each call gets a FRESH control plane. These are mutations against fakes with seeded ids
			// and a fixed clock, so running two of them at the same handler produces two different
			// release ids for the same request — and the claim being made here is that the route form
			// and the old form are the SAME REQUEST, which is only checkable from the same start.
			send := func(method, path, body string) *httptest.ResponseRecorder {
				t.Helper()
				h, d := newProvisionedAPI(t)
				d.SetPolicy(cp.DefaultPolicy().
					With(cp.GuardrailAppDeploy, cp.DispositionAllow).
					With(cp.GuardrailScaleToZero, cp.DispositionAllow).
					With(cp.GuardrailExposePublic, cp.DispositionAllow).
					With(cp.GuardrailRollback, cp.DispositionAllow).
					With(cp.GuardrailAddonInstall, cp.DispositionAllow))
				if err := d.CreateEnvironment(context.Background(), "staging", "burrow-apps-staging"); err != nil {
					t.Fatalf("CreateEnvironment: %v", err)
				}
				return do(h, method, path, token, body)
			}

			// Naming no environment at all is ambiguous with two registered — the refusal that makes
			// the rest of this test mean something.
			if rr := send(tc.method, tc.unnarrowed, tc.body); !strings.Contains(rr.Body.String(), "ambiguous_environment") {
				t.Fatalf("unnarrowed call = %d %s, want the ambiguous-environment refusal", rr.Code, rr.Body.String())
			}
			// Through the route it is not ambiguous, so the path's environment reached the engine.
			viaRoute := send(tc.method, tc.viaRoute, tc.body)
			if strings.Contains(viaRoute.Body.String(), "ambiguous_environment") {
				t.Fatalf("the route's environment did not reach the engine: %d %s", viaRoute.Code, viaRoute.Body.String())
			}
			// And an older client's form is served, identically.
			viaOldForm := send(tc.method, tc.viaOldForm, tc.oldBody)
			if viaOldForm.Code != viaRoute.Code || viaOldForm.Body.String() != viaRoute.Body.String() {
				t.Errorf("an older client's form = %d %s, want the same answer as the route form %d %s",
					viaOldForm.Code, viaOldForm.Body.String(), viaRoute.Code, viaRoute.Body.String())
			}
			// An environment nobody registered is the engine's own 404 rather than a missing route,
			// which is what keeps a typo from reading as version skew on the client side.
			ghost := strings.Replace(tc.viaRoute, "/env/staging", "/env/ghost", 1)
			if rr := send(tc.method, ghost, tc.body); !strings.Contains(rr.Body.String(), "unknown environment") {
				t.Errorf("an unregistered environment = %d %s, want the engine's unknown-environment answer", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestDestinationRidesTheRouteOnInstallAndBackup is the other narrowing in this sweep, and it is not
// an environment: it says whether what the call produces ever leaves the cluster.
//
// The lever is that no object-storage provider is registered here, so a destination that reaches the
// engine is refused BY NAME — and the unnarrowed call succeeds, which is precisely the older control
// plane's behaviour this replaces: an instance created with no archiving, or a dump written to a
// volume next to the database it insures, either one reported as done.
func TestDestinationRidesTheRouteOnInstallAndBackup(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unnarrowed succeeds; viaRoute and viaOldForm must both be refused, naming the destination.
		unnarrowed string
		viaRoute   string
		viaOldForm string
		body       string
		oldBody    string
	}{
		{
			name:       "addon install archiving to an object store",
			unnarrowed: "/v1/addons", viaRoute: "/v1/addons/archive-destination/b2", viaOldForm: "/v1/addons",
			body: `{"type":"postgres","confirm":true}`, oldBody: `{"type":"postgres","confirm":true,"archive_destination":"b2"}`,
		},
		{
			name:       "addon backup written to an object store",
			unnarrowed: "/v1/addons/backup", viaRoute: "/v1/addons/backup/destination/b2", viaOldForm: "/v1/addons/backup",
			body: `{"addon":"postgres","app":"web"}`, oldBody: `{"addon":"postgres","app":"web","destination":"b2"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			send := func(path, body string) *httptest.ResponseRecorder {
				t.Helper()
				h, _, _ := newProviderAPI(t)
				if rr := do(h, "POST", "/v1/addons", token, `{"type":"postgres","confirm":true}`); rr.Code != 200 {
					t.Fatalf("install addon = %d %s", rr.Code, rr.Body.String())
				}
				return do(h, "POST", path, token, body)
			}
			// Unnarrowed, it goes through — which is what an older control plane does with a
			// destination it never saw, and why the destination cannot be a parameter.
			if rr := send(tc.unnarrowed, tc.body); rr.Code != http.StatusOK {
				t.Fatalf("unnarrowed call = %d %s, want it served", rr.Code, rr.Body.String())
			}
			viaRoute := send(tc.viaRoute, tc.body)
			if !strings.Contains(viaRoute.Body.String(), "b2") {
				t.Fatalf("the route's destination did not reach the engine: %d %s", viaRoute.Code, viaRoute.Body.String())
			}
			viaOldForm := send(tc.viaOldForm, tc.oldBody)
			if viaOldForm.Code != viaRoute.Code || viaOldForm.Body.String() != viaRoute.Body.String() {
				t.Errorf("an older client's form = %d %s, want the same answer as the route form %d %s",
					viaOldForm.Code, viaOldForm.Body.String(), viaRoute.Code, viaRoute.Body.String())
			}
		})
	}
}

// TestInstallCarriesBothNarrowingsInOneRoute: two narrowings on one call are two segments in a fixed
// order, so the combinations stay enumerable and both values arrive.
func TestInstallCarriesBothNarrowingsInOneRoute(t *testing.T) {
	h, d := newProvisionedAPI(t)
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailAddonInstall, cp.DispositionAllow))
	if err := d.CreateEnvironment(context.Background(), "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	// staging resolves (so the environment arrived) and b2 is refused by name (so the destination
	// did). One request, both narrowings, neither silently dropped.
	rr := do(h, "POST", "/v1/addons/env/staging/archive-destination/b2", token, `{"type":"postgres","confirm":true}`)
	body := rr.Body.String()
	if strings.Contains(body, "ambiguous_environment") || strings.Contains(body, "unknown environment") {
		t.Fatalf("the environment did not reach the engine: %d %s", rr.Code, body)
	}
	if !strings.Contains(body, "b2") {
		t.Fatalf("the archive destination did not reach the engine: %d %s", rr.Code, body)
	}
}

// TestBackupsListingEnvironmentRidesTheRouteAndTheQueryStillWorks. This is the one READ in the sweep
// that follows the write rule, because its answer is an argument to a restore rather than something a
// person reads. The `addon` and `app` filters stay on the query and must still work beside it.
func TestBackupsListingEnvironmentRidesTheRouteAndTheQueryStillWorks(t *testing.T) {
	h, _, _ := newProviderAPI(t)
	if rr := do(h, "POST", "/v1/addons", token, `{"type":"postgres","confirm":true}`); rr.Code != 200 {
		t.Fatalf("install addon = %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(h, "POST", "/v1/addons/backup", token, `{"addon":"postgres","app":"web"}`); rr.Code != 200 {
		t.Fatalf("backup = %d %s", rr.Code, rr.Body.String())
	}

	// The backup was taken on the default environment, so the default environment's list has it.
	if rr := do(h, "GET", "/v1/addons/backups/env/prod?addon=postgres", token, ""); !strings.Contains(rr.Body.String(), `"id"`) {
		t.Errorf("the default environment's listing is empty: %d %s", rr.Code, rr.Body.String())
	}
	// Another environment's is not this one's — the filter reached the engine through the route.
	if rr := do(h, "GET", "/v1/addons/backups/env/staging?addon=postgres", token, ""); strings.Contains(rr.Body.String(), `"id"`) {
		t.Errorf("the route's environment did not reach the engine; it listed another environment's backups: %s", rr.Body.String())
	}
	// An older client's query form answers identically to the route form.
	viaRoute := do(h, "GET", "/v1/addons/backups/env/prod?addon=postgres&app=web", token, "")
	viaQuery := do(h, "GET", "/v1/addons/backups?addon=postgres&app=web&env=prod", token, "")
	if viaQuery.Code != viaRoute.Code || viaQuery.Body.String() != viaRoute.Body.String() {
		t.Errorf("an older client's form = %d %s, want the same answer as the route form %d %s",
			viaQuery.Code, viaQuery.Body.String(), viaRoute.Code, viaRoute.Body.String())
	}
}

// TestReadsThatOnlyInformAReaderKeptTheQueryParameter is the deliberate other half of the decision,
// asserted on the server so that adding an `/env/{env}` route to one of these later is a decision
// somebody makes rather than one that happens.
//
// A dropped narrowing on a read changes an answer, not the cluster, and every write it might lead to
// is refused on its own. So these keep the query form, and the route form is not a route.
func TestReadsThatOnlyInformAReaderKeptTheQueryParameter(t *testing.T) {
	h, _, _ := newAPI(t)
	for _, r := range []struct{ viaRoute, viaQuery string }{
		{"/v1/apps/web/status/env/staging", "/v1/apps/web/status?env=staging"},
		{"/v1/apps/web/logs/env/staging", "/v1/apps/web/logs?env=staging"},
		{"/v1/apps/env/staging", "/v1/apps?env=staging"},
		{"/v1/apps/web/reachability/env/staging", "/v1/apps/web/reachability?env=staging"},
		{"/v1/apps/web/config/env/staging", "/v1/apps/web/config?env=staging"},
		// The secrets read is 405 rather than 404: the path exists for the WRITE, which does put its
		// environment in the route. That is the split this test is about, on one path.
		{"/v1/apps/web/secrets/env/staging", "/v1/apps/web/secrets?env=staging"},
		{"/v1/addons/backup-health/env/staging", "/v1/addons/backup-health?env=staging&addon=postgres"},
	} {
		if rr := do(h, "GET", r.viaRoute, token, ""); rr.Code == http.StatusOK {
			t.Errorf("GET %s was served: this read carries its environment on the query, and turning "+
				"it into a route is a decision with a reason attached", r.viaRoute)
		}
		// And the query form is the one that works, so an older control plane answers it rather than
		// 404ing — which is the whole reason a read is allowed to keep it.
		if rr := do(h, "GET", r.viaQuery, token, ""); rr.Code == http.StatusNotFound && strings.Contains(rr.Body.String(), "unknown_operation") {
			t.Errorf("GET %s is not a route at all: %s", r.viaQuery, rr.Body.String())
		}
	}
}

// TestUnknownRequestFieldIsRefusedRatherThanDropped is the BODY half of the whole class, and the
// only part of it that generalizes to a field nobody has invented yet.
//
// Before this, an unrecognised field in a request body was discarded and the request proceeded — the
// way `env` reached the detach and the restore in #507. Now it is refused, with the version-skew
// wording and code, so a newer client's field cannot be half-honoured by an older control plane. It
// protects from this release forward and repairs nothing already deployed, which is why the routes
// exist as well.
func TestUnknownRequestFieldIsRefusedRatherThanDropped(t *testing.T) {
	h, _, _ := newAPIVersion(t, "v0.15.0")

	rr := doClient(h, "POST", "/v1/apps/web/deploy", token, "burrow", "v0.16.0",
		`{"image":"ghcr.io/x:1","replicas":1,"env_scope":"staging"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s, want it refused", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`"code":"unknown_field"`, // an agent and a --json caller branch on this
		"env_scope",              // the field, so the reader knows which one
		"nothing was done",       // and that the deploy did not half-happen
		"v0.15.0", "v0.16.0",     // both versions
		"burrow cluster upgrade", // and the remedy
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal is missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"server_version":"v0.15.0"`) {
		t.Errorf("the refusal does not carry the server version structurally: %s", body)
	}
}

// TestAKnownBodyStillDecodes keeps the refusal from becoming a wall. A body of fields this control
// plane knows is served, and so is a body from an OLDER client, which sends FEWER fields and never
// unknown ones — burrowd stays the compatibility anchor (ADR-0039 §2).
func TestAKnownBodyStillDecodes(t *testing.T) {
	h, _, _ := newAPIVersion(t, "v0.15.0")
	for _, body := range []string{
		`{"image":"ghcr.io/x:1","replicas":1,"command":["./serve"],"metrics_port":9090,"confirm":true,"env":"prod"}`,
		`{"image":"ghcr.io/x:1"}`, // an older client, before most of those fields existed
	} {
		if rr := do(h, "POST", "/v1/apps/web/deploy", token, body); strings.Contains(rr.Body.String(), "unknown_field") {
			t.Errorf("a body of known fields was refused: %s -> %s", body, rr.Body.String())
		}
	}
	// A body that is not JSON at all keeps the ordinary invalid-body answer: it is a broken request,
	// not a version gap, and sending its author to upgrade the control plane would be a lie.
	rr := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":`)
	if !strings.Contains(rr.Body.String(), `"code":"invalid"`) {
		t.Errorf("malformed JSON = %s, want the ordinary invalid-body refusal", rr.Body.String())
	}
}
