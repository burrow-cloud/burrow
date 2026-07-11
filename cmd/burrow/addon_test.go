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
	"path/filepath"
	"strings"
	"testing"
)

// isolateConfig points $BURROW_CONFIG at a temp file so a test never reads or writes the user's real
// local config while a command resolves the active environment.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
}

// TestAddonInstallNoArgListsAvailableAndInstalled asserts `addon install` with no name lists the
// installable add-ons, marks which are installed (from a stubbed Addons lookup), prints the install
// hint, and never uses the word "capability".
func TestAddonInstallNoArgListsAvailableAndInstalled(t *testing.T) {
	isolateConfig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the logs add-on is installed.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"addons": []map[string]any{
				{"name": "burrow-logs", "type": "logs", "mode": "installed", "endpoint": "logs.svc:9428", "capabilities": []string{"logs"}, "ready": true},
			},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "install", "--control-plane", srv.URL, "--token", "tok"}, &out, &errb)
	if err != nil {
		t.Fatalf("addon install (no arg): %v (stderr: %s)", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Available add-ons:",
		"NAME", "INSTALLED", "DESCRIPTION",
		"logs", "metrics", "cache", "postgres",
		"Install one with `burrow addon install <name>`.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("listing missing %q:\n%s", want, s)
		}
	}
	// logs is installed (yes); the others are not (no).
	if !lineHasCols(s, "logs", "yes") {
		t.Errorf("logs should be marked installed (yes):\n%s", s)
	}
	for _, name := range []string{"metrics", "cache", "postgres"} {
		if !lineHasCols(s, name, "no") {
			t.Errorf("%s should be marked not installed (no):\n%s", name, s)
		}
	}
	// The "capability" vocabulary is dropped from the install command's output and help.
	if strings.Contains(strings.ToLower(s), "capabilit") {
		t.Errorf("install listing must not use the word \"capability\":\n%s", s)
	}
	install := newAddonInstallCmd()
	if strings.Contains(strings.ToLower(install.Short+install.Long), "capabilit") {
		t.Errorf("install help must not use the word \"capability\": short=%q long=%q", install.Short, install.Long)
	}
}

// TestAddonInstallNoArgGracefulWhenUnreachable asserts the no-arg listing still prints the available
// add-ons when no cluster is reachable: the INSTALLED column blanks to "-", a connect hint is shown,
// and the command does not error.
func TestAddonInstallNoArgGracefulWhenUnreachable(t *testing.T) {
	isolateConfig(t)
	// A server we immediately close, so the Addons lookup fails fast (connection refused).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "install", "--control-plane", url, "--token", "tok"}, &out, &errb)
	if err != nil {
		t.Fatalf("addon install (no arg, unreachable): %v (stderr: %s)", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Available add-ons:",
		"logs", "metrics", "cache", "postgres",
		"Connect to a cluster to see which are installed",
		"Install one with `burrow addon install <name>`.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("graceful listing missing %q:\n%s", want, s)
		}
	}
	// The INSTALLED column blanks to "-" (neither yes nor no) when nothing could be probed.
	if strings.Contains(s, "yes") {
		t.Errorf("unreachable listing must not claim anything is installed:\n%s", s)
	}
}

// TestAddonInstallMetricsSelfHealsRBACBeforeAPI asserts the 1-arg metrics install stages the metrics
// RBAC kubeconfig-side (through the applyFn seam) BEFORE calling the install API, and that the applied
// manifest is the vmagent ServiceAccount + pod-discovery Role/RoleBinding.
func TestAddonInstallMetricsSelfHealsRBACBeforeAPI(t *testing.T) {
	isolateConfig(t)

	var order []string
	var appliedManifest string
	origApply := applyFn
	applyFn = func(_ context.Context, _, _ string, manifests string, _ bool, _, _ io.Writer) error {
		order = append(order, "apply")
		appliedManifest = manifests
		return nil
	}
	defer func() { applyFn = origApply }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "install")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "burrow-metrics", "type": "metrics", "mode": "installed", "image": "victoria-metrics:test",
			"endpoint": "metrics.svc:8428", "capabilities": []string{"metrics"},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "install", "metrics", "--confirm", "--control-plane", srv.URL, "--token", "tok"}, &out, &errb)
	if err != nil {
		t.Fatalf("addon install metrics: %v (stderr: %s)", err, errb.String())
	}
	if len(order) != 2 || order[0] != "apply" || order[1] != "install" {
		t.Fatalf("expected RBAC apply before the install API call, got order %v", order)
	}
	for _, want := range []string{"name: burrow-vmagent", "kind: ServiceAccount", "kind: Role", "kind: RoleBinding", `verbs: ["get", "list", "watch"]`} {
		if !strings.Contains(appliedManifest, want) {
			t.Errorf("applied RBAC manifest missing %q:\n%s", want, appliedManifest)
		}
	}
	if !strings.Contains(out.String(), "Preparing metrics RBAC") {
		t.Errorf("metrics install should announce staging the RBAC:\n%s", out.String())
	}
}

// TestAddonInstallNonMetricsStagesNoRBAC asserts logs, cache, and postgres installs apply NO
// kubeconfig-side RBAC: they have no per-add-on grant, so the self-heal path is a no-op.
func TestAddonInstallNonMetricsStagesNoRBAC(t *testing.T) {
	for _, name := range []string{"logs", "cache", "postgres"} {
		t.Run(name, func(t *testing.T) {
			isolateConfig(t)
			applied := false
			origApply := applyFn
			applyFn = func(_ context.Context, _, _ string, _ string, _ bool, _, _ io.Writer) error {
				applied = true
				return nil
			}
			defer func() { applyFn = origApply }()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"name": "burrow-" + name, "type": name, "mode": "installed",
					"endpoint": name + ".svc:1234", "capabilities": []string{name},
				})
			}))
			defer srv.Close()

			var out, errb bytes.Buffer
			err := run(context.Background(), []string{"addon", "install", name, "--confirm", "--control-plane", srv.URL, "--token", "tok"}, &out, &errb)
			if err != nil {
				t.Fatalf("addon install %s: %v (stderr: %s)", name, err, errb.String())
			}
			if applied {
				t.Errorf("%s install must not stage any kubeconfig-side RBAC", name)
			}
		})
	}
}

// TestMetricsRBACTemplateRenders asserts the embedded metrics RBAC template parses and renders, with
// the vmagent ServiceAccount in the add-on namespace and the pod-discovery Role/RoleBinding in the
// app namespace.
func TestMetricsRBACTemplateRenders(t *testing.T) {
	var sb strings.Builder
	err := metricsRBACTemplate.Execute(&sb, struct {
		AddonNamespace        string
		AppNamespace          string
		ControlPlaneNamespace string
	}{AddonNamespace: "addons-ns", AppNamespace: "apps-ns", ControlPlaneNamespace: "cp-ns"})
	if err != nil {
		t.Fatalf("rendering metrics RBAC template: %v", err)
	}
	s := sb.String()
	for _, want := range []string{
		"kind: ServiceAccount",
		"name: burrow-vmagent",
		"namespace: addons-ns", // the vmagent ServiceAccount lives in the add-on namespace
		"namespace: apps-ns",   // the Role/RoleBinding live in the app namespace
		`resources: ["pods"]`,
		`verbs: ["get", "list", "watch"]`,
		"kind: RoleBinding",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered metrics RBAC missing %q:\n%s", want, s)
		}
	}
	// The grant is pod-discovery only: no write verbs leak in.
	for _, banned := range []string{"create", "update", "patch", "delete"} {
		if strings.Contains(s, banned) {
			t.Errorf("metrics RBAC must be read-only on pods but mentions %q:\n%s", banned, s)
		}
	}
	// With distinct app and add-on namespaces, vmagent also gets pod discovery in the add-on
	// namespace (where the Postgres exporter runs) — so a Role/RoleBinding pair lands in each
	// namespace (ADR-0051): two RoleBindings total, and a Role bound in addons-ns.
	if got := strings.Count(s, "kind: RoleBinding"); got != 2 {
		t.Errorf("expected 2 RoleBindings (app ns + add-on ns), got %d:\n%s", got, s)
	}
}

// TestMetricsRBACTemplateOmitsAddonRoleWhenNamespacesEqual asserts that when the app and add-on
// namespaces are the same, only ONE pod-discovery Role/RoleBinding is emitted — the app-namespace
// Role already covers the add-on namespace, and two identically-named Roles in one namespace would
// collide on apply (ADR-0051).
func TestMetricsRBACTemplateOmitsAddonRoleWhenNamespacesEqual(t *testing.T) {
	var sb strings.Builder
	if err := metricsRBACTemplate.Execute(&sb, struct {
		AddonNamespace        string
		AppNamespace          string
		ControlPlaneNamespace string
	}{AddonNamespace: "shared-ns", AppNamespace: "shared-ns", ControlPlaneNamespace: "cp-ns"}); err != nil {
		t.Fatalf("rendering metrics RBAC template: %v", err)
	}
	s := sb.String()
	if got := strings.Count(s, "kind: RoleBinding"); got != 1 {
		t.Errorf("expected exactly 1 RoleBinding when namespaces are equal, got %d:\n%s", got, s)
	}
	// "kind: Role\nmetadata:" matches only a Role resource header, not a RoleBinding's roleRef
	// (which also reads "kind: Role").
	if got := strings.Count(s, "kind: Role\nmetadata:"); got != 1 {
		t.Errorf("expected exactly 1 Role when namespaces are equal, got %d:\n%s", got, s)
	}
}

// lineHasCols reports whether some line in s contains both col1 and col2 (in that order) — used to
// assert a NAME/INSTALLED row in the listing without depending on exact column widths.
func lineHasCols(s, col1, col2 string) bool {
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, col1); i >= 0 && strings.Contains(line[i+len(col1):], col2) {
			return true
		}
	}
	return false
}

// TestAddonConnectAuthSendsTokenInBody asserts `addon connect --auth` sends the bearer token VALUE
// in the POST body — not a kubeconfig-direct Secret write, and not in the path or query (ADR-0030).
func TestAddonConnectAuthSendsTokenInBody(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "loki", "type": "logs", "mode": "connected",
			"endpoint": "loki.svc:3100", "capabilities": []string{"logs"},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetIn(strings.NewReader("s3cr3t\n")) // piped token (non-terminal)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"addon", "connect", "loki", "--auth", "--endpoint", "loki.svc:3100",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("addon connect --auth: %v (stderr: %s)", err, errb.String())
	}

	if gotPath != "/v1/addons/connect" {
		t.Errorf("path = %q, want /v1/addons/connect", gotPath)
	}
	if strings.Contains(gotPath, "s3cr3t") || strings.Contains(gotQuery, "s3cr3t") {
		t.Errorf("token leaked into the request path/query: path=%q query=%q", gotPath, gotQuery)
	}
	if !strings.Contains(gotBody, `"token":"s3cr3t"`) {
		t.Errorf("request body missing the token: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"secret_key":"addon-loki"`) {
		t.Errorf("request body missing the secret key: %s", gotBody)
	}
	if strings.Contains(out.String(), "s3cr3t") {
		t.Errorf("CLI output leaked the token value:\n%s", out.String())
	}
}

// TestAddonConnectUnauthenticatedSendsNoToken asserts a plain `addon connect` (no --auth) sends an
// empty token and key — the agent-reachable unauthenticated path is unchanged.
func TestAddonConnectUnauthenticatedSendsNoToken(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "loki", "type": "logs", "mode": "connected",
			"endpoint": "loki.svc:3100", "capabilities": []string{"logs"},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{
		"addon", "connect", "loki", "--endpoint", "loki.svc:3100",
		"--control-plane", srv.URL, "--token", "api-tok",
	})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("addon connect: %v (stderr: %s)", err, errb.String())
	}
	if !strings.Contains(gotBody, `"token":""`) || !strings.Contains(gotBody, `"secret_key":""`) {
		t.Errorf("unauthenticated connect should send empty token and key: %s", gotBody)
	}
}
