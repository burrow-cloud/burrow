// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderAddWithoutTypeListsSupportedTypes(t *testing.T) {
	var out, errb bytes.Buffer
	// Missing <type>: the error and usage must name the available types so the user isn't left
	// guessing what to pass.
	_ = run(context.Background(), []string{"config", "provider", "add"}, &out, &errb)
	s := errb.String()
	for _, want := range []string{"needs <type>", "cloudflare", "digitalocean"} {
		if !strings.Contains(s, want) {
			t.Errorf("provider add (no type) output missing %q:\n%s", want, s)
		}
	}
}

// TestProviderAddSendsTokenInBody asserts `provider add` issues the control-plane API call with the
// token VALUE in the POST body — not a kubeconfig-direct Secret write, and not in the path or query
// (ADR-0030). The token is piped in (a script path), so the test drives the real RunE.
func TestProviderAddSendsTokenInBody(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// Respond with a recorded provider (no token echoed).
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "digitalocean", "type": "digitalocean",
			"capabilities": []string{"dns"}, "secret_key": "digitalocean",
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("dop_v1_secret\n")) // piped token (non-terminal)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"config", "provider", "add", "digitalocean",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("provider add: %v (stderr: %s)", err, errb.String())
	}

	if gotPath != "/v1/providers" {
		t.Errorf("path = %q, want /v1/providers", gotPath)
	}
	// The token must never appear in the path or query — only the body.
	if strings.Contains(gotPath, "dop_v1_secret") || strings.Contains(gotQuery, "dop_v1_secret") {
		t.Errorf("token leaked into the request path/query: path=%q query=%q", gotPath, gotQuery)
	}
	if !strings.Contains(gotBody, `"token":"dop_v1_secret"`) {
		t.Errorf("request body missing the token: %s", gotBody)
	}
	// The human output names the key, never the token value.
	if strings.Contains(out.String(), "dop_v1_secret") {
		t.Errorf("CLI output leaked the token value:\n%s", out.String())
	}
}

func TestReadTokenFromPipe(t *testing.T) {
	// A non-terminal reader (a pipe/redirect, as in a script) is read directly and trimmed.
	got, err := readToken(strings.NewReader("  dop_v1_abc\n"), io.Discard, "token: ")
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "dop_v1_abc" {
		t.Errorf("readToken = %q, want the trimmed token", got)
	}
}

func TestProviderTypesCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"config", "provider", "types"}, &out, &errb); err != nil {
		t.Fatalf("provider types: %v", err)
	}
	s := out.String()
	for _, want := range []string{"TYPE", "SUPPORTS", "cloudflare", "digitalocean", "dns"} {
		if !strings.Contains(s, want) {
			t.Errorf("provider types output missing %q:\n%s", want, s)
		}
	}
}
