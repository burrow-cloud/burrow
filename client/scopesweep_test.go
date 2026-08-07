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

// Issue #485's rule, on the calls #507 did not reach: A REQUEST PARAMETER THAT NARROWS THE SCOPE OF A
// WRITE BELONGS IN THE ROUTE. #507 took the four that destroy data; these are the rest of the writes,
// where an older control plane drops the narrowing and performs the write somewhere else — into
// production's namespace, onto production's instance, or onto a disk inside the cluster instead of an
// object store.
//
// Each is checked in both directions, because they are different properties:
//
//   - A NEWER CLIENT AGAINST AN OLDER CONTROL PLANE must be REFUSED. preSweepControlPlane below is
//     built so a fallback would be visible: it SERVES the old routes, so a client that quietly
//     retried the unnarrowed form would get a 200 and perform exactly the write these tests say it
//     must not.
//   - An OLDER CLIENT AGAINST A NEWER CONTROL PLANE must keep working, on the old routes with the old
//     meanings. That half is the server's promise and lives in controlplane/api/scopesweep_test.go,
//     because burrowd is the compatibility anchor (ADR-0039 §2).

// preSweepControlPlane answers like a control plane from before these narrowings moved into the
// route: it serves the UNNARROWED routes and gives every other path the structured unknown-operation
// refusal (ADR-0039), naming its own version and the caller's. Every request is recorded so a test
// can assert the wider write was never made.
func preSweepControlPlane(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(okBody(r)) }
	for _, route := range []string{
		"POST /v1/apps/{app}/deploy",
		"POST /v1/apps/{app}/rollback",
		"POST /v1/apps/{app}/scale",
		"POST /v1/apps/{app}/expose",
		"POST /v1/apps/{app}/unexpose",
		"POST /v1/apps/{app}/secrets",
		"DELETE /v1/apps/{app}/secrets/{key}",
		"POST /v1/addons",
		"POST /v1/addons/backup",
		"GET /v1/addons/backups",
	} {
		mux.HandleFunc(route, ok)
	}
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

// okBody is a success body wide enough for every call in this file. The backups listing gets its own
// shape because `backups` names a list there and an object on an Addon, and one map cannot be both.
func okBody(r *http.Request) map[string]any {
	if strings.HasPrefix(r.URL.Path, "/v1/addons/backups") {
		return map[string]any{"backups": []any{}}
	}
	return map[string]any{
		"app": "web", "name": "burrow-postgres", "type": "postgres",
		"backup": map[string]any{"id": "b-1"}, "release": map[string]any{"id": "r-1"},
	}
}

// TestRemainingScopeNarrowingsAreRefusedByAnOlderControlPlane is the whole point of the change, one
// row per call. The "wider" column is the request the client must NOT fall back to, and the fake
// serves it, so a regression shows up as a passing call rather than as a missing route.
func TestRemainingScopeNarrowingsAreRefusedByAnOlderControlPlane(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(*client.Client) error
		// wider is the unnarrowed request an older control plane would have served.
		wider string
		// wants are phrases the refusal must carry: what was not done, and what would have been.
		wants []string
	}{
		{
			name: "deploy replaces what is running in the wrong environment",
			call: func(c *client.Client) error {
				_, err := c.Deploy(ctx, "web", client.DeployRequest{Env: "staging", Image: "ghcr.io/x:1", Replicas: 1, Confirm: true})
				return err
			},
			wider: "POST /v1/apps/web/deploy",
			wants: []string{"nothing was deployed", `"web"`, `"staging"`},
		},
		{
			name: "rollback replaces what is running in the wrong environment",
			call: func(c *client.Client) error {
				_, err := c.Rollback(ctx, "web", "staging", client.RollbackOptions{Confirm: true})
				return err
			},
			wider: "POST /v1/apps/web/rollback",
			wants: []string{"nothing was rolled back", `"web"`, `"staging"`},
		},
		{
			name: "scale resizes the wrong environment, and zero stops it serving",
			call: func(c *client.Client) error {
				_, err := c.Scale(ctx, "web", "staging", 0, true)
				return err
			},
			wider: "POST /v1/apps/web/scale",
			wants: []string{"nothing was scaled", `"web"`, `"staging"`, "0 replicas"},
		},
		{
			name: "expose points a hostname at the wrong environment's workload",
			call: func(c *client.Client) error {
				_, err := c.Expose(ctx, "web", "staging", "staging.example.com", 8080, true, "", true)
				return err
			},
			wider: "POST /v1/apps/web/expose",
			wants: []string{"nothing was published", `"staging.example.com"`, `"web"`, `"staging"`},
		},
		{
			name:  "unexpose takes the wrong environment's routing down",
			call:  func(c *client.Client) error { return c.Unexpose(ctx, "web", "staging") },
			wider: "POST /v1/apps/web/unexpose",
			wants: []string{"nothing was unpublished", `"web"`, `"staging"`},
		},
		{
			name: "a secret is written into the wrong environment and cannot be unwritten",
			call: func(c *client.Client) error {
				return c.SetSecret(ctx, "web", "staging", "STRIPE_KEY", "sk_live_dontleakme", false)
			},
			wider: "POST /v1/apps/web/secrets",
			wants: []string{"nothing was written", `"STRIPE_KEY"`, `"web"`, `"staging"`},
		},
		{
			name: "a secret is removed from the wrong environment",
			call: func(c *client.Client) error {
				return c.UnsetSecret(ctx, "web", "staging", "STRIPE_KEY", false)
			},
			wider: "DELETE /v1/apps/web/secrets/STRIPE_KEY",
			wants: []string{"nothing was removed", `"STRIPE_KEY"`, `"staging"`},
		},
		{
			name: "an add-on instance is created for the wrong environment",
			call: func(c *client.Client) error {
				_, err := c.InstallAddon(ctx, "postgres", "staging", client.InstallAddonOptions{Confirm: true})
				return err
			},
			wider: "POST /v1/addons",
			wants: []string{"nothing was installed", "postgres", `"staging"`},
		},
		{
			name: "an add-on instance is created with no write-ahead-log archiving",
			call: func(c *client.Client) error {
				_, err := c.InstallAddon(ctx, "postgres", "", client.InstallAddonOptions{Confirm: true, ArchiveDestination: "b2"})
				return err
			},
			wider: "POST /v1/addons",
			wants: []string{"nothing was installed", "NO archiving", `"b2"`},
		},
		{
			name: "a backup is taken from the wrong environment's instance",
			call: func(c *client.Client) error {
				_, err := c.BackupAddon(ctx, "postgres", "web", "staging", "")
				return err
			},
			wider: "POST /v1/addons/backup",
			wants: []string{"nothing was backed up", `"web"`, `"staging"`},
		},
		{
			name: "a backup never leaves the cluster but is recorded as a completed backup",
			call: func(c *client.Client) error {
				_, err := c.BackupAddon(ctx, "postgres", "web", "", "b2")
				return err
			},
			wider: "POST /v1/addons/backup",
			wants: []string{"nothing was backed up", "inside the cluster", `"b2"`},
		},
		{
			// The one READ that follows the write rule: its answer is an argument to a restore.
			name: "the backups listing is the picker for a restore, so it is refused too",
			call: func(c *client.Client) error {
				_, err := c.Backups(ctx, "postgres", "web", "staging")
				return err
			},
			wider: "GET /v1/addons/backups",
			wants: []string{"nothing to show", "restored over a live database", `"staging"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			srv := preSweepControlPlane(t, &seen)

			err := tc.call(client.NewClientVersion(srv.URL, "tok", "v0.15.0"))
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
			// Both versions and the upgrade come from the control plane's own sentence, verbatim.
			for _, want := range append(tc.wants, "v0.13.4", "v0.15.0", "burrow cluster upgrade") {
				if !strings.Contains(api.Message, want) {
					t.Errorf("refusal is missing %q:\n%s", want, api.Message)
				}
			}
			for _, got := range seen {
				if got == tc.wider {
					t.Fatalf("the client fell back to %q, which is the wider write it was supposed to refuse: %v", tc.wider, seen)
				}
			}
		})
	}
}

// TestSecretRefusalNamesTheKeyAndNeverTheValue. The refusal is printed, logged by whatever is driving
// the CLI, and handed to an agent. A secret set is the one call in this sweep that carries a value,
// so the sentence it fails with has to name the key and stop there (ADR-0029).
func TestSecretRefusalNamesTheKeyAndNeverTheValue(t *testing.T) {
	var seen []string
	srv := preSweepControlPlane(t, &seen)

	err := client.NewClientVersion(srv.URL, "tok", "v0.15.0").
		SetSecret(context.Background(), "web", "staging", "STRIPE_KEY", "sk_live_dontleakme", false)
	if err == nil {
		t.Fatal("the secret set must be refused against a control plane that cannot aim it")
	}
	if strings.Contains(err.Error(), "sk_live_dontleakme") {
		t.Errorf("the refusal carries the secret VALUE:\n%s", err)
	}
	if !strings.Contains(err.Error(), "STRIPE_KEY") {
		t.Errorf("the refusal does not name the key, so a reader cannot tell which secret it is about:\n%s", err)
	}
}

// TestRemainingScopeRoutesOnAMatchedPair pins the wire shape a current client sends to a current
// control plane: the narrowing is in the PATH, once, and not repeated in the body.
func TestRemainingScopeRoutesOnAMatchedPair(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(b)
		_ = json.NewEncoder(w).Encode(okBody(r))
	}))
	defer srv.Close()
	c := client.NewClientVersion(srv.URL, "tok", "v0.15.0")
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		call       func() error
		wantMethod string
		wantPath   string
	}{
		{"deploy", func() error {
			_, err := c.Deploy(ctx, "web", client.DeployRequest{Env: "staging", Image: "ghcr.io/x:1", Replicas: 1})
			return err
		}, "POST", "/v1/apps/web/deploy/env/staging"},
		{"rollback", func() error {
			_, err := c.Rollback(ctx, "web", "staging", client.RollbackOptions{})
			return err
		}, "POST", "/v1/apps/web/rollback/env/staging"},
		{"scale", func() error {
			_, err := c.Scale(ctx, "web", "staging", 3, false)
			return err
		}, "POST", "/v1/apps/web/scale/env/staging"},
		{"publish", func() error {
			_, err := c.Publish(ctx, "web", client.PublishRequest{Env: "staging", Host: "s.example.com", Port: 80})
			return err
		}, "POST", "/v1/apps/web/publish/env/staging"},
		{"expose", func() error {
			_, err := c.Expose(ctx, "web", "staging", "s.example.com", 80, false, "", false)
			return err
		}, "POST", "/v1/apps/web/expose/env/staging"},
		{"unexpose", func() error {
			return c.Unexpose(ctx, "web", "staging")
		}, "POST", "/v1/apps/web/unexpose/env/staging"},
		{"secret set", func() error {
			return c.SetSecret(ctx, "web", "staging", "K", "V", false)
		}, "POST", "/v1/apps/web/secrets/env/staging"},
		{"secret unset", func() error {
			return c.UnsetSecret(ctx, "web", "staging", "K", false)
		}, "DELETE", "/v1/apps/web/secrets/K/env/staging"},
		{"addon install in an environment", func() error {
			_, err := c.InstallAddon(ctx, "postgres", "staging", client.InstallAddonOptions{})
			return err
		}, "POST", "/v1/addons/env/staging"},
		{"addon install archiving to a destination", func() error {
			_, err := c.InstallAddon(ctx, "postgres", "", client.InstallAddonOptions{ArchiveDestination: "b2"})
			return err
		}, "POST", "/v1/addons/archive-destination/b2"},
		{"addon install with both narrowings", func() error {
			_, err := c.InstallAddon(ctx, "postgres", "staging", client.InstallAddonOptions{ArchiveDestination: "b2"})
			return err
		}, "POST", "/v1/addons/env/staging/archive-destination/b2"},
		{"backup in an environment", func() error {
			_, err := c.BackupAddon(ctx, "postgres", "web", "staging", "")
			return err
		}, "POST", "/v1/addons/backup/env/staging"},
		{"backup to a destination", func() error {
			_, err := c.BackupAddon(ctx, "postgres", "web", "", "b2")
			return err
		}, "POST", "/v1/addons/backup/destination/b2"},
		{"backup with both narrowings", func() error {
			_, err := c.BackupAddon(ctx, "postgres", "web", "staging", "b2")
			return err
		}, "POST", "/v1/addons/backup/env/staging/destination/b2"},
		{"the backups listing", func() error {
			_, err := c.Backups(ctx, "postgres", "web", "staging")
			return err
		}, "GET", "/v1/addons/backups/env/staging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotMethod, gotPath, gotBody = "", "", ""
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath {
				t.Errorf("request = %s %s, want %s %s", gotMethod, gotPath, tc.wantMethod, tc.wantPath)
			}
			// The narrowing does not ALSO ride the body: one place carries the scope, so there is no
			// second copy for a mismatch to hide in.
			for _, leaked := range []string{`"env":"staging"`, `"archive_destination":"b2"`, `"destination":"b2"`} {
				if strings.Contains(gotBody, leaked) {
					t.Errorf("the narrowing is in the route, so it should not be repeated in the body (%s): %s", leaked, gotBody)
				}
			}
		})
	}
}

// TestRemainingScopeWithNoNarrowingKeepsTheUnnarrowedRoute keeps the refusal narrow. An empty
// environment means the default environment, which every control plane has, and an empty destination
// is one the server resolves — so these calls stay on the routes they have always used, against a
// control plane that has only those, and are not turned into a skew error.
func TestRemainingScopeWithNoNarrowingKeepsTheUnnarrowedRoute(t *testing.T) {
	var seen []string
	srv := preSweepControlPlane(t, &seen)
	c := client.NewClientVersion(srv.URL, "tok", "v0.15.0")
	ctx := context.Background()

	if _, err := c.Deploy(ctx, "web", client.DeployRequest{Image: "ghcr.io/x:1", Replicas: 1}); err != nil {
		t.Errorf("deploy with no environment: %v", err)
	}
	if _, err := c.Rollback(ctx, "web", "", client.RollbackOptions{}); err != nil {
		t.Errorf("rollback with no environment: %v", err)
	}
	if _, err := c.Scale(ctx, "web", "", 2, false); err != nil {
		t.Errorf("scale with no environment: %v", err)
	}
	if _, err := c.Expose(ctx, "web", "", "x.example.com", 80, false, "", false); err != nil {
		t.Errorf("expose with no environment: %v", err)
	}
	if err := c.Unexpose(ctx, "web", ""); err != nil {
		t.Errorf("unexpose with no environment: %v", err)
	}
	if err := c.SetSecret(ctx, "web", "", "K", "V", false); err != nil {
		t.Errorf("secret set with no environment: %v", err)
	}
	if err := c.UnsetSecret(ctx, "web", "", "K", false); err != nil {
		t.Errorf("secret unset with no environment: %v", err)
	}
	if _, err := c.InstallAddon(ctx, "postgres", "", client.InstallAddonOptions{}); err != nil {
		t.Errorf("addon install with no narrowing: %v", err)
	}
	if _, err := c.BackupAddon(ctx, "postgres", "web", "", ""); err != nil {
		t.Errorf("addon backup with no narrowing: %v", err)
	}
	if _, err := c.Backups(ctx, "postgres", "web", ""); err != nil {
		t.Errorf("backups listing with no environment: %v", err)
	}
	for _, want := range []string{
		"POST /v1/apps/web/deploy", "POST /v1/apps/web/rollback", "POST /v1/apps/web/scale",
		"POST /v1/apps/web/expose", "POST /v1/apps/web/unexpose", "POST /v1/apps/web/secrets",
		"DELETE /v1/apps/web/secrets/K", "POST /v1/addons", "POST /v1/addons/backup",
		"GET /v1/addons/backups",
	} {
		found := false
		for _, got := range seen {
			found = found || got == want
		}
		if !found {
			t.Errorf("%q was not sent; requests were %v", want, seen)
		}
	}
}

// TestRemainingScopeLeavesRealNotFoundAlone: an unknown environment is the control plane ANSWERING,
// not a missing route. Dressing it up as version skew would send an operator to upgrade over a typo.
func TestRemainingScopeLeavesRealNotFoundAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": `scale web: unknown environment "stagng"`,
			"code":  "not_found",
		})
	}))
	defer srv.Close()

	_, err := client.NewClientVersion(srv.URL, "tok", "v0.15.0").Scale(context.Background(), "web", "stagng", 2, true)
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %T (%v), want the control plane's own refusal", err, err)
	}
	if api.Code != "not_found" || !strings.Contains(api.Message, "unknown environment") {
		t.Errorf("error = %+v, want the unknown-environment refusal unchanged", api)
	}
}

// TestReadsThatOnlyInformAReaderKeepTheQueryParameter is the deliberate other half of the decision,
// and it is a test so that "we chose not to" cannot decay into "we forgot".
//
// A read answered one scope out misinforms a reader; it changes nothing, and against a control plane
// too old for named environments there is only one environment, so the answer is the only data there
// is rather than another environment's. Refusing these would also take away the commands an operator
// reaches for while diagnosing the skew. What makes it safe is that every WRITE they might lead to
// names its environment in its own route and is refused on its own — which the tests above assert.
//
// The exception is the backups listing, whose answer is an argument to a restore rather than
// something a person reads; it is above, with the writes.
func TestReadsThatOnlyInformAReaderKeepTheQueryParameter(t *testing.T) {
	var seen []string
	srv := preSweepControlPlane(t, &seen) // serves NO read routes, so every one of these 404s
	c := client.NewClientVersion(srv.URL, "tok", "v0.15.0")
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"status", func() error { _, err := c.Status(ctx, "web", "staging"); return err }},
		{"logs", func() error { _, err := c.Logs(ctx, "web", "staging", 10); return err }},
		{"the app list", func() error { _, err := c.Apps(ctx, "staging"); return err }},
		{"reachability", func() error { _, err := c.Reachability(ctx, "web", "staging"); return err }},
		{"the config listing", func() error { _, err := c.Config(ctx, "web", "staging"); return err }},
		{"the secret KEY listing", func() error { _, err := c.Secrets(ctx, "web", "staging"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("the fake serves no route for this read, so it must fail somehow")
			}
			var api *client.APIError
			if errors.As(err, &api) && api.Code == client.CodeScopeUnsupported {
				t.Fatalf("this read raises a scope refusal; it should carry its environment as a query "+
					"parameter and report the control plane's own answer:\n%s", api.Message)
			}
		})
	}
	// And the environment really is on the query for each of them, which is what makes an older
	// control plane answer at all rather than 404.
	for _, got := range seen {
		if strings.Contains(got, "/env/") {
			t.Errorf("a read put its environment in the route: %q", got)
		}
	}
}
