// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/mcp"
)

// connect wires the Burrow MCP server (fronting the given mock API handler) to an
// in-process MCP client session over an in-memory transport.
func connect(t *testing.T, apiHandler http.HandlerFunc) *sdk.ClientSession {
	t.Helper()
	api := httptest.NewServer(apiHandler)
	t.Cleanup(api.Close)

	server := mcp.NewServer(client.NewClient(api.URL, "tok"), "test")
	ct, st := sdk.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
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
	for _, want := range []string{"burrow_deploy", "burrow_status", "burrow_logs", "burrow_rollback", "burrow_scale", "burrow_domain_add", "burrow_domain_remove", "burrow_providers", "burrow_secret_list", "burrow_secret_unset", "burrow_addon_attach", "burrow_addon_backup", "burrow_addon_backups"} {
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
			if prop == "token" || prop == "auth" {
				t.Errorf("tool %q exposes a %q input: a credential value must never cross MCP", tool.Name, prop)
			}
			// The Postgres attach tool (and every tool) must never accept a database password or
			// connection string: burrowd generates the DATABASE_URL server-side (ADR-0031). No tool
			// input names a connection-string-shaped secret. (`value` is allowed: env set carries a
			// non-secret env value, and there is no secret-set tool.)
			switch prop {
			case "password", "url", "database_url", "connection_string", "dsn":
				t.Errorf("tool %q exposes a %q input: a database secret value must never cross MCP", tool.Name, prop)
			}
		}
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
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "requested 9 exceeds the ceiling of 5", "code": "replica_ceiling"})
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
	if !strings.Contains(text.String(), "replica_ceiling") {
		t.Errorf("error content = %q, want it to mention the guardrail code", text.String())
	}
}
