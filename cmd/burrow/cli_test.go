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
	}, "deploy", "web", "--image", "img:1", "--replicas", "2", "--env", "K=V")
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
	}, "deploy", "web", "--image", "img:1", "--json")
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

func TestStatus(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "has_release": true, "running": true,
			"release":  map[string]any{"id": "r1", "image": "img:1", "status": "deployed"},
			"workload": map[string]any{"desired_replicas": 3, "ready_replicas": 3, "available": true},
		})
	}, "status", "web")
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
	}, "logs", "web", "--tail", "2")
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
	}, "scale", "web", "4")
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
	}, "rollback", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "rolled web back to release r1") || !strings.Contains(out, "as release r3") {
		t.Errorf("output = %q", out)
	}
}

func TestExposeCommand(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/apps/web/expose" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "host": "web.example.com", "port": 8080, "url": "http://web.example.com"})
	}, "expose", "web", "--host", "web.example.com", "--port", "8080")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "exposed web at web.example.com") {
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
	}, "deploy", "web", "--image", "img:1", "--replicas", "99")
	if err == nil || !strings.Contains(err.Error(), "replica_ceiling") {
		t.Fatalf("err = %v, want it to surface the guardrail code", err)
	}
}

func TestExplicitControlPlaneNeedsToken(t *testing.T) {
	t.Setenv("BURROW_API_TOKEN", "")
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"status", "web", "--control-plane", "http://example"}, &out, &errb)
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
	err := run(context.Background(), []string{"status", "web", "--kubeconfig", filepath.Join(t.TempDir(), "missing")}, &out, &errb)
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
