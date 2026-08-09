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

// Issue #462's parameter is the first to follow #485's rule WITHOUT narrowing scope, and these tests
// are why it follows it anyway. The database, the instance and the namespace are the same whatever the
// variable is called; what the name aims is which key of the app's Secret is overwritten. A control
// plane that predates it drops the name, writes DATABASE_URL over whatever the app kept there — for an
// app that asked for another name, a value this attach was never aimed at — and answers 200.
//
// So it rides the route, and the two halves are checked separately: the client refuses rather than
// falling back, and a current pair sends the name in the path and nowhere else.

// TestAttachNameIsRefusedByAnOlderControlPlane. The fake SERVES the unnarrowed route, so a client that
// quietly retried the wider form would get a 200 here and perform exactly the write this refuses.
func TestAttachNameIsRefusedByAnOlderControlPlane(t *testing.T) {
	var seen []string
	srv := preSweepControlPlane(t, &seen)

	_, err := client.NewClientVersion(srv.URL, "tok", "v0.15.0").
		AttachAddon(context.Background(), "postgres", "web", "", client.AttachAddonOptions{EnvKey: "PG_DSN"})
	if err == nil {
		t.Fatal("the attach succeeded against a control plane that cannot express the variable it was given")
	}
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %T (%v), want a structured *client.APIError", err, err)
	}
	if api.Code != client.CodeScopeUnsupported {
		t.Fatalf("code = %q, want %q", api.Code, client.CodeScopeUnsupported)
	}
	for _, want := range []string{"nothing was attached", "DATABASE_URL", `"PG_DSN"`, "v0.13.4", "v0.15.0", "burrow cluster upgrade"} {
		if !strings.Contains(api.Message, want) {
			t.Errorf("refusal is missing %q:\n%s", want, api.Message)
		}
	}
	for _, got := range seen {
		if got == "POST /v1/addons/attach" {
			t.Fatalf("the client fell back to the unnarrowed attach, which writes DATABASE_URL: %v", seen)
		}
	}
}

// TestAttachWithNoNameStaysOnTheOldRoute is the other half of the compatibility story. An empty name
// narrows nothing — it means "whatever this attachment already uses", which every control plane can
// answer — so the wire shape is byte-for-byte what clients have always sent, and no refusal is raised.
func TestAttachWithNoNameStaysOnTheOldRoute(t *testing.T) {
	// A control plane that serves ONLY the route attach has always had, and refuses everything else
	// the way an older one does.
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/v1/addons/attach" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown", "code": "unknown_operation"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "secret_key": "DATABASE_URL"})
	}))
	defer srv.Close()

	res, err := client.NewClientVersion(srv.URL, "tok", "v0.15.0").
		AttachAddon(context.Background(), "postgres", "web", "", client.AttachAddonOptions{})
	if err != nil {
		t.Fatalf("an attach that names no variable must still work against any control plane: %v", err)
	}
	if res.SecretKey != "DATABASE_URL" {
		t.Errorf("secret key = %q, want the unchanged default", res.SecretKey)
	}
	if len(seen) != 1 || seen[0] != "POST /v1/addons/attach" {
		t.Errorf("requests = %v, want the single unnarrowed attach", seen)
	}
}

// TestAttachNameRidesThePathAndNotTheBody pins the wire shape a current client sends to a current
// control plane: the name is in the path, once, and the body does not carry a second copy.
func TestAttachNameRidesThePathAndNotTheBody(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "secret_key": "PG_DSN"})
	}))
	defer srv.Close()

	if _, err := client.NewClientVersion(srv.URL, "tok", "v0.15.0").
		AttachAddon(context.Background(), "postgres", "web", "staging", client.AttachAddonOptions{EnvKey: "PG_DSN"}); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if gotPath != "/v1/addons/attach/env-key/PG_DSN" {
		t.Errorf("path = %q, want the name in the route", gotPath)
	}
	if strings.Contains(gotBody, "env_key") || strings.Contains(gotBody, "PG_DSN") {
		t.Errorf("body carries a second copy of the name, so a reader has two places to check: %s", gotBody)
	}
}
