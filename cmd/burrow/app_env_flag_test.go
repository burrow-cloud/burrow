// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAppEnvFlagReachesClient confirms the --env flag on a per-app command is threaded into the
// control-plane request as the env selector, so the CLI can operate a named environment (ADR-0035
// phase 2b).
func TestAppEnvFlagReachesClient(t *testing.T) {
	var gotEnv string
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotEnv = r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web"})
	}, "app", "status", "web", "--env", "staging")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotEnv != "staging" {
		t.Errorf("env query at the API = %q, want staging", gotEnv)
	}
}

// TestAppEnvFlagDefaultsEmpty confirms omitting --env sends no env selector, so the server applies
// the default environment (ADR-0035 phase 2b).
func TestAppEnvFlagDefaultsEmpty(t *testing.T) {
	var gotEnv string
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotEnv = r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web"})
	}, "app", "status", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotEnv != "" {
		t.Errorf("env query = %q, want empty (default environment)", gotEnv)
	}
}
