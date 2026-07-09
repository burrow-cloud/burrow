// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// cannedControlPlane stands up an httptest.Server that answers the read-only control-plane endpoints
// the client hits with fixed JSON, so the command wiring can be exercised without a cluster.
func cannedControlPlane(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/apps", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"apps": []map[string]any{
			{"app": "web", "kind": "Deployment", "image": "img:1", "desired_replicas": 2, "ready_replicas": 2, "available": true},
		}})
	})
	mux.HandleFunc("/v1/apps/web/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "has_release": true, "running": true})
	})
	mux.HandleFunc("/v1/apps/web/logs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"lines": []map[string]any{{"pod": "web-1", "message": "hello"}}})
	})
	mux.HandleFunc("/v1/apps/web/secrets", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []string{"DATABASE_URL", "API_KEY"}})
	})
	mux.HandleFunc("/v1/guard", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.deploy", "disposition": "allow", "description": "deploy an app"},
		}})
	})
	mux.HandleFunc("/v1/audit", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": []map[string]any{
			{"operation": "deploy", "target": "web", "outcome": "executed"},
		}})
	})
	mux.HandleFunc("/v1/cluster", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ingress": map[string]any{"present": true, "classes": []string{"nginx"}}})
	})
	mux.HandleFunc("/v1/providers", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{
			{"name": "do", "type": "digitalocean", "capabilities": []string{"dns"}, "secret_key": "do-token"},
		}})
	})
	mux.HandleFunc("/v1/addons", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"addons": []map[string]any{
			{"name": "logs", "type": "logs", "mode": "installed", "endpoint": "http://logs", "capabilities": []string{"logs"}, "ready": true},
		}})
	})
	mux.HandleFunc("/v1/logs/query", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": []map[string]any{{"message": "boom", "pod": "web-1"}}})
	})
	mux.HandleFunc("/v1/metrics/query", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"samples": []map[string]any{{"value": "1", "labels": map[string]string{"job": "web"}}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runAgent drives run against the direct control-plane URL with a token, capturing stdout.
func runAgent(t *testing.T, srv *httptest.Server, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	full := append(args, "--control-plane", srv.URL, "--token", "t")
	if err := run(context.Background(), full, &out, &errb); err != nil {
		t.Fatalf("run(%v): %v (stderr: %s)", args, err, errb.String())
	}
	return out.String()
}

func TestReadOnlyVerbsWiring(t *testing.T) {
	srv := cannedControlPlane(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"apps", []string{"apps"}, `"web"`},
		{"status", []string{"status", "web"}, `"has_release": true`},
		{"logs", []string{"logs", "web", "--tail", "5"}, `"hello"`},
		{"secret", []string{"secret", "web"}, `"DATABASE_URL"`},
		{"guard", []string{"guard"}, `"app.deploy"`},
		{"audit", []string{"audit", "--operation", "deploy"}, `"executed"`},
		{"cluster", []string{"cluster"}, `"present": true`},
		{"providers", []string{"providers"}, `"digitalocean"`},
		{"addons", []string{"addons"}, `"logs"`},
		{"logs-query", []string{"logs-query", "error", "--limit", "10"}, `"boom"`},
		{"metrics-query", []string{"metrics-query", "up"}, `"value": "1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runAgent(t, srv, tc.args...)
			if !strings.Contains(out, tc.want) {
				t.Errorf("output = %q, want it to contain %q", out, tc.want)
			}
			// Every verb prints JSON — a decodable document (array or object), not human text.
			if !json.Valid([]byte(out)) {
				t.Errorf("output is not valid JSON: %q", out)
			}
		})
	}
}

// TestSecretEmitsKeysOnly confirms the secret verb wraps the KEYS in a {"keys": [...]} object and
// never a value (there is no secret value to leak; the endpoint returns keys only).
func TestSecretEmitsKeysOnly(t *testing.T) {
	srv := cannedControlPlane(t)
	out := runAgent(t, srv, "secret", "web")
	var got map[string][]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v (out=%q)", err, out)
	}
	if len(got["keys"]) != 2 || got["keys"][0] != "DATABASE_URL" {
		t.Errorf("keys = %v, want the two key names", got["keys"])
	}
}

// TestEnvironmentsReadsLocalConfig confirms the environments verb reports the local handles with no
// cluster contact, reading them from $BURROW_CONFIG.
func TestEnvironmentsReadsLocalConfig(t *testing.T) {
	writeConfig(t, `apiVersion: burrow.dev/v1
kind: Config
current: prod
environments:
  - name: prod
    context: prod-ctx
  - name: staging
    context: staging-ctx
`)
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"environments"}, &out, &errb); err != nil {
		t.Fatalf("run environments: %v (stderr: %s)", err, errb.String())
	}
	var got environmentsResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (out=%q)", err, out.String())
	}
	if len(got.Environments) != 2 {
		t.Fatalf("environments = %v, want 2 handles", got.Environments)
	}
	if got.Current != "prod" {
		t.Errorf("current = %q, want prod (pinned)", got.Current)
	}
	if got.Mode != "pinned" {
		t.Errorf("mode = %q, want pinned", got.Mode)
	}
}

// TestAdminVerbsAbsent is the structural capability-reduction assertion (ADR-0049 §2a): the dangerous
// ADMIN verbs are not compiled into this binary, so invoking one is an unknown-command error. The
// compute mutating verbs (deploy, rollback, scale, autoscale, run) are now PRESENT (Phase 2a) and so
// are deliberately not listed here; TestMutatingVerbsPresent asserts they exist. What must remain
// absent is the admin surface — install/bootstrap/cluster setup, guard set, app delete, the
// registry/provider credential writes, and the `burrow agent <tool> install` wiring command.
func TestAdminVerbsAbsent(t *testing.T) {
	absent := [][]string{
		{"install"},
		{"bootstrap"},
		{"cluster", "bootstrap"},
		{"cluster", "ingress", "install"},
		{"upgrade"},
		{"delete", "web"},                      // app delete → Phase 2b, still absent
		{"expose", "web"},                      // expose → Phase 2b
		{"unexpose", "web"},                    // unexpose → Phase 2b
		{"guard", "set", "app.deploy", "deny"}, // guardrail policy write (operator only)
		{"config", "set", "web", "K=V"},        // config write → Phase 2b
		{"secret", "set", "web", "K=V"},        // secret write (never over the agent channel)
		{"provider", "add", "digitalocean"},    // provider credential write
		{"registry", "login", "ghcr.io"},       // registry credential write
		{"agent", "claude", "install"},         // the wiring command → Phase 3
		{"env", "add", "prod"},                 // environment registration (operator)
	}
	for _, args := range absent {
		var out, errb bytes.Buffer
		err := run(context.Background(), args, &out, &errb)
		if err == nil {
			t.Errorf("run(%v) succeeded, want an error — the admin verb must be structurally absent", args)
		}
	}
}

// TestStructuralReduction asserts the property behind the capability reduction still holds: the
// binary imports the thin client and its own package, never cmd/burrow (the human admin CLI), so the
// admin verbs cannot leak in through a shared command tree. It is a coarser, compile-independent guard
// than enumerating verbs.
func TestStructuralReduction(t *testing.T) {
	pkgs, err := exec.Command("go", "list", "-deps", "./.").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	if strings.Contains(string(pkgs), "burrow/cmd/burrow\n") {
		t.Error("burrow-agent imports cmd/burrow (the human admin CLI); the agent binary must not pull the admin command tree")
	}
}
