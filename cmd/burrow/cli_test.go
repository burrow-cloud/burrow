// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI runs the CLI against a mock control-plane API, appending the config flags so the
// commands point at the test server regardless of the ambient environment.
func runCLI(t *testing.T, h http.HandlerFunc, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	full := append(append([]string{}, args...), "--control-plane", srv.URL, "--token", "x")
	var out, errb bytes.Buffer
	err = run(context.Background(), full, &out, &errb)
	return out.String(), errb.String(), err
}

func TestDeploy(t *testing.T) {
	var gotMethod, gotPath string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release":               map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed", "replicas": 2},
			"superseded_release_id": "r0",
		})
	}, "app", "deploy", "web", "--image", "img:1", "--replicas", "2")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/deploy" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(out, "deployed web as release r1") || !strings.Contains(out, "superseded release r0") {
		t.Errorf("output = %q", out)
	}
}

func TestAudit(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{
				{"id": 2, "timestamp": "2026-06-23T12:00:01Z", "operation": "deploy", "target": "web", "principal": "shared-agent", "outcome": "executed"},
				{"id": 1, "timestamp": "2026-06-23T12:00:00Z", "operation": "deploy", "target": "web", "guardrail_code": "", "disposition": "allow", "outcome": "allowed"},
			},
		})
	}, "audit", "--app", "web", "--operation", "deploy", "--outcome", "executed", "--limit", "10")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/v1/audit" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	for _, want := range []string{"app=web", "operation=deploy", "outcome=executed", "limit=10"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
	// Tabular output names the columns (including PRINCIPAL) and the rows; the principal cell
	// carries the shared-agent actor (ADR-0038).
	for _, want := range []string{"TIME", "OP", "TARGET", "PRINCIPAL", "OUTCOME", "GUARDRAIL", "deploy", "web", "shared-agent", "executed", "allowed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
	// The "allowed" row carries no principal, so its PRINCIPAL cell renders as a standalone dash
	// (matching how other empty fields render). The "executed" row's "shared-agent" must not bleed
	// onto it.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "allowed") {
			continue
		}
		fields := strings.Fields(line) // TIME(date) TIME(clock) OP TARGET PRINCIPAL OUTCOME GUARDRAIL
		if len(fields) < 7 {
			t.Fatalf("allowed row has too few columns: %q", line)
		}
		if principal := fields[4]; principal != "-" {
			t.Errorf("allowed row PRINCIPAL cell = %q, want dash for empty principal", principal)
		}
	}
}

func TestAuditJSON(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{{"id": 1, "operation": "rollback", "target": "web", "outcome": "executed"}},
		})
	}, "audit", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if len(entries) != 1 || entries[0]["operation"] != "rollback" {
		t.Errorf("json entries = %v", entries)
	}
}

func TestAuditEmpty(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": []any{}})
	}, "audit")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "No audit records match.") {
		t.Errorf("output = %q, want the empty message", out)
	}
}

func TestDeployJSON(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"release": map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed", "replicas": 1}})
	}, "app", "deploy", "web", "--image", "img:1", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if _, ok := res["release"]; !ok {
		t.Errorf("json output missing release: %s", out)
	}
}

// deployBody runs `app deploy` against a mock control plane and returns the decoded request
// body. It calls run() directly (not runCLI) so the connection flags sit before any --
// separator — as they must on a real command line, since -- consumes everything after it.
func deployBody(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"release": map[string]any{"id": "r1", "image": "img:1", "status": "deployed", "replicas": 1}})
	}))
	defer srv.Close()
	full := append([]string{"app", "deploy", "web", "--image", "img:1", "--control-plane", srv.URL, "--token", "x"}, args...)
	var out, errb bytes.Buffer
	if err := run(context.Background(), full, &out, &errb); err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	return gotBody
}

func TestDeployCommandOverride(t *testing.T) {
	body := deployBody(t, "--", "sh", "-c", "echo hi")
	cmd, ok := body["command"].([]any)
	if !ok || len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[2] != "echo hi" {
		t.Errorf("command in body = %#v, want [sh -c \"echo hi\"]", body["command"])
	}
}

func TestDeployNoCommandOmitsIt(t *testing.T) {
	body := deployBody(t) // no -- separator
	if _, present := body["command"]; present {
		t.Errorf("command should be omitted when no -- args given, got %#v", body["command"])
	}
}

func TestDeployMetricsPort(t *testing.T) {
	body := deployBody(t, "--metrics-port", "9090")
	if got, ok := body["metrics_port"].(float64); !ok || int(got) != 9090 {
		t.Errorf("metrics_port in body = %#v, want 9090", body["metrics_port"])
	}
}

func TestDeployNoMetricsPortOmitsIt(t *testing.T) {
	body := deployBody(t) // no --metrics-port flag
	if _, present := body["metrics_port"]; present {
		t.Errorf("metrics_port should be omitted when the flag is unset, got %#v", body["metrics_port"])
	}
}

func TestAppList(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/apps" {
			t.Errorf("request = %s %s, want GET /v1/apps", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apps": []map[string]any{
			{"app": "web", "image": "nginx:alpine", "desired_replicas": 2, "ready_replicas": 2, "available": true},
		}})
	}, "app", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "web") || !strings.Contains(out, "nginx:alpine") || !strings.Contains(out, "2/2") {
		t.Errorf("output = %q", out)
	}
}

func TestAddonInstallAndList(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/addons" {
			t.Errorf("request = %s %s, want POST /v1/addons", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "burrow-logs", "type": "logs", "mode": "installed",
			"image": "victoria-logs:1", "endpoint": "burrow-logs.burrow.svc:9428", "capabilities": []string{"logs"},
		})
	}, "addon", "install", "logs", "--confirm")
	if err != nil {
		t.Fatalf("addon install: %v", err)
	}
	if !strings.Contains(out, "burrow-logs") || !strings.Contains(out, "logs") {
		t.Errorf("output = %q", out)
	}

	out, _, err = runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/addons" {
			t.Errorf("request = %s %s, want GET /v1/addons", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"addons": []map[string]any{
			{"name": "burrow-logs", "type": "logs", "mode": "installed", "endpoint": "burrow-logs.burrow.svc:9428", "capabilities": []string{"logs"}},
		}})
	}, "addon", "list")
	if err != nil {
		t.Fatalf("addon list: %v", err)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "burrow-logs") || !strings.Contains(out, "installed") {
		t.Errorf("list output = %q", out)
	}
}

func TestAddonLogsQuery(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/logs/query" {
			t.Errorf("request = %s %s, want POST /v1/logs/query", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": []map[string]any{
			{"time": "2026-06-27T00:00:00Z", "message": "boom", "pod": "web-1"},
		}})
	}, "addon", "logs", "error")
	if err != nil {
		t.Fatalf("addon logs: %v", err)
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "web-1") {
		t.Errorf("output = %q", out)
	}
}

func TestStatus(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "has_release": true, "running": true,
			"release":  map[string]any{"id": "r1", "image": "img:1", "status": "deployed"},
			"workload": map[string]any{"desired_replicas": 3, "ready_replicas": 3, "available": true},
		})
	}, "app", "status", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "workload: 3/3 replicas ready, available") {
		t.Errorf("output = %q", out)
	}
}

func TestLogs(t *testing.T) {
	out, errOut, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("tail"); got != "2" {
			t.Errorf("tail query = %q, want 2", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lines": []map[string]any{{"pod": "web-1", "message": "hello"}}})
	}, "app", "logs", "web", "--tail", "2")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The log lines go to stdout, clean of any metadata so `burrow app logs web | grep` stays usable.
	if !strings.Contains(out, "web-1  hello") {
		t.Errorf("stdout = %q, want the log line", out)
	}
	if strings.Contains(out, "Source:") || strings.Contains(out, "targeting") || strings.Contains(out, "───") {
		t.Errorf("stdout = %q, want no metadata (source/targeting/divider) on stdout", out)
	}
	// The metadata leads on stderr: targeting, then the source note, then a divider — all before
	// the logs so it is never missed and would still appear ahead of a stream.
	divider := strings.Repeat("─", 60)
	targetIdx := strings.Index(errOut, "targeting")
	sourceIdx := strings.Index(errOut, "Source: live Kubernetes pod logs")
	dividerIdx := strings.Index(errOut, divider)
	if targetIdx < 0 || sourceIdx < 0 || dividerIdx < 0 {
		t.Fatalf("stderr = %q, want targeting, source note, and divider", errOut)
	}
	if !(targetIdx < sourceIdx && sourceIdx < dividerIdx) {
		t.Errorf("stderr order = targeting@%d source@%d divider@%d, want targeting < source < divider in %q",
			targetIdx, sourceIdx, dividerIdx, errOut)
	}
}

func TestScale(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "previous_replicas": 2, "replicas": 4})
	}, "app", "scale", "web", "4")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "scaled web from 2 to 4") {
		t.Errorf("output = %q", out)
	}
}

func TestConfigSet(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "LOG_LEVEL"})
	}, "app", "config", "set", "web", "LOG_LEVEL=debug")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/config" {
		t.Errorf("request = %s %s, want POST /v1/apps/web/config", gotMethod, gotPath)
	}
	if gotBody["key"] != "LOG_LEVEL" || gotBody["value"] != "debug" {
		t.Errorf("body = %#v, want key=LOG_LEVEL value=debug", gotBody)
	}
	if nr, _ := gotBody["no_restart"].(bool); nr {
		t.Errorf("no_restart = true, want false by default")
	}
	if !strings.Contains(out, "set LOG_LEVEL on web") {
		t.Errorf("output = %q", out)
	}
}

func TestConfigSetNoRestart(t *testing.T) {
	var gotBody map[string]any
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "K"})
	}, "app", "config", "set", "web", "K=V", "--no-restart")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if nr, _ := gotBody["no_restart"].(bool); !nr {
		t.Errorf("no_restart = %#v, want true", gotBody["no_restart"])
	}
	if !strings.Contains(out, "lands on next deploy") {
		t.Errorf("output = %q, want a no-restart note", out)
	}
}

func TestConfigList(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/apps/web/config" {
			t.Errorf("request = %s %s, want GET /v1/apps/web/config", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"config": map[string]string{"B": "2", "A": "1"}})
	}, "app", "config", "list", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Keys are printed sorted, one KEY=VALUE per line.
	if out != "A=1\nB=2\n" {
		t.Errorf("output = %q, want sorted A=1\\nB=2\\n", out)
	}
}

func TestConfigUnset(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "K"})
	}, "app", "config", "unset", "web", "K", "--no-restart")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/apps/web/config/K" {
		t.Errorf("request = %s %s, want DELETE /v1/apps/web/config/K", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "no_restart=true") {
		t.Errorf("query = %q, want no_restart=true", gotQuery)
	}
	if !strings.Contains(out, "unset K on web") {
		t.Errorf("output = %q", out)
	}
}

func TestAppDelete(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/v1/apps/web" {
			t.Errorf("request = %s %s, want DELETE /v1/apps/web", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("confirm"); got != "true" {
			t.Errorf("confirm query = %q, want true", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web"})
	}, "app", "delete", "web", "--confirm")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "deleted app web") {
		t.Errorf("output = %q", out)
	}
}

func TestRollback(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release":                   map[string]any{"id": "r3", "image": "img:1", "status": "deployed"},
			"rolled_back_to_release_id": "r1", "superseded_release_id": "r2",
		})
	}, "app", "rollback", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "rolled web back to release r1") || !strings.Contains(out, "as release r3") {
		t.Errorf("output = %q", out)
	}
}

func TestReachabilityCommand(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/apps/web/reachability" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "summary": "web is reachable at http://web.example.com", "reachable": true})
	}, "app", "reachability", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "reachable at http://web.example.com") {
		t.Errorf("output = %q", out)
	}
}

func TestReachabilityWaitLive(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		// Already live on the first check, so --wait converges without polling.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "reachable": true, "url": "https://web.example.com",
		})
	}, "app", "reachability", "web", "--wait")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "web is live at https://web.example.com") {
		t.Errorf("output = %q, want the live URL", out)
	}
}

func TestReachabilityWaitTimeout(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "reachable": false, "blocked_on": "tls certificate",
		})
	}, "app", "reachability", "web", "--wait", "--timeout", "1ms")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "not reachable after 1ms: waiting on tls certificate") {
		t.Errorf("output = %q, want the blocked-on message", out)
	}
}

func TestExposeCommand(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/apps/web/expose" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "host": "web.example.com", "port": 8080, "url": "http://web.example.com"})
	}, "app", "publish", "web", "--host", "web.example.com", "--port", "8080")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "published web at web.example.com") {
		t.Errorf("output = %q", out)
	}
}

func TestGuardList(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/guard" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.scale_to_zero", "disposition": "confirm", "description": "scale an app to zero"},
		}})
	}, "guard", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "app.scale_to_zero") || !strings.Contains(out, "confirm") {
		t.Errorf("output = %q", out)
	}
}

// TestGuardSetEnv confirms `guard set --env prod` scopes the set to the environment: the env query
// reaches the API and the confirmation names the environment (ADR-0035 phase 2c).
func TestGuardSetEnv(t *testing.T) {
	var gotMethod, gotPath, gotEnv string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotEnv = r.Method, r.URL.Path, r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.delete", "disposition": "deny", "description": "delete an app", "source": "env"},
		}})
	}, "guard", "set", "app.delete", "deny", "--env", "prod")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/guard/app.delete" {
		t.Errorf("request = %s %s, want PUT /v1/guard/app.delete", gotMethod, gotPath)
	}
	if gotEnv != "prod" {
		t.Errorf("env query = %q, want prod", gotEnv)
	}
	if !strings.Contains(out, `set guardrail "app.delete" to "deny" in environment "prod"`) {
		t.Errorf("output = %q, want it to name the environment", out)
	}
}

// TestGuardSetEnvAppDeploy confirms the marquee `guard set --env prod app.deploy confirm` round-trips:
// the app.deploy code reaches the API path and the env selector is prod (ADR-0007, ADR-0035 phase 2c).
func TestGuardSetEnvAppDeploy(t *testing.T) {
	var gotMethod, gotPath, gotEnv string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotEnv = r.Method, r.URL.Path, r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.deploy", "disposition": "confirm", "description": "deploy a new release of an application", "source": "env"},
		}})
	}, "guard", "set", "app.deploy", "confirm", "--env", "prod")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/guard/app.deploy" {
		t.Errorf("request = %s %s, want PUT /v1/guard/app.deploy", gotMethod, gotPath)
	}
	if gotEnv != "prod" {
		t.Errorf("env query = %q, want prod", gotEnv)
	}
	if !strings.Contains(out, `set guardrail "app.deploy" to "confirm" in environment "prod"`) {
		t.Errorf("output = %q, want it to name the app.deploy guardrail and the environment", out)
	}
}

// TestGuardSetNoEnvOmitsSelector confirms `guard set` without --env sends no env selector, so the
// server sets the global disposition (ADR-0035 phase 2c).
func TestGuardSetNoEnvOmitsSelector(t *testing.T) {
	var gotEnv string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotEnv = r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.delete", "disposition": "deny", "description": "delete an app"},
		}})
	}, "guard", "set", "app.delete", "deny")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotEnv != "" {
		t.Errorf("env query = %q, want empty (global)", gotEnv)
	}
	if strings.Contains(out, "environment") {
		t.Errorf("output = %q, should not name an environment without --env", out)
	}
}

// TestGuardListEnv confirms `guard list --env prod` requests the environment's policy and renders the
// SOURCE column marking env-specific vs inherited dispositions (ADR-0035 phase 2c).
func TestGuardListEnv(t *testing.T) {
	var gotEnv string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotEnv = r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
			{"code": "app.delete", "disposition": "deny", "description": "delete an app", "source": "env"},
			{"code": "app.rollback", "disposition": "allow", "description": "roll back", "source": "global"},
		}})
	}, "guard", "list", "--env", "prod")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotEnv != "prod" {
		t.Errorf("env query = %q, want prod", gotEnv)
	}
	for _, want := range []string{"SOURCE", "app.delete", "environment", "inherited (global)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestDeployGuardrailErrorSurfaces(t *testing.T) {
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "exceeds the replica ceiling", "code": "app.replica_ceiling"})
	}, "app", "deploy", "web", "--image", "img:1", "--replicas", "99")
	if err == nil || !strings.Contains(err.Error(), "app.replica_ceiling") {
		t.Fatalf("err = %v, want it to surface the guardrail code", err)
	}
}

func TestExplicitControlPlaneNeedsToken(t *testing.T) {
	t.Setenv("BURROW_API_TOKEN", "")
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"app", "status", "web", "--control-plane", "http://example"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("err = %v, want a missing-token error", err)
	}
}

func TestAutoConnectFailsWithoutCluster(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	// No --control-plane → auto-connect via kubeconfig; a bogus kubeconfig path fails
	// deterministically regardless of the ambient environment.
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"app", "status", "web", "--kubeconfig", filepath.Join(t.TempDir(), "missing")}, &out, &errb)
	if err == nil {
		t.Fatalf("expected an error when the kubeconfig is unreadable")
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"frobnicate"}, &out, &errb); err == nil {
		t.Fatalf("expected an error for an unknown command")
	}
}

// --- build-and-push (fake runner, no Docker) ---

type fakeRunner struct {
	calls   [][]string
	failArg string // fail when the first command arg matches (e.g. "build")
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.failArg != "" && len(args) > 0 && args[0] == f.failArg {
		return errors.New("command failed")
	}
	return nil
}

func TestBuildAndPush(t *testing.T) {
	f := &fakeRunner{}
	if err := buildAndPush(context.Background(), "./app", "registry.example.com/web:1", f.run); err != nil {
		t.Fatalf("buildAndPush: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v, want 2 (build, push)", f.calls)
	}
	build := strings.Join(f.calls[0], " ")
	push := strings.Join(f.calls[1], " ")
	if build != "docker build -t registry.example.com/web:1 ./app" {
		t.Errorf("build call = %q", build)
	}
	if push != "docker push registry.example.com/web:1" {
		t.Errorf("push call = %q", push)
	}
}

func TestBuildAndPushPropagatesError(t *testing.T) {
	f := &fakeRunner{failArg: "build"}
	err := buildAndPush(context.Background(), "./app", "img:1", f.run)
	if err == nil || !strings.Contains(err.Error(), "docker build") {
		t.Fatalf("err = %v, want a docker build error", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("push should not run after build fails; calls = %v", f.calls)
	}
}

func TestBuildAndPushRequiresImage(t *testing.T) {
	if err := buildAndPush(context.Background(), "./app", "", (&fakeRunner{}).run); err == nil {
		t.Fatalf("expected an error when --image is empty")
	}
}

func TestKVFlag(t *testing.T) {
	var f kvFlag
	if err := f.Set("KEY=value=with=eq"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if f.m["KEY"] != "value=with=eq" {
		t.Errorf("m = %v", f.m)
	}
	if err := f.Set("noequals"); err == nil {
		t.Errorf("expected an error for a flag without =")
	}
}

// TestHelpLayoutKubectlStyle confirms every help screen renders in kubectl's order: the single
// Usage line sits at the bottom, after the Flags block (and thus after the description), and no help
// screen leaks an internal ADR reference into user-facing copy.
func TestHelpLayoutKubectlStyle(t *testing.T) {
	configWithEnv(t) // a config is present so the root `-h` never mixes in the first-run banner

	screens := [][]string{
		{"-h"},
		{"install", "-h"},
		{"upgrade", "-h"},
		{"mcp", "-h"},
		{"cluster", "-h"},
		{"config", "-h"},
		{"config", "provider", "add", "-h"},
		{"env", "-h"},
		{"env", "add", "-h"},
		{"env", "list", "-h"},
		{"app", "-h"},
		{"app", "deploy", "-h"},
		{"app", "secret", "set", "-h"},
		{"addon", "-h"},
		{"addon", "connect", "-h"},
		{"guard", "-h"},
		{"audit", "-h"},
		{"version", "-h"},
	}
	for _, args := range screens {
		var out, errb bytes.Buffer
		if err := run(context.Background(), args, &out, &errb); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, errb.String())
		}
		s := out.String() + errb.String()

		// No help screen may expose an internal ADR reference in user-facing copy.
		if strings.Contains(s, "ADR") {
			t.Errorf("%v help leaks an ADR reference:\n%s", args, s)
		}

		// Usage is a single line at the bottom.
		usage := strings.LastIndex(s, "Usage:")
		if usage < 0 {
			t.Errorf("%v help has no Usage line:\n%s", args, s)
			continue
		}
		// It must render after the description (never at the very top).
		if usage == 0 {
			t.Errorf("%v help puts Usage at the very top:\n%s", args, s)
		}
		// When a Flags block is present, Usage comes after it (kubectl order).
		if f := strings.Index(s, "Flags:"); f >= 0 && f > usage {
			t.Errorf("%v help renders Flags after Usage; want Usage at the bottom:\n%s", args, s)
		}
	}
}

func TestExactArgsErrorNamesArgsAndPrintsUsage(t *testing.T) {
	// A missing <app> names the argument and prints the command's usage, not Cobra's bare
	// "accepts 1 arg(s), received 0".
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"app", "status"}, &out, &errb)
	if err == nil {
		t.Fatal("status with no arg should error")
	}
	s := errb.String()
	if !strings.Contains(s, "burrow app status needs <app>.") {
		t.Errorf("missing-arg message = %q, want it to name <app>", s)
	}
	if !strings.Contains(s, "Usage:") || !strings.Contains(s, "burrow app status <app>") {
		t.Errorf("should print the command usage, got %q", s)
	}

	// A two-arg command names both placeholders.
	errb.Reset()
	_ = run(context.Background(), []string{"app", "scale"}, &out, &errb)
	if !strings.Contains(errb.String(), "burrow app scale needs <app> <replicas>.") {
		t.Errorf("scale message = %q, want both args named", errb.String())
	}

	// Too many args is reported too.
	errb.Reset()
	_ = run(context.Background(), []string{"app", "status", "web", "extra"}, &out, &errb)
	if !strings.Contains(errb.String(), "takes only <app>") {
		t.Errorf("extra-arg message = %q", errb.String())
	}
}

func TestAutoscaleDefaults(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "env": "default", "min_replicas": 1, "max_replicas": 10, "cpu_percent": 80,
			"metrics_available": true,
		})
	}, "app", "autoscale", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/autoscale" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	// The flag defaults (min 1, max 10, cpu 80) travel in the request body.
	if !strings.Contains(gotBody, `"min":1`) || !strings.Contains(gotBody, `"max":10`) || !strings.Contains(gotBody, `"cpu":80`) {
		t.Errorf("body = %s, want defaults 1/10/80", gotBody)
	}
	if !strings.Contains(out, "set web to autoscale between 1 and 10 replicas at 80% CPU") {
		t.Errorf("output = %q", out)
	}
}

func TestAutoscaleCPUFlag(t *testing.T) {
	var gotBody string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "env": "default", "min_replicas": 1, "max_replicas": 10, "cpu_percent": 90,
			"metrics_available": true,
		})
	}, "app", "autoscale", "web", "--cpu", "90")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(gotBody, `"cpu":90`) {
		t.Errorf("body = %s, want cpu 90", gotBody)
	}
	if !strings.Contains(out, "at 90% CPU") {
		t.Errorf("output = %q", out)
	}
}

func TestAutoscaleMetricsWarning(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "env": "default", "min_replicas": 1, "max_replicas": 10, "cpu_percent": 80,
			"metrics_available": false,
			"warning":           "autoscaling needs metrics-server, which was not detected. The autoscaler is set but will not scale until metrics-server is installed.",
		})
	}, "app", "autoscale", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "Note: autoscaling needs metrics-server, which was not detected.") {
		t.Errorf("output = %q, want the metrics-absent note", out)
	}
	if strings.Contains(out, "—") {
		t.Errorf("output must not contain an em-dash: %q", out)
	}
}

func TestAutoscaleOff(t *testing.T) {
	var gotMethod, gotPath string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}, "app", "autoscale", "web", "off")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/apps/web/autoscale" {
		t.Errorf("request = %s %s, want DELETE /v1/apps/web/autoscale", gotMethod, gotPath)
	}
	if !strings.Contains(out, "turned autoscaling off for web") {
		t.Errorf("output = %q", out)
	}
}
