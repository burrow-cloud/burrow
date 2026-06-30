// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/localconfig"
)

// targetCapture records that a fake burrowd cluster was contacted and the env name an operation
// carried, so a test can assert which cluster a command routed to and the env NAME it sent.
type targetCapture struct {
	hit bool
	env string
}

// fakeBurrowdEnv stands in for one cluster's burrowd: it serves the install token Secret and an
// app-status response, recording the env query the status call carried. It mirrors
// fakeBurrowdCluster but additionally captures the env selector (ADR-0036 slice 5a).
func fakeBurrowdEnv(c *targetCapture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hit = true
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/secrets/") {
			_ = json.NewEncoder(w).Encode(&corev1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: "burrow"},
				Data:       map[string][]byte{"token": []byte("s3cr3t")},
			})
			return
		}
		c.env = r.URL.Query().Get("env")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "has_release": true, "running": true,
			"release":  map[string]any{"id": "r1", "image": "img:1", "status": "deployed"},
			"workload": map[string]any{"desired_replicas": 1, "ready_replicas": 1, "available": true},
		})
	}))
}

// TestPinnedHandleRoutesToHandleContextAndEnv confirms a pinned handle decides both the cluster
// (its context, not the current kube context) and the env NAME sent to burrowd, and that the
// resolved target is printed to stderr (ADR-0036 slice 5a).
func TestPinnedHandleRoutesToHandleContextAndEnv(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var stg, prod targetCapture
	staging := fakeBurrowdEnv(&stg)
	prodSrv := fakeBurrowdEnv(&prod)
	defer staging.Close()
	defer prodSrv.Close()

	// Current kube context is staging; pin the prod handle so it must win.
	kc := writeKubeconfig(t, twoContextConfig(staging.URL, prodSrv.URL))
	cfg := &localconfig.Config{
		Current: "prod",
		Environments: []localconfig.Environment{
			// The env NAME ("prod") is distinct from the app namespace ("team-prod"), so the
			// assertion below proves the NAME is sent, not the namespace.
			{Name: "prod", Context: "prod", AppNamespace: "team-prod", Env: "prod"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "status", "web", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	if !prod.hit {
		t.Errorf("pinned handle did not route to its context (prod)")
	}
	if stg.hit {
		t.Errorf("the current context (staging) was contacted; the pin should route to prod")
	}
	if prod.env != "prod" {
		t.Errorf("env sent = %q, want the handle's env NAME prod (not the namespace)", prod.env)
	}
	if !strings.Contains(errb.String(), "targeting prod") {
		t.Errorf("stderr target line = %q, want it to name the pinned target", errb.String())
	}
}

// TestFollowModeUsesCurrentContextAndEnv confirms that with nothing pinned, a command targets the
// current kube context and sends the env NAME of the handle registered for that context.
func TestFollowModeUsesCurrentContextAndEnv(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var stg, prod targetCapture
	staging := fakeBurrowdEnv(&stg)
	prodSrv := fakeBurrowdEnv(&prod)
	defer staging.Close()
	defer prodSrv.Close()

	kc := writeKubeconfig(t, twoContextConfig(staging.URL, prodSrv.URL)) // current = staging
	cfg := &localconfig.Config{
		Environments: []localconfig.Environment{
			{Name: "staging", Context: "staging", AppNamespace: "team-x", Env: "staging"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "status", "web", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	if !stg.hit {
		t.Errorf("follow mode did not use the current kube context (staging)")
	}
	if prod.hit {
		t.Errorf("prod was contacted; follow mode should use the current context staging")
	}
	if stg.env != "staging" {
		t.Errorf("env sent = %q, want the followed handle's env staging", stg.env)
	}
	if !strings.Contains(errb.String(), "following kubectl") {
		t.Errorf("stderr target line = %q, want it to show follow mode", errb.String())
	}
}

// TestContextAndEnvOverrideResolved confirms the low-level --context and --env flags override the
// resolved handle: the command targets the named context and sends the raw env name, preserving the
// ADR-0035 escape hatches.
func TestContextAndEnvOverrideResolved(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var stg, prod targetCapture
	staging := fakeBurrowdEnv(&stg)
	prodSrv := fakeBurrowdEnv(&prod)
	defer staging.Close()
	defer prodSrv.Close()

	kc := writeKubeconfig(t, twoContextConfig(staging.URL, prodSrv.URL)) // current = staging
	// Pin staging; the flags must override both the context (to prod) and the env name.
	cfg := &localconfig.Config{
		Current: "staging",
		Environments: []localconfig.Environment{
			{Name: "staging", Context: "staging", AppNamespace: "team-x", Env: "staging"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"app", "status", "web", "--kubeconfig", kc, "--context", "prod", "--env", "hotfix"}, &out, &errb)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	if !prod.hit {
		t.Errorf("--context prod override did not redirect to prod")
	}
	if stg.hit {
		t.Errorf("staging was contacted despite the --context prod override")
	}
	if prod.env != "hotfix" {
		t.Errorf("env sent = %q, want the --env override hotfix", prod.env)
	}
	if !strings.Contains(errb.String(), "flag override") {
		t.Errorf("stderr = %q, want it to note the flag override", errb.String())
	}
}

// TestTargetLinePrintsToStderrJSONStdoutClean confirms the resolved-target line goes to stderr, so
// stdout (and a --json result) stays clean (ADR-0036).
func TestTargetLinePrintsToStderrJSONStdoutClean(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var cap targetCapture
	srv := fakeBurrowdEnv(&cap)
	defer srv.Close()

	// Both contexts point at the one fake; pin staging so the resolved target is deterministic.
	kc := writeKubeconfig(t, twoContextConfig(srv.URL, srv.URL))
	cfg := &localconfig.Config{
		Current:      "staging",
		Environments: []localconfig.Environment{{Name: "staging", Context: "staging", AppNamespace: "team-x", Env: "staging"}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "status", "web", "--kubeconfig", kc, "--json"}, &out, &errb); err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("--json stdout is not clean JSON: %v\n%s", err, out.String())
	}
	if !strings.Contains(errb.String(), "targeting") {
		t.Errorf("stderr is missing the target line: %q", errb.String())
	}
}

// TestRecordEnvironmentRecordsEmptyEnv confirms install (cluster-per-environment) records a handle
// with an empty Env, so commands send burrowd no env name and get the default namespace and global
// guardrails (ADR-0036).
func TestRecordEnvironmentRecordsEmptyEnv(t *testing.T) {
	tempConfig(t)
	if err := recordEnvironment(installArgs{
		environment:  "prod",
		kubeContext:  "do-nyc1-prod",
		namespace:    "burrow",
		appNamespace: "burrow-apps",
	}, io.Discard); err != nil {
		t.Fatalf("recordEnvironment: %v", err)
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h, ok := cfg.Lookup("prod")
	if !ok {
		t.Fatal("recordEnvironment did not record the handle")
	}
	if h.Env != "" {
		t.Errorf("install handle Env = %q, want empty (cluster-per-environment)", h.Env)
	}
	if cfg.Current != "prod" {
		t.Errorf("current = %q, want the installed env pinned", cfg.Current)
	}
}

// TestEnvAddRecordsEnvName confirms `env add` (namespace-per-environment) records a handle whose
// Env is the registered environment NAME, so later commands send burrowd that name (ADR-0036).
func TestEnvAddRecordsEnvName(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	srv := fakeEnvAPI(t, nil)
	defer srv.Close()

	orig := applyFn
	applyFn = func(_ context.Context, _ string, _ string, _ string, _ bool, _, _ io.Writer) error { return nil }
	defer func() { applyFn = orig }()

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "add", "staging", "--context", "staging-ctx", "--control-plane", srv.URL, "--token", "t"}, &out, &errb); err != nil {
		t.Fatalf("env add: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h, ok := cfg.Lookup("staging")
	if !ok {
		t.Fatal("env add did not record a handle")
	}
	if h.Env != "staging" {
		t.Errorf("env add handle Env = %q, want the registered env name staging", h.Env)
	}
}
