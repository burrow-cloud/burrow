// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSecretSet proves `secret set` carries the VALUE in the POST body to burrowd's
// control-plane API (ADR-0029) — not in the path or query, where the access log would see it —
// and that burrowd, not the CLI, writes the Secret.
func TestSecretSet(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	var gotBody map[string]any
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// The response carries the app and KEY only — never the value.
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "STRIPE_KEY"})
	}, "app", "secret", "set", "web", "STRIPE_KEY=sk_live_x")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/secrets" {
		t.Errorf("request = %s %s, want POST /v1/apps/web/secrets", gotMethod, gotPath)
	}
	// The value must be in the BODY, never the path or query (the access log records path+query).
	if strings.Contains(gotPath, "sk_live_x") || strings.Contains(gotQuery, "sk_live_x") {
		t.Errorf("value leaked into the request line: path=%q query=%q", gotPath, gotQuery)
	}
	if gotBody["key"] != "STRIPE_KEY" || gotBody["value"] != "sk_live_x" {
		t.Errorf("body = %#v, want key=STRIPE_KEY value=sk_live_x", gotBody)
	}
	if nr, _ := gotBody["no_restart"].(bool); nr {
		t.Errorf("no_restart = true, want false by default")
	}
	if !strings.Contains(out, "set secret STRIPE_KEY on web") {
		t.Errorf("output = %q", out)
	}
}

func TestSecretSetNoRestart(t *testing.T) {
	var gotBody map[string]any
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "K"})
	}, "app", "secret", "set", "web", "K=V", "--no-restart")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if nr, _ := gotBody["no_restart"].(bool); !nr {
		t.Errorf("no_restart = %#v, want true", gotBody["no_restart"])
	}
	if !strings.Contains(out, "lands on next deploy") {
		t.Errorf("output = %q, want a no-restart note", out)
	}
}

// TestSecretSetStdinKeepsTheValueOutOfArgv is issue #425: with --stdin the argument is the KEY
// alone, so the value never enters shell history or the process table, and a multi-line credential
// (a PEM key) round-trips, which it cannot as an argument.
func TestSecretSetStdinKeepsTheValueOutOfArgv(t *testing.T) {
	const pem = "-----BEGIN PRIVATE KEY-----\nline1\nline2\n-----END PRIVATE KEY-----"
	var gotBody map[string]any
	out, _, err := runCLIStdin(t, pem+"\n", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "GITHUB_APP_KEY"})
	}, "app", "secret", "set", "web", "GITHUB_APP_KEY", "--stdin")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotBody["key"] != "GITHUB_APP_KEY" {
		t.Errorf("body = %#v, want the KEY taken from the argument", gotBody)
	}
	if gotBody["value"] != pem {
		t.Errorf("value = %q, want the multi-line value read from standard input", gotBody["value"])
	}
	if !strings.Contains(out, "set secret GITHUB_APP_KEY on web") {
		t.Errorf("output = %q", out)
	}
}

// TestSecretSetStdinRejectsAValueInTheArgument keeps the two forms from being mixed: passing
// KEY=VALUE with --stdin would put the value in argv, which is the whole thing --stdin avoids.
func TestSecretSetStdinRejectsAValueInTheArgument(t *testing.T) {
	_, _, err := runCLIStdin(t, "value\n", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the control plane was called for an invalid invocation")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}, "app", "secret", "set", "web", "K=V", "--stdin")
	if err == nil || !strings.Contains(err.Error(), "the KEY alone") {
		t.Errorf("err = %v, want a refusal naming the KEY-alone form", err)
	}
}

// TestSecretSetWithoutStdinNeedsAValue confirms the argument form still requires KEY=VALUE, and that
// it fails on the argument rather than blocking on a read nobody asked for.
func TestSecretSetWithoutStdinNeedsAValue(t *testing.T) {
	_, _, err := runCLIStdin(t, "", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the control plane was called for an invalid invocation")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}, "app", "secret", "set", "web", "STRIPE_KEY")
	if err == nil || !strings.Contains(err.Error(), "--stdin") {
		t.Errorf("err = %v, want a refusal that points at --stdin", err)
	}
}

func TestSecretListShowsKeysNotValues(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/apps/web/secrets" {
			t.Errorf("request = %s %s, want GET /v1/apps/web/secrets", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []string{"DATABASE_URL", "STRIPE_KEY"}})
	}, "app", "secret", "list", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "DATABASE_URL") || !strings.Contains(out, "STRIPE_KEY") {
		t.Errorf("output = %q, want the keys", out)
	}
}

func TestSecretUnset(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "STRIPE_KEY"})
	}, "app", "secret", "unset", "web", "STRIPE_KEY", "--no-restart")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/apps/web/secrets/STRIPE_KEY" {
		t.Errorf("request = %s %s, want DELETE /v1/apps/web/secrets/STRIPE_KEY", gotMethod, gotPath)
	}
	if gotQuery != "no_restart=true" {
		t.Errorf("query = %q, want no_restart=true", gotQuery)
	}
}
