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
	connectpkg "github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/mcp"
)

// toolErrorText joins the text content of a tool result, for asserting on an error message.
func toolErrorText(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected a tool error, got success: %v", res.Content)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return text.String()
}

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

// TestReadOnlyToolEchoesEnv confirms the ADR-0047 §3 legibility property: a read-only per-app tool
// called with an explicit env handle echoes the environment it read (the handle name, its kube
// context, and the registered env NAME) in its structured result, alongside the tool's own
// top-level fields, so a survey never silently conflates two environments. It exercises status
// (which embeds the raw client result) and apps (which embeds a wrapper output struct).
func TestReadOnlyToolEchoesEnv(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
    appNamespace: apps
    env: production
`)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/apps") {
			_ = json.NewEncoder(w).Encode(map[string]any{"apps": []map[string]any{{"app": "web", "image": "img:1"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web"})
	}))
	t.Cleanup(api.Close)
	clientFor := func(string) (*client.Client, error) { return client.NewClient(api.URL, "tok"), nil }
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	type envEcho struct {
		Name    string `json:"name"`
		Context string `json:"context"`
		Env     string `json:"env"`
	}
	wantEcho := func(t *testing.T, got envEcho) {
		t.Helper()
		if got.Name != "prod" || got.Context != "do-nyc1-prod" || got.Env != "production" {
			t.Errorf("echoed environment = %+v, want prod/do-nyc1-prod/production", got)
		}
	}

	// status embeds the raw client.StatusResult; its "app" field stays top-level and the echo is added.
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_status",
		Arguments: map[string]any{"app": "web", "env": "prod"},
	})
	if err != nil {
		t.Fatalf("CallTool status: %v", err)
	}
	if res.IsError {
		t.Fatalf("status returned error: %v", res.Content)
	}
	statusOut := decodeStructured[struct {
		App         string  `json:"app"`
		Environment envEcho `json:"environment"`
	}](t, res)
	if statusOut.App != "web" {
		t.Errorf("status app = %q, want web (the raw result is still top-level)", statusOut.App)
	}
	wantEcho(t, statusOut.Environment)

	// apps embeds the wrapper appsOutput; its "apps" list stays and the echo is added.
	res, err = cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_apps",
		Arguments: map[string]any{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("CallTool apps: %v", err)
	}
	if res.IsError {
		t.Fatalf("apps returned error: %v", res.Content)
	}
	appsOut := decodeStructured[struct {
		Apps []struct {
			App string `json:"app"`
		} `json:"apps"`
		Environment envEcho `json:"environment"`
	}](t, res)
	if len(appsOut.Apps) != 1 || appsOut.Apps[0].App != "web" {
		t.Errorf("apps = %+v, want the web app (the listing is still present)", appsOut.Apps)
	}
	wantEcho(t, appsOut.Environment)
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

// TestMutatingRefusesAmbiguousEnv confirms the ADR-0047 forcing function: with more than one
// environment registered, a mutating tool called with no env/context is refused with a structured
// error that names the handles, instead of silently routing the change to whatever context is
// current. The client is never even built — the refusal happens before routing.
func TestMutatingRefusesAmbiguousEnv(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
    appNamespace: apps
    env: production
  - name: jolly-marmot
    context: burrow-vps
    appNamespace: burrow-apps
`)
	clientFor := func(string) (*client.Client, error) {
		t.Error("clientFor must not be called: an ambiguous mutating call must be refused before it routes")
		return nil, nil
	}
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_deploy",
		Arguments: map[string]any{"app": "web", "image": "img:1", "replicas": 1},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error: a mutating deploy with no env and more than one environment must be refused")
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	for _, want := range []string{"prod", "jolly-marmot", "more than one environment", "env argument"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("refusal = %q, want it to contain %q", text.String(), want)
		}
	}
}

// TestReadOnlyToolIgnoresAmbiguity confirms the guard is scoped to mutating operations: a read-only
// tool with no env still routes (to the current context) even with several environments registered,
// so the agent can survey before it acts (ADR-0047 §3).
func TestReadOnlyToolIgnoresAmbiguity(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
  - name: jolly-marmot
    context: burrow-vps
`)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web"})
	}))
	t.Cleanup(api.Close)
	clientFor := func(string) (*client.Client, error) { return client.NewClient(api.URL, "tok"), nil }
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_status",
		Arguments: map[string]any{"app": "web"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("read-only status must not be refused for ambiguity; got: %v", res.Content)
	}
}

// TestUnreachableNamesOtherEnvironments confirms the ADR-0047 §4 stickiness enrichment: when a
// per-app tool's target control plane is unreachable and other environments are registered, the
// tool error still conveys the failure AND names the OTHER registered handles (each as
// "name (context <context>)") so the human can be told where to redirect — while making clear
// Burrow did not switch targets. The failed environment itself is not offered as an alternative.
func TestUnreachableNamesOtherEnvironments(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
    env: production
  - name: staging
    context: do-nyc1-staging
    env: stg
  - name: jolly-marmot
    context: burrow-vps
`)
	// The target (prod) is unreachable; the classifier returns the same typed error connect.Client
	// produces on a dial failure, so the enrichment keys off the real signal, not a message match.
	clientFor := func(kubeContext string) (*client.Client, error) {
		return nil, &connectpkg.UnreachableError{Context: kubeContext, Reason: "connection refused"}
	}
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_deploy",
		Arguments: map[string]any{"app": "web", "image": "img:1", "env": "prod"},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	msg := toolErrorText(t, res)
	// (a) still conveys the underlying unreachable failure, naming the failed context.
	if !strings.Contains(msg, "control plane unreachable") || !strings.Contains(msg, "do-nyc1-prod") {
		t.Errorf("error = %q, want it to still convey the unreachable failure on do-nyc1-prod", msg)
	}
	// (b) names the OTHER registered handles as name (context <context>).
	for _, want := range []string{"other registered environments", "staging (context do-nyc1-staging)", "jolly-marmot (context burrow-vps)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to contain %q", msg, want)
		}
	}
	// The failed environment is not offered back as an alternative to redirect to.
	if _, alternatives, found := strings.Cut(msg, "other registered environments:"); found {
		if strings.Contains(alternatives, "prod (context") {
			t.Errorf("alternatives %q offered the failed environment prod back as a redirect target", alternatives)
		}
	}
	// And it never claims to have switched or retried elsewhere.
	if !strings.Contains(msg, "did not switch") {
		t.Errorf("error = %q, want it to state Burrow did not switch targets", msg)
	}
}

// TestUnreachableSingleHandleNoAlternatives confirms that with only the failed handle registered
// there is nothing to suggest, so the error is the plain unreachable message with no alternatives
// appended.
func TestUnreachableSingleHandleNoAlternatives(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
    env: production
`)
	clientFor := func(kubeContext string) (*client.Client, error) {
		return nil, &connectpkg.UnreachableError{Context: kubeContext, Reason: "connection refused"}
	}
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_deploy",
		Arguments: map[string]any{"app": "web", "image": "img:1", "env": "prod"},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	msg := toolErrorText(t, res)
	if !strings.Contains(msg, "control plane unreachable") {
		t.Errorf("error = %q, want the plain unreachable message", msg)
	}
	if strings.Contains(msg, "other registered environments") {
		t.Errorf("error = %q, want no alternatives with a single registered handle", msg)
	}
}

// TestNonConnectivityErrorNotEnriched confirms the enrichment is scoped to the genuinely-unreachable
// case: a normal control-plane error (here a 4xx from the API) is returned untouched even when other
// environments are registered, so an operation that reached the target and failed on its own terms is
// not mislabeled as a routing problem.
func TestNonConnectivityErrorNotEnriched(t *testing.T) {
	writeHandleConfig(t, `apiVersion: burrow.dev/v1
kind: Config
environments:
  - name: prod
    context: do-nyc1-prod
    env: production
  - name: staging
    context: do-nyc1-staging
    env: stg
`)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "app not found", http.StatusNotFound)
	}))
	t.Cleanup(api.Close)
	clientFor := func(string) (*client.Client, error) { return client.NewClient(api.URL, "tok"), nil }
	cs := newSession(t, mcp.NewServer(clientFor, "", "test"))

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "burrow_status",
		Arguments: map[string]any{"app": "web", "env": "prod"},
	})
	if err != nil {
		t.Fatalf("CallTool transport error: %v", err)
	}
	msg := toolErrorText(t, res)
	if strings.Contains(msg, "other registered environments") {
		t.Errorf("error = %q, a non-connectivity control-plane error must not be enriched with alternatives", msg)
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
