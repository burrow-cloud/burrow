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

	"github.com/burrow-cloud/burrow/client"
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

// TestAddonRemoveDefaultReportsKeptData asserts the operator CLI says what happened to the data. A
// removal that keeps the volume must SAY so — naming the volume, its namespace, and the fact that a
// reinstall reuses it — or the operator cannot tell a preserved database from a destroyed one.
func TestAddonRemoveDefaultReportsKeptData(t *testing.T) {
	isolateConfig(t)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "burrow-postgres", "type": "postgres", "namespace": "burrow-addons",
			"data_deleted": false, "retained_data_volume": "burrow-postgres",
			"retained_backup_volume": "burrow-postgres-backups",
			"attached_apps":          []string{"api", "web"},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "remove", "burrow-postgres", "--confirm", "--control-plane", srv.URL, "--token", "tok"}, &out, &errb)
	if err != nil {
		t.Fatalf("addon remove: %v (stderr: %s)", err, errb.String())
	}
	// Without --delete-data the request must not ask for deletion: the safe default is the wire
	// default too.
	if strings.Contains(gotQuery, "delete_data") {
		t.Errorf("query %q asks to delete data without --delete-data", gotQuery)
	}
	s := out.String()
	for _, want := range []string{
		"removed add-on \"burrow-postgres\"",
		"kept the data volume \"burrow-postgres\"",
		"burrow-addons",
		"reinstalling the add-on reuses it",
		"kubectl -n burrow-addons delete pvc burrow-postgres",
		"kept the backup volume \"burrow-postgres-backups\"",
		"2 attached app(s) (api, web)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("removal output missing %q:\n%s", want, s)
		}
	}
}

// TestAddonRemoveDeleteDataAsksAndReports asserts --delete-data reaches the API and the output states
// the destruction plainly, including which apps lost their database. It runs non-interactively, so it
// carries the acknowledgement flag --delete-data requires with no terminal to type into (ADR-0064 §2).
func TestAddonRemoveDeleteDataAsksAndReports(t *testing.T) {
	isolateConfig(t)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "burrow-postgres", "type": "postgres", "namespace": "burrow-addons",
			"data_deleted": true, "retained_backup_volume": "burrow-postgres-backups",
			"attached_apps": []string{"web"},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "remove", "burrow-postgres", "--delete-data", "--acknowledge-data-loss", "--confirm", "--control-plane", srv.URL, "--token", "tok"}, &out, &errb)
	if err != nil {
		t.Fatalf("addon remove --delete-data: %v (stderr: %s)", err, errb.String())
	}
	if !strings.Contains(gotQuery, "delete_data=true") {
		t.Errorf("query %q does not carry delete_data=true", gotQuery)
	}
	s := out.String()
	for _, want := range []string{"DESTROYED", "1 attached app(s) (web)", "lost their database", "kept the backup volume"} {
		if !strings.Contains(s, want) {
			t.Errorf("removal output missing %q:\n%s", want, s)
		}
	}
}

// addonRemoveServer is a stub control plane for the `addon remove --delete-data` gate tests. It
// answers the three read paths the pre-removal notice uses best-effort — the add-on listing, the app
// listing, and each app's Secret KEY names (never values) — and records every DELETE it receives, so a
// test can assert that a refused removal never reached the API at all. attachedApps names the apps
// whose Secret carries a DATABASE_URL; every other app answers with an unrelated key.
func addonRemoveServer(t *testing.T, apps, attachedApps []string, deletes *[]string) *httptest.Server {
	t.Helper()
	return addonRemoveServerWithProviders(t, apps, attachedApps, nil, deletes)
}

// addonRemoveServerWithProviders is addonRemoveServer with the provider listing the notice reads to
// say whether a final backup will be taken (ADR-0064 §5). An empty listing is the "nothing durable
// is registered" case, which is what the plain helper serves.
func addonRemoveServerWithProviders(t *testing.T, apps, attachedApps []string, providers []map[string]any, deletes *[]string) *httptest.Server {
	t.Helper()
	attached := map[string]bool{}
	for _, a := range attachedApps {
		attached[a] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			*deletes = append(*deletes, r.URL.Path+"?"+r.URL.RawQuery)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "burrow-postgres", "type": "postgres", "namespace": "burrow-addons",
				"data_deleted": true, "retained_backup_volume": "burrow-postgres-backups",
				"attached_apps": attachedApps,
			})
		case r.URL.Path == "/v1/addons":
			_ = json.NewEncoder(w).Encode(map[string]any{"addons": []map[string]any{
				{"name": "burrow-postgres", "type": "postgres", "mode": "installed", "endpoint": "burrow-postgres:5432", "capabilities": []string{"database"}, "ready": true},
			}})
		case r.URL.Path == "/v1/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": providers})
		case r.URL.Path == "/v1/apps":
			rows := make([]map[string]any, 0, len(apps))
			for _, a := range apps {
				rows = append(rows, map[string]any{"app": a})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apps": rows})
		case strings.HasSuffix(r.URL.Path, "/secrets"):
			app := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/apps/"), "/secrets")
			keys := []string{"API_KEY"}
			if attached[app] {
				keys = []string{"DATABASE_URL"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// execAddonRemove drives `addon remove` with an explicit stdin and interactive-terminal flag,
// returning stdout, stderr, and the RunE error. The terminal flag drives the stdinIsTerminal seam so
// both branches of ADR-0064 §2's gate are exercised without a real TTY.
func execAddonRemove(t *testing.T, baseURL, stdin string, terminal bool, args ...string) (string, string, error) {
	t.Helper()
	isolateConfig(t)
	origTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return terminal }
	t.Cleanup(func() { stdinIsTerminal = origTerm })

	var out, errb bytes.Buffer
	cmd := newAddonRemoveCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append(args, "--control-plane", baseURL, "--token", "tok"))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// TestAddonRemoveDeleteDataTypedNameProceeds asserts the interactive half of ADR-0064 §2: on a
// terminal, --delete-data prints a warning-styled notice naming the data volume, its namespace, the
// attached apps whose databases are in it, and the backup volume that survives — and proceeds once the
// add-on's name is typed back.
func TestAddonRemoveDeleteDataTypedNameProceeds(t *testing.T) {
	var deletes []string
	srv := addonRemoveServer(t, []string{"api", "static", "web"}, []string{"api", "web"}, &deletes)

	out, errb, err := execAddonRemove(t, srv.URL, "burrow-postgres\n", true, "burrow-postgres", "--delete-data", "--confirm")
	if err != nil {
		t.Fatalf("addon remove --delete-data with the name typed back: %v (stderr: %s)", err, errb)
	}
	for _, want := range []string{
		"DESTROYS the data volume \"burrow-postgres\"",
		"burrow-addons",
		"This cannot be undone",
		"every attached app's database: 2 attached apps (api, web)",
		"backup volume \"burrow-postgres-backups\" is kept",
		"Type the add-on's name (burrow-postgres) to proceed",
	} {
		if !strings.Contains(errb, want) {
			t.Errorf("the --delete-data notice is missing %q:\n%s", want, errb)
		}
	}
	// The notice and the prompt go to stderr, so a --json run keeps a machine-readable stdout.
	if strings.Contains(out, "Type the add-on's name") {
		t.Errorf("the typed-name prompt must not land on stdout:\n%s", out)
	}
	if len(deletes) != 1 || !strings.Contains(deletes[0], "delete_data=true") {
		t.Fatalf("expected one removal carrying delete_data=true, got %v", deletes)
	}
}

// TestAddonRemoveDeleteDataWrongNameRefuses asserts a typed name that is not the add-on's aborts: the
// command errors, says nothing was removed, and no removal reaches the API.
func TestAddonRemoveDeleteDataWrongNameRefuses(t *testing.T) {
	var deletes []string
	srv := addonRemoveServer(t, []string{"web"}, []string{"web"}, &deletes)

	for _, typed := range []string{"postgres\n", "\n", ""} { // a near-miss, an empty line, and EOF
		_, _, err := execAddonRemove(t, srv.URL, typed, true, "burrow-postgres", "--delete-data", "--confirm")
		if err == nil {
			t.Fatalf("typing %q should abort the removal", typed)
		}
		if !strings.Contains(err.Error(), "nothing was removed") {
			t.Errorf("the abort should say nothing was removed, got: %v", err)
		}
	}
	if len(deletes) != 0 {
		t.Errorf("an aborted --delete-data must not reach the API, got %v", deletes)
	}
}

// TestAddonRemoveDeleteDataNonInteractiveRefuses asserts the other half of ADR-0064 §2: with no
// terminal to type into, --delete-data refuses rather than proceeding, names the acknowledgement flag,
// and — the property that matters — DELETES NOTHING. --confirm satisfies the addon.remove guardrail
// and is deliberately not the acknowledgement, so it does not open this path on its own.
func TestAddonRemoveDeleteDataNonInteractiveRefuses(t *testing.T) {
	var deletes []string
	srv := addonRemoveServer(t, []string{"web"}, []string{"web"}, &deletes)

	_, _, err := execAddonRemove(t, srv.URL, "", false, "burrow-postgres", "--delete-data", "--confirm")
	if err == nil {
		t.Fatal("--delete-data without a terminal and without the acknowledgement flag must refuse")
	}
	for _, want := range []string{"--acknowledge-data-loss", "interactive terminal", "burrow-postgres"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, err)
		}
	}
	if len(deletes) != 0 {
		t.Fatalf("a refused --delete-data must not delete: the API saw %v", deletes)
	}
}

// TestAddonRemoveDeleteDataAcknowledgedProceedsNonInteractively asserts the escape hatch: a script
// that says out loud that it destroys data proceeds with no terminal and no prompt (ADR-0064 §2).
func TestAddonRemoveDeleteDataAcknowledgedProceedsNonInteractively(t *testing.T) {
	var deletes []string
	srv := addonRemoveServer(t, []string{"web"}, []string{"web"}, &deletes)

	out, errb, err := execAddonRemove(t, srv.URL, "", false, "burrow-postgres", "--delete-data", "--acknowledge-data-loss", "--confirm")
	if err != nil {
		t.Fatalf("--delete-data --acknowledge-data-loss without a terminal: %v (stderr: %s)", err, errb)
	}
	if strings.Contains(errb+out, "Type the add-on's name") {
		t.Errorf("the acknowledgement flag should skip the prompt:\n%s%s", errb, out)
	}
	if len(deletes) != 1 || !strings.Contains(deletes[0], "delete_data=true") {
		t.Fatalf("expected one removal carrying delete_data=true, got %v", deletes)
	}
}

// TestAddonRemoveDeleteDataNoticeDegradesWhenUnreachable asserts the notice's enumeration is
// best-effort and never blocking (ADR-0064 §3): a control plane that will not answer the add-on and
// app lookups degrades the notice to the volume-concrete message, and the removal still goes through.
// An add-on is often removed because it is wedged, so being unable to ask who is attached must not
// make it unremovable.
func TestAddonRemoveDeleteDataNoticeDegradesWhenUnreachable(t *testing.T) {
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes = append(deletes, r.URL.Path+"?"+r.URL.RawQuery)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "burrow-postgres", "type": "postgres", "data_deleted": true})
			return
		}
		http.Error(w, "the add-on is wedged", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, errb, err := execAddonRemove(t, srv.URL, "burrow-postgres\n", true, "burrow-postgres", "--delete-data", "--confirm")
	if err != nil {
		t.Fatalf("a removal whose enumeration failed should still proceed: %v (stderr: %s)", err, errb)
	}
	if !strings.Contains(errb, "DESTROYS the data volume \"burrow-postgres\"") {
		t.Errorf("the degraded notice should still name the volume:\n%s", errb)
	}
	if strings.Contains(errb, "attached app") {
		t.Errorf("the degraded notice must not claim an app enumeration it could not make:\n%s", errb)
	}
	if len(deletes) != 1 {
		t.Fatalf("expected the removal to proceed, got %v", deletes)
	}
}

// TestAddonRemoveDefaultNeedsNoTerminal asserts the gate is --delete-data's alone: a plain removal
// keeps the data volume, so it neither prompts nor refuses without a terminal.
func TestAddonRemoveDefaultNeedsNoTerminal(t *testing.T) {
	var deletes []string
	srv := addonRemoveServer(t, []string{"web"}, []string{"web"}, &deletes)

	out, errb, err := execAddonRemove(t, srv.URL, "", false, "burrow-postgres", "--confirm")
	if err != nil {
		t.Fatalf("a data-keeping removal should need no terminal: %v (stderr: %s)", err, errb)
	}
	if strings.Contains(errb+out, "Type the add-on's name") {
		t.Errorf("a removal without --delete-data must not prompt:\n%s%s", errb, out)
	}
	if len(deletes) != 1 || strings.Contains(deletes[0], "delete_data") {
		t.Fatalf("expected one removal that does not ask to delete data, got %v", deletes)
	}
}

// addonListingWith returns a stub control plane serving an add-on listing: one installed add-on plus
// whatever retained volumes the test wants reported.
func addonListingWith(t *testing.T, retained []map[string]any) *httptest.Server {
	t.Helper()
	body := map[string]any{
		"addons": []map[string]any{
			{"name": "burrow-logs", "type": "logs", "mode": "installed", "endpoint": "logs.svc:9428", "capabilities": []string{"logs"}, "ready": true},
		},
	}
	if retained != nil {
		body["retained_volumes"] = retained
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
}

// TestAddonListReportsRetainedVolumes asserts `addon list` surfaces the volumes an earlier removal
// kept (ADR-0064 §6) — in a section of their own, naming the claim, the add-on it belonged to, what
// it holds, and its size, and saying both ways out: reinstall to get the data back, or delete the
// claim to get the storage back. Without this the claim is named once, by the removal that created
// it, and never again.
func TestAddonListReportsRetainedVolumes(t *testing.T) {
	isolateConfig(t)
	srv := addonListingWith(t, []map[string]any{
		{"name": "burrow-postgres", "namespace": "burrow-addons", "addon": "postgres", "role": "data", "size": "10Gi", "reinstall_adopts": true},
		{"name": "burrow-postgres-backups", "namespace": "burrow-addons", "addon": "postgres", "role": "backup", "size": "10Gi", "reinstall_adopts": false},
	})
	defer srv.Close()

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"addon", "list", "--control-plane", srv.URL, "--token", "tok"}, &out, &errb); err != nil {
		t.Fatalf("addon list: %v (stderr: %s)", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		// The installed add-on is still listed as before.
		"burrow-logs", "logs.svc:9428",
		// The retained volumes are a separate, labelled section — not extra rows in the add-on table.
		"Retained volumes (2)", "still allocated and still billed",
		"CLAIM", "ADD-ON", "HOLDS", "SIZE",
		"burrow-postgres", "burrow-postgres-backups", "postgres", "data", "backup", "10Gi", "burrow-addons",
		// Both ways out, per ADR-0064 §1 ("how to reclaim it later") and §6.
		"burrow addon install", "kubectl -n <namespace> delete pvc <claim>",
		"Nothing deletes these for you.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("listing missing %q:\n%s", want, s)
		}
	}
	// A retained claim must not be readable as a running add-on: it appears below the add-on table,
	// never inside it.
	hdr := strings.Index(s, "CAPABILITIES")
	idx := strings.Index(s, "Retained volumes")
	if hdr == -1 || idx == -1 || idx < hdr {
		t.Errorf("the retained section must come after the add-on table, not inside or before it:\n%s", s)
	}
}

// TestAddonListJSONSeparatesRetainedVolumes asserts the --json shape keeps the two kinds apart: the
// add-ons under "addons", the leftovers under "retained_volumes", each carrying the add-on it
// belonged to and its size.
func TestAddonListJSONSeparatesRetainedVolumes(t *testing.T) {
	isolateConfig(t)
	srv := addonListingWith(t, []map[string]any{
		{"name": "burrow-postgres", "namespace": "burrow-addons", "addon": "postgres", "role": "data", "size": "10Gi", "reinstall_adopts": true},
	})
	defer srv.Close()

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"addon", "list", "--json", "--control-plane", srv.URL, "--token", "tok"}, &out, &errb); err != nil {
		t.Fatalf("addon list --json: %v (stderr: %s)", err, errb.String())
	}
	var got struct {
		Addons []struct {
			Name string `json:"name"`
		} `json:"addons"`
		RetainedVolumes []struct {
			Name            string `json:"name"`
			Namespace       string `json:"namespace"`
			Addon           string `json:"addon"`
			Role            string `json:"role"`
			Size            string `json:"size"`
			ReinstallAdopts bool   `json:"reinstall_adopts"`
		} `json:"retained_volumes"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decoding --json output: %v (%s)", err, out.String())
	}
	if len(got.Addons) != 1 || got.Addons[0].Name != "burrow-logs" {
		t.Errorf("addons = %+v, want the installed add-on", got.Addons)
	}
	if len(got.RetainedVolumes) != 1 {
		t.Fatalf("retained_volumes = %+v, want one", got.RetainedVolumes)
	}
	v := got.RetainedVolumes[0]
	if v.Name != "burrow-postgres" || v.Addon != "postgres" || v.Role != "data" || v.Size != "10Gi" || v.Namespace != "burrow-addons" || !v.ReinstallAdopts {
		t.Errorf("retained volume = %+v, want the postgres data claim with its size and namespace", v)
	}
}

// TestAddonListCleanWithoutRetainedVolumes asserts a cluster that has removed nothing lists exactly
// as it did before: no empty section, no heading with nothing under it, nothing to explain.
func TestAddonListCleanWithoutRetainedVolumes(t *testing.T) {
	isolateConfig(t)
	srv := addonListingWith(t, nil)
	defer srv.Close()

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"addon", "list", "--control-plane", srv.URL, "--token", "tok"}, &out, &errb); err != nil {
		t.Fatalf("addon list: %v (stderr: %s)", err, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "burrow-logs") {
		t.Errorf("listing does not show the installed add-on:\n%s", s)
	}
	for _, unwanted := range []string{"Retained", "kubectl", "CLAIM"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("listing mentions %q with no retained volumes:\n%s", unwanted, s)
		}
	}
}

// TestAddonRemoveNoticeNamesTheFinalBackup asserts the typed-name notice says a copy is made before
// the name is typed, not only afterwards in the output (ADR-0064 §5). "A copy is written off-cluster
// first and this is abandoned if that fails" and "this is the last moment the data exists" are
// different things to be agreeing to.
func TestAddonRemoveNoticeNamesTheFinalBackup(t *testing.T) {
	var deletes []string
	srv := addonRemoveServerWithProviders(t, []string{"web"}, []string{"web"}, []map[string]any{
		{"name": "backups", "type": "s3", "capabilities": []string{"object-storage"}},
	}, &deletes)

	_, errb, err := execAddonRemove(t, srv.URL, "burrow-postgres\n", true, "burrow-postgres", "--delete-data", "--confirm")
	if err != nil {
		t.Fatalf("addon remove --delete-data: %v (stderr: %s)", err, errb)
	}
	for _, want := range []string{"final backup", "backups object store", "nothing is removed"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the --delete-data notice is missing %q:\n%s", want, errb)
		}
	}
}

// TestAddonRemoveNoticeSaysWhenNoBackupIsTaken asserts the other half: with nothing durable
// registered, and with --skip-final-backup, the operator is told plainly that no copy will exist —
// which is the version of this decision that cannot be taken back.
func TestAddonRemoveNoticeSaysWhenNoBackupIsTaken(t *testing.T) {
	var deletes []string
	srv := addonRemoveServer(t, []string{"web"}, []string{"web"}, &deletes)

	_, errb, err := execAddonRemove(t, srv.URL, "burrow-postgres\n", true, "burrow-postgres", "--delete-data", "--confirm")
	if err != nil {
		t.Fatalf("addon remove --delete-data: %v (stderr: %s)", err, errb)
	}
	if !strings.Contains(errb, "No object-storage provider is registered") {
		t.Errorf("the notice does not say that no off-cluster copy will be taken:\n%s", errb)
	}

	_, errb, err = execAddonRemove(t, srv.URL, "burrow-postgres\n", true, "burrow-postgres", "--delete-data", "--skip-final-backup", "--confirm")
	if err != nil {
		t.Fatalf("addon remove --delete-data --skip-final-backup: %v (stderr: %s)", err, errb)
	}
	if !strings.Contains(errb, "NO final backup will be taken") {
		t.Errorf("the notice does not say the override skips the backup:\n%s", errb)
	}
	if len(deletes) != 2 || !strings.Contains(deletes[1], "skip_final_backup=true") {
		t.Fatalf("the override did not reach the API: %v", deletes)
	}
}

// TestAddonRemoveSkipFinalBackupNeedsDeleteData asserts a lone --skip-final-backup is refused rather
// than accepted quietly. Its likely reading is "and destroy the data", which is not what it says,
// and a destructive command is the wrong place to let a misread flag pass without comment.
func TestAddonRemoveSkipFinalBackupNeedsDeleteData(t *testing.T) {
	var deletes []string
	srv := addonRemoveServer(t, []string{"web"}, []string{"web"}, &deletes)

	_, _, err := execAddonRemove(t, srv.URL, "", true, "burrow-postgres", "--skip-final-backup", "--confirm")
	if err == nil {
		t.Fatal("--skip-final-backup without --delete-data should be refused")
	}
	if !strings.Contains(err.Error(), "--delete-data") {
		t.Errorf("the refusal should say which flag it applies to, got: %v", err)
	}
	if len(deletes) != 0 {
		t.Fatalf("a refused invocation must not reach the API, got %v", deletes)
	}
}

// TestAddonRemoveSummaryReportsWhatHappenedToTheCopy asserts the outcome says what happened to the
// copy immediately after what happened to the original — a taken backup by id and destination, and a
// skipped one as plainly. An operator who destroyed a database believing a copy exists is worse off
// than one who knows none does.
func TestAddonRemoveSummaryReportsWhatHappenedToTheCopy(t *testing.T) {
	taken := client.RemoveAddonResult{
		Name: "burrow-postgres", Type: "postgres", DataDeleted: true,
		FinalBackups: []client.Backup{{
			ID: "bk1", App: "web", Status: "completed",
			Destination: "object-store", Provider: "backups", ObjectKey: "web/bk1.dump",
		}},
	}
	s := removeAddonSummary(taken)
	for _, want := range []string{"final backup of \"web\" taken first", "bk1", "backups object store"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}

	skipped := client.RemoveAddonResult{
		Name: "burrow-postgres", Type: "postgres", DataDeleted: true,
		FinalBackupSkipped: true, FinalBackupNote: "--skip-final-backup was passed",
	}
	s = removeAddonSummary(skipped)
	if !strings.Contains(s, "NO final backup was taken — --skip-final-backup was passed") {
		t.Errorf("summary does not state plainly that no copy exists:\n%s", s)
	}
}
