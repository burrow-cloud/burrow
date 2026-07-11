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
	mux.HandleFunc("/v1/apps/web/history", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"releases": []map[string]any{
			{"id": "r2", "app": "web", "image": "img:2", "status": "deployed"},
			{"id": "r1", "app": "web", "image": "img:1", "status": "superseded"},
		}})
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
		{"history", []string{"history", "web"}, `"img:2"`},
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

// TestHistoryIsReadOnly confirms `history` is a read verb, not a mutating one: it prints the release
// array straight through withClient, never the outcome envelope the mutate path wraps a result in. A
// deploy timeline observes; it never changes anything (ADR-0052 §6 — the agent observes).
func TestHistoryIsReadOnly(t *testing.T) {
	srv := cannedControlPlane(t)
	out := runAgent(t, srv, "history", "web")
	// The read path emits the bare release array; the mutate path would emit an object with a
	// top-level "outcome" field. Assert we got the array and not the envelope.
	var releases []map[string]any
	if err := json.Unmarshal([]byte(out), &releases); err != nil {
		t.Fatalf("history output is not a JSON array: %v (out=%q)", err, out)
	}
	if len(releases) != 2 || releases[0]["image"] != "img:2" {
		t.Errorf("releases = %v, want the two-entry timeline newest first", releases)
	}
	if strings.Contains(out, `"outcome"`) {
		t.Errorf("history emitted a mutation outcome envelope; it must be a read-only verb: %q", out)
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
// ADMIN verbs are not compiled into this binary, so invoking one is an error (an unknown command, or a
// bad-arg refusal where no such subcommand exists). The mutating verbs — the Phase 2a compute verbs
// (deploy, rollback, scale, autoscale, run) and the Phase 2b routing/add-on/config/delete verbs — are
// now PRESENT and so are deliberately not listed here; TestMutatingVerbsPresent asserts they exist.
// What must remain absent is the admin surface — install/bootstrap/cluster setup, guard set, the
// registry/provider credential writes, the `burrow agent <tool> install` wiring command, and — the
// standing project rule — SETTING a secret value, which never routes through the agent channel.
func TestAdminVerbsAbsent(t *testing.T) {
	absent := [][]string{
		{"install"},
		{"bootstrap"},
		{"cluster", "bootstrap"},
		{"cluster", "ingress", "install"},
		{"upgrade"},
		{"guard", "set", "app.deploy", "deny"}, // guardrail policy write (operator only)
		{"secret", "set", "web", "K=V"},        // secret VALUE write — NEVER over the agent channel (ADR-0029)
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

// TestSecretSetStructurallyAbsent pins the standing project rule (ADR-0029): there is no `secret set`
// verb — a secret VALUE must never route through the agent channel. The `secret` command carries only
// the read-only key list and the value-free `unset` subcommand, so `secret set` has no handler: it
// falls to the list verb, which refuses the extra args. `secret unset` and the secret LIST remain the
// only secret surfaces the agent can express.
func TestSecretSetStructurallyAbsent(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"secret", "set", "web", "K=V"}, &out, &errb); err == nil {
		t.Fatal("run(secret set web K=V) succeeded, want an error — setting a secret value must be structurally absent")
	}
	// The subcommand tree confirms it: `set` is not registered under `secret`; only `unset` is.
	secret := newSecretCmd()
	for _, sub := range secret.Commands() {
		if sub.Name() == "set" {
			t.Error("the secret command has a `set` subcommand; a secret value must never route through the agent")
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
