// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
)

// Issue #485 is issue #472's defect on the calls that destroy things: a parameter that narrows the
// scope of a write is silently dropped by an older control plane, and the write happens anyway,
// wider than asked. These tests pin the four cases that lose data — the add-on removal's data
// disposition, and the environment on the app delete, the add-on detach and the add-on restore.
//
// Each is checked in both directions, because both matter and they are not the same property:
//
//   - A NEWER CLIENT AGAINST AN OLDER CONTROL PLANE must be REFUSED, and the refusal must carry the
//     version handshake's own sentence. The fake below is built so that a fallback would be visible:
//     it serves the OLD routes, so a client that quietly retried the unnarrowed form would get a 200
//     and destroy exactly what these tests say it must not.
//   - An OLDER CLIENT AGAINST A NEWER CONTROL PLANE must keep working, unchanged. That half lives in
//     the control-plane package, where the real routes are (see api_test.go), because it is the
//     server's promise: burrowd is the compatibility anchor (ADR-0039 §2).

// preScopeControlPlane answers like a control plane from before the scope moved into the route: it
// serves the four unnarrowed routes and gives every other path the structured unknown-operation
// refusal (ADR-0039), naming its own version and the caller's. Every request is recorded, so a test
// can assert that the destructive call was never made.
func preScopeControlPlane(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(body map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(body) }
	}
	// The removal's legacy answer is the one that matters most: this control plane keeps the volume
	// only because it is asked to in a parameter it does not, in the story being told, understand.
	mux.HandleFunc("DELETE /v1/addons/{name}", ok(map[string]any{"name": "burrow-postgres", "type": "postgres", "data_deleted": true}))
	mux.HandleFunc("DELETE /v1/apps/{app}", ok(map[string]any{"app": "web"}))
	mux.HandleFunc("POST /v1/addons/detach", ok(map[string]any{"addon": "postgres", "app": "web"}))
	mux.HandleFunc("POST /v1/addons/restore", ok(map[string]any{"addon": "postgres", "app": "web"}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.Path)
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "this control plane (v0.13.4) does not recognize " + r.Method + " " + r.URL.Path +
				"; if your burrow CLI (v0.15.0) is newer, ask an operator to run `burrow cluster upgrade` to update the control plane",
			"code":           "unknown_operation",
			"server_version": "v0.13.4",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newerClient is a client from after the fix, talking to whatever server the test gives it.
func newerClient(url string) *client.Client { return client.NewClientVersion(url, "tok", "v0.15.0") }

// scopeRefused asserts the call was refused as version skew rather than performed, and that the
// refusal is the structured kind an agent and a --json caller already branch on.
func scopeRefused(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the call succeeded against a control plane that cannot express the scope it was given; it must be refused, never performed wider")
	}
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %T (%v), want a structured *client.APIError like every other refusal", err, err)
	}
	if api.Code != client.CodeScopeUnsupported {
		t.Fatalf("code = %q, want %q", api.Code, client.CodeScopeUnsupported)
	}
	if api.ServerVersion != "v0.13.4" {
		t.Errorf("ServerVersion = %q, want the version the control plane reported", api.ServerVersion)
	}
	// Both versions and the upgrade come from the control plane's own sentence, relayed verbatim.
	for _, want := range append(wants, "v0.13.4", "v0.15.0", "burrow cluster upgrade") {
		if !strings.Contains(api.Message, want) {
			t.Errorf("refusal is missing %q:\n%s", want, api.Message)
		}
	}
}

// notReached asserts none of the recorded requests is the unnarrowed, destructive one.
func notReached(t *testing.T, seen []string, request string) {
	t.Helper()
	for _, got := range seen {
		if got == request {
			t.Fatalf("the client fell back to %q, which is the wider write it was supposed to refuse: %v", request, seen)
		}
	}
}

// TestAddonRemoveRefusedRatherThanDestroyingDataItWasAskedToKeep is the case the whole change exists
// for, and its name is what it prevents.
//
// `delete_data` inverted the default (issue #323): before it, a removal destroyed the data volume
// unconditionally; now the volume survives unless the flag is passed. So the request a current
// client sends to MEAN "KEEP MY DATA" is the empty one — and an older control plane answers the
// empty one by destroying the volume, with no final backup, because it takes none and there is
// nothing for `skip_final_backup`'s absence to buy. This is the only case in the audit that destroys
// data on a request that asked to preserve it, and it must end in a refusal with the add-on still
// standing.
func TestAddonRemoveRefusedRatherThanDestroyingDataItWasAskedToKeep(t *testing.T) {
	var seen []string
	srv := preScopeControlPlane(t, &seen)

	res, err := newerClient(srv.URL).RemoveAddon(context.Background(), "burrow-postgres", client.RemoveAddonOptions{Confirm: true})
	if err == nil {
		t.Fatalf("the removal reported success (%+v); against this control plane it destroyed the data volume the caller asked to keep", res)
	}
	scopeRefused(t, err, "nothing was removed", "burrow-postgres", "KEEP")
	notReached(t, seen, "DELETE /v1/addons/burrow-postgres")
}

// TestAddonRemoveDeleteDataRefusedWhenNoFinalBackupIsPossible is the same route in the other
// direction, and it is refused too rather than let through. An older control plane WOULD destroy the
// volume, which is what was asked — but it cannot take the final backup ADR-0064 §5 makes first, and
// the caller's own client promises that backup and abandons the removal if it does not reach the
// store. Destroying the data with no copy off the cluster is not the operation that was requested.
func TestAddonRemoveDeleteDataRefusedWhenNoFinalBackupIsPossible(t *testing.T) {
	var seen []string
	srv := preScopeControlPlane(t, &seen)

	_, err := newerClient(srv.URL).RemoveAddon(context.Background(), "burrow-postgres", client.RemoveAddonOptions{DeleteData: true, Confirm: true})
	scopeRefused(t, err, "nothing was removed", "final backup", "burrow-postgres")
	notReached(t, seen, "DELETE /v1/addons/burrow-postgres")
}

// TestDeleteAppInEnvironmentRefusedRatherThanDeletingProduction: naming staging against a control
// plane that drops the environment deletes the app of that name in the DEFAULT environment —
// workload, routing and release history — and reports it as staging's.
func TestDeleteAppInEnvironmentRefusedRatherThanDeletingProduction(t *testing.T) {
	var seen []string
	srv := preScopeControlPlane(t, &seen)

	err := newerClient(srv.URL).DeleteApp(context.Background(), "web", "staging", true)
	scopeRefused(t, err, "nothing was deleted", `"web"`, `"staging"`, "release history")
	notReached(t, seen, "DELETE /v1/apps/web")
}

// TestDetachAddonInEnvironmentRefusedRatherThanDroppingProductionsDatabase. The environment is a
// BODY field here rather than a query parameter, which changes nothing: `decode` reads the body with
// a plain json.Decoder, so an unknown field is dropped exactly as an unknown parameter is.
func TestDetachAddonInEnvironmentRefusedRatherThanDroppingProductionsDatabase(t *testing.T) {
	var seen []string
	srv := preScopeControlPlane(t, &seen)

	err := newerClient(srv.URL).DetachAddon(context.Background(), "postgres", "web", "staging", true)
	scopeRefused(t, err, "nothing was detached", `"web"`, `"staging"`)
	notReached(t, seen, "POST /v1/addons/detach")
}

// TestRestoreAddonInEnvironmentRefusedRatherThanOverwritingProduction. The worst of the three
// environment cases in one way: it does not remove a database, it replaces a live one's contents
// with another environment's dump.
func TestRestoreAddonInEnvironmentRefusedRatherThanOverwritingProduction(t *testing.T) {
	var seen []string
	srv := preScopeControlPlane(t, &seen)

	err := newerClient(srv.URL).RestoreAddon(context.Background(), "postgres", "web", "backup-1", "staging", true)
	scopeRefused(t, err, "nothing was restored", `"web"`, `"staging"`)
	notReached(t, seen, "POST /v1/addons/restore")
}

// TestDestructiveScopeRoutesOnAMatchedPair is the pair that must be untouched: a current client
// against a current control plane. Each narrowing rides the route, and the call goes through.
func TestDestructiveScopeRoutesOnAMatchedPair(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "burrow-postgres", "data_deleted": true, "app": "web"})
	}))
	defer srv.Close()
	c := newerClient(srv.URL)
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		call       func() error
		wantMethod string
		wantPath   string
	}{
		{"remove keeping the data", func() error {
			_, err := c.RemoveAddon(ctx, "burrow-postgres", client.RemoveAddonOptions{Confirm: true})
			return err
		}, "DELETE", "/v1/addons/burrow-postgres/data/keep"},
		{"remove destroying the data", func() error {
			_, err := c.RemoveAddon(ctx, "burrow-postgres", client.RemoveAddonOptions{DeleteData: true, Confirm: true})
			return err
		}, "DELETE", "/v1/addons/burrow-postgres/data/delete"},
		{"delete an app in an environment", func() error {
			return c.DeleteApp(ctx, "web", "staging", true)
		}, "DELETE", "/v1/apps/web/env/staging"},
		{"detach in an environment", func() error {
			return c.DetachAddon(ctx, "postgres", "web", "staging", true)
		}, "POST", "/v1/addons/detach/env/staging"},
		{"restore in an environment", func() error {
			return c.RestoreAddon(ctx, "postgres", "web", "backup-1", "staging", true)
		}, "POST", "/v1/addons/restore/env/staging"},
		// A statement carries its environment the same way, and for a sharper version of the same
		// reason: a dropped environment runs the caller's SQL against another instance's database of
		// the same name (ADR-0087 §1).
		{"a statement in an environment", func() error {
			_, err := c.AddonSQL(ctx, "postgres", "web", "staging", "select 1", false)
			return err
		}, "POST", "/v1/addons/sql/env/staging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotMethod, gotPath, gotBody = "", "", ""
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath {
				t.Errorf("request = %s %s, want %s %s", gotMethod, gotPath, tc.wantMethod, tc.wantPath)
			}
			// The environment does not ALSO ride the body: one place carries the scope, so there is
			// no second copy for a mismatch to hide in.
			if strings.Contains(gotBody, "staging") {
				t.Errorf("the environment is in the route, so it should not be repeated in the body: %s", gotBody)
			}
		})
	}
}

// TestDestructiveScopeWithNoEnvironmentKeepsTheUnnarrowedRoute keeps the refusal narrow. An empty
// environment narrows nothing — it means the default environment, which every control plane has — so
// these calls stay on the routes they have always used and are not turned into a skew error.
func TestDestructiveScopeWithNoEnvironmentKeepsTheUnnarrowedRoute(t *testing.T) {
	var seen []string
	srv := preScopeControlPlane(t, &seen)
	c := newerClient(srv.URL)
	ctx := context.Background()

	if err := c.DeleteApp(ctx, "web", "", true); err != nil {
		t.Errorf("delete without an environment: %v", err)
	}
	if err := c.DetachAddon(ctx, "postgres", "web", "", true); err != nil {
		t.Errorf("detach without an environment: %v", err)
	}
	if err := c.RestoreAddon(ctx, "postgres", "web", "backup-1", "", true); err != nil {
		t.Errorf("restore without an environment: %v", err)
	}
	for _, want := range []string{"DELETE /v1/apps/web", "POST /v1/addons/detach", "POST /v1/addons/restore"} {
		if !slicesContains(seen, want) {
			t.Errorf("%q was not sent; requests were %v", want, seen)
		}
	}
}

// TestDestructiveScopeLeavesRealNotFoundAlone: an unknown environment is the control plane ANSWERING,
// not a missing route. Dressing it up as version skew would send an operator to upgrade over a typo.
func TestDestructiveScopeLeavesRealNotFoundAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": `delete app: unknown environment "stagng"`,
			"code":  "not_found",
		})
	}))
	defer srv.Close()

	err := newerClient(srv.URL).DeleteApp(context.Background(), "web", "stagng", true)
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %T (%v), want the control plane's own refusal", err, err)
	}
	if api.Code != "not_found" || !strings.Contains(api.Message, "unknown environment") {
		t.Errorf("error = %+v, want the unknown-environment refusal unchanged", api)
	}
}

// TestDestructiveScopeRefusedByPreHandshakeControlPlane covers a control plane older than the
// handshake itself, whose 404 carries no structured code and cannot name its own version. The client
// supplies the remedy rather than relaying one, and still refuses.
func TestDestructiveScopeRefusedByPreHandshakeControlPlane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")

	_, err := c.RemoveAddon(context.Background(), "burrow-postgres", client.RemoveAddonOptions{Confirm: true})
	var api *client.APIError
	if !errors.As(err, &api) || api.Code != client.CodeScopeUnsupported {
		t.Fatalf("error = %v, want a %s refusal", err, client.CodeScopeUnsupported)
	}
	for _, want := range []string{"nothing was removed", "burrow cluster upgrade"} {
		if !strings.Contains(api.Message, want) {
			t.Errorf("refusal is missing %q:\n%s", want, api.Message)
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
