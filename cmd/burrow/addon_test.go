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

// TestAddonConnectAuthSendsTokenInBody asserts `addon connect --auth` sends the bearer token VALUE
// in the POST body — not a kubeconfig-direct Secret write, and not in the path or query (ADR-0030).
func TestAddonConnectAuthSendsTokenInBody(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "loki", "type": "logs", "mode": "connected",
			"endpoint": "loki.svc:3100", "capabilities": []string{"logs"},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("s3cr3t\n")) // piped token (non-terminal)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"addon", "connect", "loki", "--auth", "--endpoint", "loki.svc:3100",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("addon connect --auth: %v (stderr: %s)", err, errb.String())
	}

	if gotPath != "/v1/addons/connect" {
		t.Errorf("path = %q, want /v1/addons/connect", gotPath)
	}
	if strings.Contains(gotPath, "s3cr3t") || strings.Contains(gotQuery, "s3cr3t") {
		t.Errorf("token leaked into the request path/query: path=%q query=%q", gotPath, gotQuery)
	}
	if !strings.Contains(gotBody, `"token":"s3cr3t"`) {
		t.Errorf("request body missing the token: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"secret_key":"addon-loki"`) {
		t.Errorf("request body missing the secret key: %s", gotBody)
	}
	if strings.Contains(out.String(), "s3cr3t") {
		t.Errorf("CLI output leaked the token value:\n%s", out.String())
	}
}

// TestAddonConnectUnauthenticatedSendsNoToken asserts a plain `addon connect` (no --auth) sends an
// empty token and key — the agent-reachable unauthenticated path is unchanged.
func TestAddonConnectUnauthenticatedSendsNoToken(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "loki", "type": "logs", "mode": "connected",
			"endpoint": "loki.svc:3100", "capabilities": []string{"logs"},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"addon", "connect", "loki", "--endpoint", "loki.svc:3100",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("addon connect: %v (stderr: %s)", err, errb.String())
	}
	if !strings.Contains(gotBody, `"token":""`) || !strings.Contains(gotBody, `"secret_key":""`) {
		t.Errorf("unauthenticated connect should send empty token and key: %s", gotBody)
	}
}
