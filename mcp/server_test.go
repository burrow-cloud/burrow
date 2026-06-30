// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/burrow-cloud/burrow/client"
	bconnect "github.com/burrow-cloud/burrow/connect"
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
// every call at the mock API; the routing of context to a client is exercised separately in
// TestPerCallContextRouting.
func connect(t *testing.T, apiHandler http.HandlerFunc) *sdk.ClientSession {
	t.Helper()
	api := httptest.NewServer(apiHandler)
	t.Cleanup(api.Close)

	clientFor := func(string) (*client.Client, error) { return client.NewClient(api.URL, "tok"), nil }
	lister := func() ([]bconnect.Context, error) {
		return []bconnect.Context{{Name: "current", Cluster: "c", Current: true}}, nil
	}
	return newSession(t, mcp.NewServer(clientFor, lister, "test"))
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
	for _, want := range []string{"burrow_deploy", "burrow_status", "burrow_logs", "burrow_rollback", "burrow_scale", "burrow_domain_add", "burrow_domain_remove", "burrow_providers", "burrow_secret_list", "burrow_secret_unset", "burrow_addon_attach", "burrow_addon_backup", "burrow_addon_backups", "burrow_audit", "burrow_cluster", "burrow_environments"} {
		if !got[want] {
			t.Errorf("tool %q not registered (have %v)", want, got)
		}
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

	// Per-call environment routing (ADR-0035): every operating tool, read or mutate, takes an
	// optional `context`. `context` is a kubeconfig label, not a credential, so the secret scan
	// above lets it through. burrow_environments is the exception: it lists the contexts the agent
	// can target and so takes no context of its own.
	for _, want := range []string{"burrow_deploy", "burrow_status", "burrow_apps", "burrow_scale", "burrow_cluster", "burrow_guard"} {
		if !hasContext[want] {
			t.Errorf("tool %q has no context input: every operating tool must be targetable per call", want)
		}
	}
	if hasContext["burrow_environments"] {
		t.Error("burrow_environments must NOT take a context: it lists the contexts to target")
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
	lister := func() ([]bconnect.Context, error) { return nil, nil }
	cs := newSession(t, mcp.NewServer(clientFor, lister, "test"))

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

// TestEnvironmentsToolListsContexts confirms burrow_environments returns the available
// environments (kubeconfig contexts) and marks the current one, so the agent knows what it can
// name in another tool's context argument (ADR-0035 phase 1b).
func TestEnvironmentsToolListsContexts(t *testing.T) {
	clientFor := func(string) (*client.Client, error) {
		t.Error("burrow_environments must not build a control-plane client: it reads the local kubeconfig")
		return nil, nil
	}
	lister := func() ([]bconnect.Context, error) {
		return []bconnect.Context{
			{Name: "prod-cluster", Cluster: "prod", Current: false},
			{Name: "staging", Cluster: "stg", Current: true},
		}, nil
	}
	cs := newSession(t, mcp.NewServer(clientFor, lister, "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "burrow_environments"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error: %v", res.Content)
	}
	out := decodeStructured[struct {
		Environments []struct {
			Name    string `json:"name"`
			Cluster string `json:"cluster"`
			Current bool   `json:"current"`
		} `json:"environments"`
	}](t, res)
	if len(out.Environments) != 2 {
		t.Fatalf("environments = %+v, want 2", out.Environments)
	}
	current := map[string]bool{}
	for _, e := range out.Environments {
		current[e.Name] = e.Current
	}
	if !current["staging"] {
		t.Errorf("staging should be marked current: %+v", out.Environments)
	}
	if current["prod-cluster"] {
		t.Errorf("prod-cluster should not be marked current: %+v", out.Environments)
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
