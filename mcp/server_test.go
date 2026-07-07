// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/mcp"
)

// newSession wires the given Burrow MCP server to an in-process MCP client session over an
// in-memory transport.
func newSession(t *testing.T, server *sdk.Server) *sdk.ClientSession {
	t.Helper()
	ct, st := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// connect wires the Burrow MCP server (fronting the given mock API handler) to an
// in-process MCP client session. Its client factory ignores the per-call context and points
// every call at the mock API; the routing of an env handle to a context + client is exercised
// separately in TestEnvHandleRoutesAndSendsName.
func connect(t *testing.T, apiHandler http.HandlerFunc) *sdk.ClientSession {
	t.Helper()
	// Isolate from the developer's real ~/.burrow/config: point BURROW_CONFIG at a non-existent file
	// so the ADR-0047 mutating-op ambiguity guard sees an empty handle set here (no environments →
	// no ambiguity). Tests that want registered handles call writeHandleConfig, which overrides this.
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	api := httptest.NewServer(apiHandler)
	t.Cleanup(api.Close)

	clientFor := func(string) (*client.Client, error) { return client.NewClient(api.URL, "tok"), nil }
	return newSession(t, mcp.NewServer(clientFor, "", "test"))
}

func decodeStructured[T any](t *testing.T, res *sdk.CallToolResult) T {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal into %T: %v", out, err)
	}
	return out
}

// TestServerInstructions confirms the server advertises non-empty, agent-orienting instructions at
// connect (the MCP InitializeResult.instructions field), so the agent learns Burrow from
// always-loaded guidance rather than a help tool. The instructions must anchor the load-bearing
// cross-cutting rules: guardrails, confirmation, the human-only registry-login step, and that code
// never travels over MCP.
func TestServerInstructions(t *testing.T) {
	cs := connect(t, func(http.ResponseWriter, *http.Request) {})
	got := cs.InitializeResult().Instructions
	if strings.TrimSpace(got) == "" {
		t.Fatal("server advertised no instructions: the agent gets no top-level orientation")
	}
	for _, anchor := range []string{"guardrail", "confirm", "burrow config registry login", "never travels over MCP"} {
		if !strings.Contains(got, anchor) {
			t.Errorf("instructions missing anchor %q; got:\n%s", anchor, got)
		}
	}
	// The environment guidance must make the target explicit and sticky (ADR-0036): tell the agent to
	// name the env, and never to switch environments to work around a failure — a transient error on
	// one environment must never become an operation against another.
	for _, anchor := range []string{"burrow_environments", "env argument explicitly", "different environment"} {
		if !strings.Contains(got, anchor) {
			t.Errorf("instructions missing environment-safety anchor %q; got:\n%s", anchor, got)
		}
	}
	// The orientation must steer the agent to these tools, not the human's CLI.
	if !strings.Contains(got, "these tools") {
		t.Errorf("instructions should tell the agent to operate through these tools; got:\n%s", got)
	}
	// Human, CLI-only steps (credential and setup commands) must be run by the user in their own
	// terminal, not via a Claude Code `!` prefix in the session: a `!` run is non-interactive and
	// cannot answer the hidden secret prompts, and a credential must never route through the agent
	// session. The guidance must say so and must not suggest an inline `!` run.
	if !strings.Contains(strings.ToLower(got), "own terminal") {
		t.Errorf("instructions should tell the agent to have the user run human steps in their own terminal; got:\n%s", got)
	}
	for _, inline := range []string{"! burrow", "!burrow"} {
		if strings.Contains(got, inline) {
			t.Errorf("instructions must not suggest running a human command via a %q inline `!` prefix; got:\n%s", inline, got)
		}
	}
}

// TestExposureGuidanceSteersToLoadBalancer confirms the agent-facing exposure guidance on the
// tools that recommend ingress install presents a LoadBalancer as the way to make an app publicly
// reachable — a public IP to point DNS at — and no longer offers NodePort as a user choice.
// NodePort exposes on high ports (30000+) and cannot serve a turnkey public site on :80/:443, so
// the guidance must not suggest it or steer the human at `--expose nodeport`.
func TestExposureGuidanceSteersToLoadBalancer(t *testing.T) {
	cs := connect(t, func(http.ResponseWriter, *http.Request) {})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	desc := map[string]string{}
	for _, tool := range res.Tools {
		desc[tool.Name] = tool.Description
	}
	for _, name := range []string{"burrow_expose", "burrow_cluster"} {
		got, ok := desc[name]
		if !ok {
			t.Errorf("tool %q not registered", name)
			continue
		}
		if !strings.Contains(got, "LoadBalancer") {
			t.Errorf("tool %q guidance does not present a LoadBalancer; got:\n%s", name, got)
		}
		if !strings.Contains(got, "--expose loadbalancer") {
			t.Errorf("tool %q guidance omits the `--expose loadbalancer` install command; got:\n%s", name, got)
		}
		// NodePort is no longer a user-facing exposure choice: the guidance must not offer it.
		if strings.Contains(got, "--expose nodeport") {
			t.Errorf("tool %q guidance still offers `--expose nodeport`: NodePort must not be suggested; got:\n%s", name, got)
		}
		if strings.Contains(got, "single point of failure") {
			t.Errorf("tool %q guidance still carries the NodePort single-point-of-failure framing; got:\n%s", name, got)
		}
	}
}

func TestListTools(t *testing.T) {
	cs := connect(t, func(http.ResponseWriter, *http.Request) {})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"burrow_deploy", "burrow_status", "burrow_logs", "burrow_rollback", "burrow_scale", "burrow_run", "burrow_autoscale", "burrow_domain_add", "burrow_domain_remove", "burrow_providers", "burrow_secret_list", "burrow_secret_unset", "burrow_addon_attach", "burrow_addon_backup", "burrow_addon_backups", "burrow_audit", "burrow_cluster", "burrow_environments"} {
		if !got[want] {
			t.Errorf("tool %q not registered (have %v)", want, got)
		}
	}

	// burrow_contexts is retired (ADR-0036): the agent discovers what it can target through
	// burrow_environments (local handles), not a raw kubeconfig-context listing.
	if got["burrow_contexts"] {
		t.Error("burrow_contexts must NOT exist: it is retired in favor of burrow_environments (ADR-0036)")
	}

	// Security boundary (ADR-0032): restore overwrites an app's live database, so it is CLI-only —
	// there must be NO restore (or detach) MCP tool. Backup and the backups listing are allowed:
	// they move no secret value (an in-cluster Job does the dump).
	for _, banned := range []string{"burrow_addon_restore", "burrow_addon_detach"} {
		if got[banned] {
			t.Errorf("tool %q must NOT exist: a destructive overwrite is CLI-only", banned)
		}
	}
	// Security boundary (ADR-0029/0004): there must be NO secret-set tool — a secret value never
	// crosses MCP. Setting a secret travels over burrowd's authenticated control-plane API (the
	// CLI or the UI), never the agent surface.
	if got["burrow_secret_set"] {
		t.Error("burrow_secret_set must NOT exist: secret values never travel over MCP")
	}

	// Security boundary (ADR-0030/0004): a credential VALUE never crosses MCP either. There must be
	// no provider-add or authenticated-connect tool, and NO tool may accept a `token` (or `auth`)
	// input — provider add and authenticated addon connect are human/CLI operations. The agent only
	// connects unauthenticated backends or references an already-configured credential.
	for _, banned := range []string{"burrow_provider_add", "burrow_addon_connect"} {
		if got[banned] {
			t.Errorf("tool %q must NOT exist: a credential value never travels over MCP", banned)
		}
	}
	hasContext := map[string]bool{}
	for _, tool := range res.Tools {
		if tool.InputSchema == nil {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %q input schema: %v", tool.Name, err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode %q input schema: %v", tool.Name, err)
		}
		for prop := range schema.Properties {
			if prop == "context" {
				hasContext[tool.Name] = true
			}
			if prop == "token" || prop == "auth" {
				t.Errorf("tool %q exposes a %q input: a credential value must never cross MCP", tool.Name, prop)
			}
			// The Postgres attach tool (and every tool) must never accept a database password or
			// connection string: burrowd generates the DATABASE_URL server-side (ADR-0031). No tool
			// input names a connection-string-shaped secret. (`value` is allowed: config set carries a
			// non-secret config value, and there is no secret-set tool.)
			switch prop {
			case "password", "url", "database_url", "connection_string", "dsn":
				t.Errorf("tool %q exposes a %q input: a database secret value must never cross MCP", tool.Name, prop)
			}
		}
	}

	// Per-call targeting (ADR-0035/0036): every operating tool that contacts a cluster, read or
	// mutate, takes an optional `context` (the low-level raw kube-context override). `context` is a
	// kubeconfig label, not a credential, so the secret scan above lets it through.
	for _, want := range []string{"burrow_deploy", "burrow_status", "burrow_apps", "burrow_scale", "burrow_cluster", "burrow_guard"} {
		if !hasContext[want] {
			t.Errorf("tool %q has no context input: every operating tool must be targetable per call", want)
		}
	}
	// burrow_environments lists the LOCAL handles (ADR-0036), reading the local config and contacting
	// no cluster, so it takes no context of its own.
	if hasContext["burrow_environments"] {
		t.Error("burrow_environments must NOT take a context: it lists local handles and contacts no cluster")
	}
}

// TestPerCallContextRouting confirms a tool's optional context selects which client (which
// cluster's burrowd) the call routes to, and that omitting it uses the default current context
// (the empty string), for a read tool and a mutating tool alike (ADR-0035 phase 1b).
func TestPerCallContextRouting(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/deploy") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"release": map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed", "replicas": 1},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "has_release": true, "running": true,
			"release":  map[string]any{"id": "r1", "image": "img:1", "status": "deployed"},
			"workload": map[string]any{"desired_replicas": 1, "ready_replicas": 1, "available": true},
		})
	}))
	t.Cleanup(api.Close)

	var mu sync.Mutex
	var gotContexts []string
	clientFor := func(kubeContext string) (*client.Client, error) {
		mu.Lock()
		gotContexts = append(gotContexts, kubeContext)
		mu.Unlock()
		return client.NewClient(api.URL, "tok"), nil
	}
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	call := func(name string, args map[string]any) {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("CallTool %q: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("tool %q returned error: %v", name, res.Content)
		}
	}

	// A read tool routes to the named context.
	call("burrow_status", map[string]any{"app": "web", "context": "prod-cluster"})
	// A mutating tool routes to the named context.
	call("burrow_deploy", map[string]any{"app": "web", "image": "img:1", "replicas": 1, "context": "staging"})
	// Omitting context falls back to the current context (the empty string).
	call("burrow_status", map[string]any{"app": "web"})

	want := []string{"prod-cluster", "staging", ""}
	mu.Lock()
	defer mu.Unlock()
	if len(gotContexts) != len(want) {
		t.Fatalf("requested contexts = %v, want %v", gotContexts, want)
	}
	for i, w := range want {
		if gotContexts[i] != w {
			t.Errorf("call %d routed to context %q, want %q (all: %v)", i, gotContexts[i], w, gotContexts)
		}
	}
}

// TestEnvironmentsToolListsLocalHandles confirms burrow_environments lists the LOCAL environment
// handles from the handle config (not the burrowd registry), marking the pinned current selection,
// and contacts no cluster (ADR-0036 slice 5b).
func TestEnvironmentsToolListsLocalHandles(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
current: prod
environments:
  - name: dev
    context: do-nyc1-dev
    appNamespace: burrow-apps
  - name: prod
    context: do-nyc1-prod
    appNamespace: apps
    env: prod
`)

	clientFor := func(string) (*client.Client, error) {
		t.Error("burrow_environments must not build a control-plane client: it reads the local handle config")
		return nil, nil
	}
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "burrow_environments"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := decodeStructured[struct {
		Environments []struct {
			Name      string `json:"name"`
			Context   string `json:"context"`
			Namespace string `json:"namespace"`
			Env       string `json:"env"`
			Current   bool   `json:"current"`
		} `json:"environments"`
	}](t, res)
	if len(out.Environments) != 2 {
		t.Fatalf("environments = %+v, want 2 local handles", out.Environments)
	}
	if out.Environments[0].Name != "dev" || out.Environments[0].Context != "do-nyc1-dev" {
		t.Errorf("first handle = %+v, want dev/do-nyc1-dev", out.Environments[0])
	}
	prod := out.Environments[1]
	if prod.Name != "prod" || prod.Context != "do-nyc1-prod" || prod.Env != "prod" {
		t.Errorf("prod handle = %+v, want prod/do-nyc1-prod with env name prod", prod)
	}
	if !prod.Current {
		t.Errorf("prod should be marked current (it is pinned): %+v", out.Environments)
	}
	if out.Environments[0].Current {
		t.Errorf("dev should not be marked current: %+v", out.Environments)
	}
}

func TestSecretListToolReturnsKeysOnly(t *testing.T) {
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/apps/web/secrets" {
			t.Errorf("path = %q, want /v1/apps/web/secrets", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []string{"DATABASE_URL", "STRIPE_KEY"}})
	})
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_secret_list",
		Arguments: map[string]any{"app": "web"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := decodeStructured[struct {
		Keys []string `json:"keys"`
	}](t, res)
	if len(out.Keys) != 2 || out.Keys[0] != "DATABASE_URL" {
		t.Errorf("keys = %v, want [DATABASE_URL STRIPE_KEY]", out.Keys)
	}
}

func TestAuditToolReturnsRecords(t *testing.T) {
	var gotPath, gotQuery string
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{
				{
					"timestamp": "2026-06-23T02:00:00Z",
					"operation": "app_delete",
					"target":    "web",
					"args":      map[string]string{"confirm": "false"},
					"outcome":   "held",
				},
			},
		})
	})

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_audit",
		Arguments: map[string]any{"app": "web", "operation": "app_delete", "outcome": "held", "limit": 50},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := decodeStructured[struct {
		Entries []client.AuditEntry `json:"entries"`
	}](t, res)
	if len(out.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(out.Entries))
	}
	e := out.Entries[0]
	if e.Operation != "app_delete" || e.Outcome != "held" || e.Target != "web" {
		t.Errorf("entry = %+v, want app_delete/held/web", e)
	}
	// Redaction (ADR-0027): args carry KEY NAMES and safe metadata only — never a secret value.
	if _, hasValue := e.Args["DATABASE_URL"]; hasValue {
		t.Errorf("audit args leaked a secret value: %v", e.Args)
	}
	if gotPath != "/v1/audit" {
		t.Errorf("API path = %q, want /v1/audit", gotPath)
	}
	// The MCP tool reuses the same read path/filters as the `burrow audit` CLI.
	for _, want := range []string{"app=web", "operation=app_delete", "outcome=held", "limit=50"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestClusterToolReturnsCapabilities(t *testing.T) {
	var gotPath string
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ingress":       map[string]any{"present": true, "classes": []string{"nginx"}},
			"storage":       map[string]any{"default_present": true, "default_class": "do-block-storage"},
			"load_balancer": map[string]any{"supported": true, "inferred": true},
			"cert_manager":  map[string]any{"present": false},
			"provider":      map[string]any{"cloud": "digitalocean", "name": "DigitalOcean"},
			"dns":           map[string]any{"configured": false},
		})
	})

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "burrow_cluster"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := decodeStructured[client.ClusterCapabilities](t, res)
	if !out.Ingress.Present || out.Ingress.Classes[0] != "nginx" {
		t.Errorf("ingress = %+v", out.Ingress)
	}
	if out.Storage.DefaultClass != "do-block-storage" || out.Provider.Name != "DigitalOcean" {
		t.Errorf("report = %+v", out)
	}
	if gotPath != "/v1/cluster" {
		t.Errorf("API path = %q, want /v1/cluster", gotPath)
	}
}

func TestDeployToolRoundTrip(t *testing.T) {
	var gotPath string
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release": map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed", "replicas": 2},
		})
	})

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_deploy",
		Arguments: map[string]any{"app": "web", "image": "img:1", "replicas": 2},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := decodeStructured[client.DeployResult](t, res)
	if out.Release.ID != "r1" || out.Release.Status != "deployed" {
		t.Errorf("release = %+v", out.Release)
	}
	if gotPath != "/v1/apps/web/deploy" {
		t.Errorf("API path = %q, want /v1/apps/web/deploy", gotPath)
	}
}

// TestRunToolRoundTrip confirms burrow_run posts the command to the app's run endpoint with the
// confirm and ttl_seconds pass-through, echoes the environment it acted in, and surfaces the
// structured exit code and captured (combined stdout+stderr) output (ADR-0048).
func TestRunToolRoundTrip(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
    appNamespace: apps
    env: production
`)

	var gotMethod, gotPath string
	var gotBody map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "exit_code": 3, "stdout": "migrating...\nfailed: table locked\n",
		})
	}))
	t.Cleanup(api.Close)
	clientFor := func(string) (*client.Client, error) { return client.NewClient(api.URL, "tok"), nil }
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_run",
		Arguments: map[string]any{"app": "web", "command": []string{"npm", "run", "migrate"}, "ttl_seconds": 0, "confirm": true, "env": "prod"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/run" {
		t.Errorf("request = %s %s, want POST /v1/apps/web/run", gotMethod, gotPath)
	}
	// The command, the confirm acknowledgement, and the explicit ttl_seconds=0 all reach the API.
	if cmd, ok := gotBody["command"].([]any); !ok || len(cmd) != 3 || cmd[0] != "npm" || cmd[2] != "migrate" {
		t.Errorf("command in body = %#v, want [npm run migrate]", gotBody["command"])
	}
	if c, _ := gotBody["confirm"].(bool); !c {
		t.Errorf("confirm in body = %#v, want true", gotBody["confirm"])
	}
	if ttl, ok := gotBody["ttl_seconds"].(float64); !ok || ttl != 0 {
		t.Errorf("ttl_seconds in body = %#v, want 0 (present, delete immediately)", gotBody["ttl_seconds"])
	}

	out := decodeStructured[struct {
		client.RunResult
		Environment struct {
			Name    string `json:"name"`
			Context string `json:"context"`
			Env     string `json:"env"`
		} `json:"environment"`
	}](t, res)
	if out.ExitCode != 3 || !strings.Contains(out.Stdout, "table locked") {
		t.Errorf("result = %+v, want exit code 3 and the captured output", out.RunResult)
	}
	if out.Environment.Name != "prod" || out.Environment.Context != "do-nyc1-prod" || out.Environment.Env != "production" {
		t.Errorf("echoed environment = %+v, want prod/do-nyc1-prod/production", out.Environment)
	}
}

// TestRunToolHeldSurfaces confirms a run the app.run guardrail holds for confirmation comes back as a
// tool error the agent reads (naming the guardrail), rather than executing — the standard held flow,
// with no special client handling (ADR-0048, ADR-0020).
func TestRunToolHeldSurfaces(t *testing.T) {
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "run held for confirmation", "code": "app.run"})
	})

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_run",
		Arguments: map[string]any{"app": "web", "command": []string{"psql", "-c", "DROP TABLE users"}},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected a tool error result for a held run")
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "app.run") {
		t.Errorf("error content = %q, want it to name the app.run guardrail", text.String())
	}
}

// TestRunToolRefusesAmbiguousEnv confirms burrow_run is a mutating tool under the ADR-0047 forcing
// function: with more than one environment registered and no env named, the run is refused before it
// routes, so a command never lands on whichever context happens to be current.
func TestRunToolRefusesAmbiguousEnv(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
    appNamespace: apps
    env: production
  - name: staging
    context: do-nyc1-staging
    appNamespace: staging
`)
	clientFor := func(string) (*client.Client, error) {
		t.Error("clientFor must not be called: an ambiguous mutating run must be refused before it routes")
		return nil, nil
	}
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_run",
		Arguments: map[string]any{"app": "web", "command": []string{"echo", "hi"}},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error: an ambiguous run with no env and more than one environment must be refused")
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	for _, want := range []string{"prod", "staging", "more than one environment", "env argument"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("refusal = %q, want it to mention %q", text.String(), want)
		}
	}
}

func TestToolSurfacesControlPlaneError(t *testing.T) {
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "requested 9 exceeds the ceiling of 5", "code": "app.replica_ceiling"})
	})

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_scale",
		Arguments: map[string]any{"app": "web", "replicas": 9},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected a tool error result for a refused scale")
	}
	// The control plane's message and code reach the agent in the error content.
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "app.replica_ceiling") {
		t.Errorf("error content = %q, want it to mention the guardrail code", text.String())
	}
}

func TestReachabilityToolWaitConverges(t *testing.T) {
	// The app is already live on the first check, so wait mode converges without polling (no real
	// sleeping); the poll/timeout loop itself is exercised deterministically in the client tests.
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/apps/web/reachability" {
			t.Errorf("path = %q, want /v1/apps/web/reachability", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "reachable": true, "url": "https://web.example.com",
		})
	})

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_reachability",
		Arguments: map[string]any{"app": "web", "wait": true},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := decodeStructured[client.ReachabilityResult](t, res)
	if !out.Reachable || out.URL != "https://web.example.com" {
		t.Errorf("verdict = {reachable:%v url:%q}, want live at the https URL", out.Reachable, out.URL)
	}
}

func TestAutoscaleToolAppliesDefaults(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "env": "default", "min_replicas": 1, "max_replicas": 10, "cpu_percent": 90,
			"metrics_available": false, "warning": "autoscaling needs metrics-server, which was not detected.",
		})
	})

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_autoscale",
		Arguments: map[string]any{"app": "web", "cpu": 90},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/autoscale" {
		t.Errorf("request = %s %s, want POST /v1/apps/web/autoscale", gotMethod, gotPath)
	}
	// The agent named only cpu; the tool fills the min/max defaults (1/10).
	if !strings.Contains(gotBody, `"min":1`) || !strings.Contains(gotBody, `"max":10`) || !strings.Contains(gotBody, `"cpu":90`) {
		t.Errorf("body = %s, want min 1, max 10, cpu 90", gotBody)
	}
	out := decodeStructured[client.AutoscaleResult](t, res)
	if out.MaxReplicas != 10 || out.CPUPercent != 90 || out.MetricsAvailable {
		t.Errorf("result = %+v", out)
	}
	if out.Warning == "" {
		t.Errorf("expected the metrics-absent warning to reach the agent")
	}
}

func TestAutoscaleToolOff(t *testing.T) {
	var gotMethod, gotPath string
	cs := connect(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_autoscale",
		Arguments: map[string]any{"app": "web", "off": true},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/apps/web/autoscale" {
		t.Errorf("request = %s %s, want DELETE /v1/apps/web/autoscale", gotMethod, gotPath)
	}
}
