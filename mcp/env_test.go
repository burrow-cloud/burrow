// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEnvArgReachesAPI confirms a per-app tool's optional env argument flows through the client to
// the control-plane API as the env selector, so the agent can operate a named environment (ADR-0035
// phase 2b).
func TestEnvArgReachesAPI(t *testing.T) {
	var gotEnv string
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		gotEnv = r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web"})
	})
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
	if gotEnv != "staging" {
		t.Errorf("env query at the API = %q, want staging", gotEnv)
	}
}

// TestEnvArgDefaultsEmpty confirms omitting the env argument sends no env selector, so the server
// applies the default environment (ADR-0035 phase 2b).
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
