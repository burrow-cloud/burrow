// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
)

// The guardrail name tier (ADR-0085) narrows a write to one app or add-on instance. These tests pin
// the property issue #472 was opened for: a control plane that cannot express that scope must
// REFUSE the call, never perform it one tier wider. The mechanism is that the name is part of the
// route, so an older control plane answers with the ADR-0039 unknown-operation refusal instead of
// ignoring a query parameter and writing the environment-wide entry.

// oldControlPlane answers like a control plane that predates the name tier: it serves the routes it
// has and gives every other path the structured unknown-operation refusal (ADR-0039), naming its own
// version and the caller's. It records every request so a test can assert that NOTHING was written.
func oldControlPlane(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/guard", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.rollback", "disposition": "allow", "description": "roll an application back"},
		}})
	})
	mux.HandleFunc("PUT /v1/guard/{code}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{}})
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.Path)
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "this control plane (v0.14.0-rc.2) does not recognize " + r.Method + " " + r.URL.Path +
				"; if your burrow CLI (v0.14.0-rc.5) is newer, ask an operator to run `burrow cluster upgrade` to update the control plane",
			"code":           "unknown_operation",
			"server_version": "v0.14.0-rc.2",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGuardNameScopeRefusedByOlderControlPlane is the reported bug. A newer client asks to deny one
// guardrail for ONE application; the control plane does not know the tier. The write must not land
// on the environment, and the refusal must carry the control plane's own sentence, which names both
// versions and the upgrade.
func TestGuardNameScopeRefusedByOlderControlPlane(t *testing.T) {
	var seen []string
	srv := oldControlPlane(t, &seen)
	c := client.NewClientVersion(srv.URL, "tok", "v0.14.0-rc.5")

	_, err := c.SetGuardrail(context.Background(), client.GuardScope{Env: "prod", Name: "burrowd-cloud"}, "", "app.rollback", "deny")
	if err == nil {
		t.Fatal("SetGuardrail succeeded against a control plane without the name tier; it must refuse rather than write one tier wider")
	}
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %T (%v), want a structured *client.APIError like every other refusal", err, err)
	}
	if api.Code != client.CodeScopeUnsupported {
		t.Errorf("code = %q, want %q", api.Code, client.CodeScopeUnsupported)
	}
	if api.ServerVersion != "v0.14.0-rc.2" {
		t.Errorf("ServerVersion = %q, want the version the control plane reported", api.ServerVersion)
	}
	for _, want := range []string{"nothing was written", "app.rollback", `"prod"`, "v0.14.0-rc.2", "v0.14.0-rc.5", "burrow cluster upgrade"} {
		if !strings.Contains(api.Message, want) {
			t.Errorf("refusal is missing %q:\n%s", want, api.Message)
		}
	}
	for _, req := range seen {
		if req == "PUT /v1/guard/app.rollback" {
			t.Fatalf("the client fell back to the environment-wide write: %v", seen)
		}
	}
}

// TestGuardNameScopeListRefusedByOlderControlPlane is the same gap on the read. Returning the
// environment's policy under a heading that names one app is how the write above went unnoticed:
// every row read as the app's own until an operator spotted a SOURCE of "inherited (default)" on a
// value that differed from the built-in default.
func TestGuardNameScopeListRefusedByOlderControlPlane(t *testing.T) {
	var seen []string
	srv := oldControlPlane(t, &seen)
	c := client.NewClientVersion(srv.URL, "tok", "v0.14.0-rc.5")

	gs, err := c.Guardrails(context.Background(), client.GuardScope{Env: "prod", Name: "burrowd-cloud"})
	if err == nil {
		t.Fatalf("Guardrails returned %d rows for an app the control plane cannot scope to; want a refusal", len(gs))
	}
	var api *client.APIError
	if !errors.As(err, &api) || api.Code != client.CodeScopeUnsupported {
		t.Fatalf("error = %v, want a %s refusal", err, client.CodeScopeUnsupported)
	}
	if !strings.Contains(api.Message, "burrow cluster upgrade") {
		t.Errorf("refusal does not name the upgrade:\n%s", api.Message)
	}
}

// TestGuardNameScopeRefusedByPreHandshakeControlPlane covers a control plane older than the
// handshake itself, whose 404 carries no structured code. It cannot name its version, so the client
// supplies the remedy rather than relaying one.
func TestGuardNameScopeRefusedByPreHandshakeControlPlane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	_, err := c.SetGuardrail(context.Background(), client.GuardScope{Env: "prod", Name: "website"}, "", "app.deploy", "deny")
	var api *client.APIError
	if !errors.As(err, &api) || api.Code != client.CodeScopeUnsupported {
		t.Fatalf("error = %v, want a %s refusal", err, client.CodeScopeUnsupported)
	}
	for _, want := range []string{"nothing was written", "burrow cluster upgrade"} {
		if !strings.Contains(api.Message, want) {
			t.Errorf("refusal is missing %q:\n%s", want, api.Message)
		}
	}
}

// TestGuardNameScopeMatchedPair is the pair that must be untouched: a current client against a
// current control plane. The name rides the path, the environment rides the query, and the call
// succeeds.
func TestGuardNameScopeMatchedPair(t *testing.T) {
	var gotMethod, gotPath, gotEnv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotEnv = r.Method, r.URL.Path, r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.rollback", "disposition": "deny", "description": "roll an application back", "source": "name"},
		}})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	gs, err := c.SetGuardrail(context.Background(), client.GuardScope{Env: "prod", Name: "burrowd-cloud"}, "", "app.rollback", "deny")
	if err != nil {
		t.Fatalf("SetGuardrail: %v", err)
	}
	if len(gs) != 1 || gs[0].Source != "name" {
		t.Errorf("guardrails = %+v, want the name-tier disposition", gs)
	}
	if gotMethod != "PUT" || gotPath != "/v1/guard/name/burrowd-cloud/app.rollback" || gotEnv != "prod" {
		t.Errorf("request = %s %s?env=%s, want PUT /v1/guard/name/burrowd-cloud/app.rollback?env=prod", gotMethod, gotPath, gotEnv)
	}

	if _, err := c.Guardrails(context.Background(), client.GuardScope{Env: "prod", Name: "burrowd-cloud"}); err != nil {
		t.Fatalf("Guardrails: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/v1/guard/name/burrowd-cloud" || gotEnv != "prod" {
		t.Errorf("request = %s %s?env=%s, want GET /v1/guard/name/burrowd-cloud?env=prod", gotMethod, gotPath, gotEnv)
	}
}

// TestGuardNameScopeLeavesRealNotFoundAlone keeps the refusal narrow. An unknown environment is a
// 404 from the engine, not a missing route: it is the control plane answering the question, and
// dressing it up as version skew would send an operator to upgrade a control plane over a typo.
func TestGuardNameScopeLeavesRealNotFoundAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": `set guardrail: unknown environment "stagng"`,
			"code":  "not_found",
		})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	_, err := c.SetGuardrail(context.Background(), client.GuardScope{Env: "stagng", Name: "website"}, "", "app.deploy", "deny")
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %T (%v), want the control plane's own refusal", err, err)
	}
	if api.Code != "not_found" || !strings.Contains(api.Message, "unknown environment") {
		t.Errorf("error = %+v, want the unknown-environment refusal unchanged", api)
	}
}

// The guardrail CALLER tier (ADR-0094) narrows a write to one kind of credential, and it follows the
// same rule for the same reason, one axis over. These two tests pin the half that matters most: a
// control plane that cannot express the binding must REFUSE, because the write it would otherwise
// perform binds every caller — the operator the flag was reached for to protect included.

// TestGuardBindingRefusedByOlderControlPlane: dropping a `--binds agent` does not merely widen the
// scope, it inverts the intent. The operator asked for a deny the agent is bound by and they are not;
// what an older control plane would store is a deny they are bound by too.
func TestGuardBindingRefusedByOlderControlPlane(t *testing.T) {
	var seen []string
	srv := oldControlPlane(t, &seen)
	c := client.NewClientVersion(srv.URL, "tok", "v0.14.0-rc.5")

	_, err := c.SetGuardrail(context.Background(), client.GuardScope{}, "agent", "app.rollback", "deny")
	if err == nil {
		t.Fatal("SetGuardrail succeeded against a control plane without the caller tier; it must refuse rather than write a disposition binding everyone")
	}
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("error = %T (%v), want a structured *client.APIError", err, err)
	}
	if api.Code != client.CodeScopeUnsupported {
		t.Errorf("code = %q, want %q", api.Code, client.CodeScopeUnsupported)
	}
	for _, want := range []string{"nothing was written", "--binds", "EVERY caller", "burrow cluster upgrade"} {
		if !strings.Contains(api.Message, want) {
			t.Errorf("refusal is missing %q:\n%s", want, api.Message)
		}
	}
	for _, req := range seen {
		if req == "PUT /v1/guard/app.rollback" {
			t.Fatalf("the client fell back to the unbound write: %v", seen)
		}
	}
}

// TestGuardBindingMatchedPair is the current client against a current control plane: the binding
// rides the path ahead of the name, the environment stays on the query, and the two axes compose.
func TestGuardBindingMatchedPair(t *testing.T) {
	var gotPath, gotEnv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotEnv = r.URL.Path, r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.deploy", "disposition": "deny", "description": "deploy a new release", "source": "name", "binds": "agent"},
		}})
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	gs, err := c.SetGuardrail(context.Background(), client.GuardScope{Env: "prod", Name: "burrowd-cloud"}, "agent", "app.deploy", "deny")
	if err != nil {
		t.Fatalf("SetGuardrail: %v", err)
	}
	if len(gs) != 1 || gs[0].Binds != "agent" {
		t.Errorf("guardrails = %+v, want the binding reported back", gs)
	}
	if gotPath != "/v1/guard/binds/agent/name/burrowd-cloud/app.deploy" || gotEnv != "prod" {
		t.Errorf("request = %s?env=%s, want /v1/guard/binds/agent/name/burrowd-cloud/app.deploy?env=prod", gotPath, gotEnv)
	}
	// Without a name the binding still rides the path, alone.
	if _, err := c.SetGuardrail(context.Background(), client.GuardScope{}, "agent", "app.delete", "deny"); err != nil {
		t.Fatalf("SetGuardrail(global): %v", err)
	}
	if gotPath != "/v1/guard/binds/agent/app.delete" {
		t.Errorf("request = %s, want /v1/guard/binds/agent/app.delete", gotPath)
	}
	// And an unbound write is byte-identical to what it has always been.
	if _, err := c.SetGuardrail(context.Background(), client.GuardScope{}, "", "app.delete", "deny"); err != nil {
		t.Fatalf("SetGuardrail(unbound): %v", err)
	}
	if gotPath != "/v1/guard/app.delete" {
		t.Errorf("request = %s, want /v1/guard/app.delete: an unbound write must not change shape", gotPath)
	}
}
