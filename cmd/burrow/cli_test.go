// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	}, "app", "deploy", "web", "--image", "img:1", "--replicas", "2", "--env", "K=V")
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
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("tail"); got != "2" {
			t.Errorf("tail query = %q, want 2", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lines": []map[string]any{{"pod": "web-1", "message": "hello"}}})
	}, "app", "logs", "web", "--tail", "2")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "web-1  hello") {
		t.Errorf("output = %q", out)
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
			{"code": "scale_to_zero", "disposition": "confirm", "description": "scale an app to zero"},
		}})
	}, "guard", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "scale_to_zero") || !strings.Contains(out, "confirm") {
		t.Errorf("output = %q", out)
	}
}

func TestDeployGuardrailErrorSurfaces(t *testing.T) {
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "exceeds the replica ceiling", "code": "replica_ceiling"})
	}, "app", "deploy", "web", "--image", "img:1", "--replicas", "99")
	if err == nil || !strings.Contains(err.Error(), "replica_ceiling") {
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
