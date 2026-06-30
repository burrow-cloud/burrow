// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/mcp"
)

// writeHandleConfig writes the given localconfig YAML to a temp file and points $BURROW_CONFIG at
// it, so the MCP server resolves env handles against this config (ADR-0036 slice 5b).
func writeHandleConfig(t *testing.T, yaml string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write handle config: %v", err)
	}
	t.Setenv("BURROW_CONFIG", path)
}

// TestEnvHandleRoutesAndSendsName confirms a per-app tool's env argument is a LOCAL HANDLE NAME:
// burrow-mcp resolves it through the handle config to the handle's kube context (which client the
// call routes to) and sends the handle's Env value (the burrowd-registered environment NAME) to the
// control-plane API. The NAME is asserted, never a namespace (ADR-0036 slice 5b; the prior attempt
// #140 wrongly sent the namespace).
func TestEnvHandleRoutesAndSendsName(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: staging
    context: do-nyc1-staging
    appNamespace: burrow-apps-staging
    env: stg
`)

	var gotEnv string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEnv = r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web"})
	}))
	t.Cleanup(api.Close)

	var mu sync.Mutex
	var gotContext string
	clientFor := func(kubeContext string) (*client.Client, error) {
		mu.Lock()
		gotContext = kubeContext
		mu.Unlock()
		return client.NewClient(api.URL, "tok"), nil
	}
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_status",
		Arguments: map[string]any{"app": "web", "env": "staging"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	// The call routes to the handle's kube context.
	mu.Lock()
	defer mu.Unlock()
	if gotContext != "do-nyc1-staging" {
		t.Errorf("routed to context %q, want the handle's context do-nyc1-staging", gotContext)
	}
	// The control plane receives the handle's Env NAME (stg), NOT its namespace (burrow-apps-staging).
	if gotEnv != "stg" {
		t.Errorf("env at the API = %q, want the registered env NAME stg (never the namespace)", gotEnv)
	}
	if gotEnv == "burrow-apps-staging" {
		t.Errorf("env at the API = %q: sent the handle's namespace, must send the env NAME", gotEnv)
	}
}

// TestEnvHandleSendsEchoedEnv confirms a mutating tool echoes the environment it acted in, with the
// handle name, its kube context, and the registered env NAME (ADR-0036 slice 5b).
func TestEnvHandleSendsEchoedEnv(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
    appNamespace: apps
    env: production
`)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release": map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed", "replicas": 1},
		})
	}))
	t.Cleanup(api.Close)
	clientFor := func(string) (*client.Client, error) { return client.NewClient(api.URL, "tok"), nil }
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_deploy",
		Arguments: map[string]any{"app": "web", "image": "img:1", "replicas": 1, "env": "prod"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := decodeStructured[struct {
		Release     client.Release `json:"release"`
		Environment struct {
			Name    string `json:"name"`
			Context string `json:"context"`
			Env     string `json:"env"`
		} `json:"environment"`
	}](t, res)
	if out.Release.ID != "r1" {
		t.Errorf("release = %+v, want r1 (the deploy result is still present)", out.Release)
	}
	if out.Environment.Name != "prod" || out.Environment.Context != "do-nyc1-prod" || out.Environment.Env != "production" {
		t.Errorf("echoed environment = %+v, want prod/do-nyc1-prod/production", out.Environment)
	}
}

// TestEnvHandleUnknownErrors confirms an env argument naming a handle not in the config produces a
// clear, not-registered error (ADR-0036 slice 5b).
func TestEnvHandleUnknownErrors(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
`)

	clientFor := func(string) (*client.Client, error) {
		t.Error("clientFor must not be called when the env handle is unknown")
		return nil, nil
	}
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_status",
		Arguments: map[string]any{"app": "web", "env": "ghost"},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error for an unknown env handle")
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "ghost") || !strings.Contains(text.String(), "not in the config") {
		t.Errorf("error content = %q, want it to name the missing handle and say it is not in the config", text.String())
	}
}

// TestEnvArgDefaultsEmpty confirms omitting the env argument sends no env selector and routes to the
// current kube context (the empty string), so the server applies the default environment without
// reading the handle config (ADR-0036 slice 5b).
func TestEnvArgDefaultsEmpty(t *testing.T) {
	var gotEnv string
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		gotEnv = r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web"})
	})
	if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_status",
		Arguments: map[string]any{"app": "web"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if gotEnv != "" {
		t.Errorf("env query = %q, want empty (default environment)", gotEnv)
	}
}
