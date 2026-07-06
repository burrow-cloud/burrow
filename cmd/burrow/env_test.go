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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/burrow-cloud/burrow/localconfig"
)

func TestRenderEnvManifests(t *testing.T) {
	out, err := renderEnvManifests(envOptions{Namespace: "burrow", AppNamespace: "burrow-apps-staging"})
	if err != nil {
		t.Fatalf("renderEnvManifests: %v", err)
	}
	for _, want := range []string{
		"kind: Namespace",
		"name: burrow-apps-staging", // the environment's namespace is created
		"kind: Role",
		"kind: RoleBinding",
		"namespace: burrow-apps-staging", // the Role lives in the environment's namespace
		// The Role's rules MUST match the install app-namespace Role (shared template, no drift).
		`verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]`,
		`resources: ["services"]`,
		`resources: ["ingresses"]`,
		`resources: ["secrets"]`,
		`verbs: ["get", "list", "create", "update"]`, // the app env-secrets grant
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered env manifests missing %q\n%s", want, out)
		}
	}
	// The RoleBinding subject ServiceAccount lives in the control-plane namespace, not the env one.
	if !strings.Contains(out, "namespace: burrow\n") {
		t.Errorf("RoleBinding subject should reference the control-plane namespace (burrow)\n%s", out)
	}
}

// TestEnvManifestRoleMatchesInstall asserts the per-environment Role rules are byte-identical to the
// install app-namespace Role for the same namespace — the anti-drift guarantee of the shared
// template (ADR-0035).
func TestEnvManifestRoleMatchesInstall(t *testing.T) {
	ns := "burrow-apps-staging"
	envOut, err := renderEnvManifests(envOptions{Namespace: "burrow", AppNamespace: ns})
	if err != nil {
		t.Fatalf("renderEnvManifests: %v", err)
	}
	installOut, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: ns, Image: "img:1", Token: "t", DBPassword: "p", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	role := roleBlock(t, envOut)
	if !strings.Contains(installOut, role) {
		t.Errorf("env Role/RoleBinding block is not present verbatim in the install manifests (drift)\n--- env block ---\n%s", role)
	}
}

// roleBlock extracts the Role+RoleBinding block (from "kind: Role" to the end of the binding) from
// rendered manifests, for the cross-check above.
func roleBlock(t *testing.T, manifests string) string {
	t.Helper()
	start := strings.Index(manifests, "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\n")
	if start < 0 {
		t.Fatalf("no Role block found in:\n%s", manifests)
	}
	rest := manifests[start:]
	// The block runs through the RoleBinding's subject namespace line.
	end := strings.Index(rest, "namespace: burrow\n")
	if end < 0 {
		t.Fatalf("no RoleBinding subject namespace found in:\n%s", rest)
	}
	return rest[:end+len("namespace: burrow")]
}

// fakeEnvAPI is a fake burrowd serving the environments endpoints, recording an add.
func fakeEnvAPI(t *testing.T, onAdd func(name, namespace string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
			var body struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if onAdd != nil {
				onAdd(body.Name, body.Namespace)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": body.Name, "namespace": body.Namespace})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
}

// tempConfig points $BURROW_CONFIG at a fresh temp file so a test never touches ~/.burrow/config,
// and returns the path.
func tempConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("BURROW_CONFIG", path)
	return path
}

// kubeconfigWithCurrent writes a kubeconfig naming the given contexts, with `current` selected, and
// returns its path. It backs the follow-mode resolution `burrow env list` performs.
func kubeconfigWithCurrent(t *testing.T, current string, contexts ...string) string {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["c"] = &api.Cluster{Server: "https://x:6443", InsecureSkipTLSVerify: true}
	cfg.AuthInfos["u"] = &api.AuthInfo{Token: "t"}
	for _, c := range contexts {
		cfg.Contexts[c] = &api.Context{Cluster: "c", AuthInfo: "u"}
	}
	cfg.CurrentContext = current
	return writeKubeconfig(t, cfg)
}

// twoHandleConfig saves a config with dev and nonprod handles (optionally pinning one) to the path
// $BURROW_CONFIG points at.
func twoHandleConfig(t *testing.T, current string) {
	t.Helper()
	cfg := &localconfig.Config{
		Current: current,
		Environments: []localconfig.Environment{
			{Name: "dev", Context: "ctx-dev", AppNamespace: "burrow-apps"},
			{Name: "nonprod", Context: "ctx-nonprod", AppNamespace: "team-x"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// TestEnvListFollowing lists handles and marks the one matching the current kube context as the
// current, following-kubectl row.
func TestEnvListFollowing(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "")
	kc := kubeconfigWithCurrent(t, "ctx-nonprod", "ctx-dev", "ctx-nonprod")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "list", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env list: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{"CURRENT", "NAME", "CONTEXT", "NAMESPACE", "dev", "nonprod", "ctx-nonprod", "team-x"} {
		if !strings.Contains(s, want) {
			t.Errorf("env list output missing %q\n%s", want, s)
		}
	}
	// The active env's data row carries a "*" in the leading CURRENT column; the other does not.
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.Contains(line, "ctx-nonprod") && !strings.HasPrefix(line, "*"):
			t.Errorf("nonprod row not marked current with a leading *: %q", line)
		case strings.Contains(line, "ctx-dev") && strings.HasPrefix(line, "*"):
			t.Errorf("dev row wrongly marked current: %q", line)
		}
	}
	if !strings.Contains(s, "* current environment, following your kube context. Pin one with `burrow env use <name>`.") {
		t.Errorf("missing the following-mode legend in plain language\n%s", s)
	}
	if strings.Contains(s, "(pinned)") || strings.Contains(s, "<---") {
		t.Errorf("output should not carry the old inline markers\n%s", s)
	}
}

// TestEnvListPinned marks the pinned handle as current (pinned), regardless of the kube context.
func TestEnvListPinned(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "dev")
	kc := kubeconfigWithCurrent(t, "ctx-nonprod", "ctx-dev", "ctx-nonprod")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "list", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env list: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.Contains(line, "ctx-dev") && !strings.HasPrefix(line, "*"):
			t.Errorf("dev row not marked current with a leading *: %q", line)
		case strings.Contains(line, "ctx-nonprod") && strings.HasPrefix(line, "*"):
			t.Errorf("nonprod row wrongly marked current: %q", line)
		}
	}
	// The legend names the pinned mode in plain language and points at how to return to following.
	if !strings.Contains(s, "* current environment, pinned. Return to following your kube context with `burrow env follow`.") {
		t.Errorf("missing the pinned-mode legend in plain language\n%s", s)
	}
	if strings.Contains(s, "(pinned)") || strings.Contains(s, "<---") {
		t.Errorf("output should not carry the old inline markers\n%s", s)
	}
}

// TestEnvListUnregistered prints the trailing follow line when the current context matches no handle.
func TestEnvListUnregistered(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "")
	kc := kubeconfigWithCurrent(t, "ctx-orphan", "ctx-dev", "ctx-nonprod", "ctx-orphan")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "list", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env list: %v\n%s", err, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "following kubectl: ctx-orphan (unregistered)") {
		t.Errorf("missing unregistered follow line\n%s", s)
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "dev") || strings.HasPrefix(line, "nonprod") {
			if strings.Contains(line, "current") {
				t.Errorf("no handle row should be marked current when following an unregistered context: %q", line)
			}
		}
	}
}

// TestEnvListJSON honors --json, surfacing the handles and the resolved selection.
func TestEnvListJSON(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "dev")
	kc := kubeconfigWithCurrent(t, "ctx-nonprod", "ctx-dev", "ctx-nonprod")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "list", "--json", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env list --json: %v\n%s", err, errb.String())
	}
	var got envListResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if got.Current != "dev" || got.Mode != string(localconfig.ModePinned) {
		t.Errorf("resolved current/mode = %q/%q, want dev/pinned", got.Current, got.Mode)
	}
	if len(got.Environments) != 2 {
		t.Errorf("environments = %d, want 2", len(got.Environments))
	}
}

// TestEnvListBare confirms the bare `burrow env` lists handles, like `burrow env list`.
func TestEnvListBare(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "")
	kc := kubeconfigWithCurrent(t, "ctx-dev", "ctx-dev", "ctx-nonprod")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "nonprod") || !strings.Contains(out.String(), "dev") {
		t.Errorf("bare `env` did not list handles\n%s", out.String())
	}
}

// TestEnvUse pins a handle and rejects an unregistered name.
func TestEnvUse(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "use", "nonprod"}, &out, &errb); err != nil {
		t.Fatalf("env use: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Current != "nonprod" {
		t.Errorf("current = %q, want nonprod", cfg.Current)
	}

	out.Reset()
	errb.Reset()
	if err := run(context.Background(), []string{"env", "use", "ghost"}, &out, &errb); err == nil {
		t.Errorf("env use of an unregistered handle should error")
	}
}

// TestEnvFollow clears the pin.
func TestEnvFollow(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "dev")
	kc := kubeconfigWithCurrent(t, "ctx-nonprod", "ctx-dev", "ctx-nonprod")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "follow", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env follow: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Current != "" {
		t.Errorf("current = %q, want empty after follow", cfg.Current)
	}
}

// TestEnvRename renames a handle and carries the pin when the renamed handle was current.
func TestEnvRename(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "dev")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "rename", "dev", "dev-new"}, &out, &errb); err != nil {
		t.Fatalf("env rename: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Lookup("dev"); ok {
		t.Errorf("old handle dev should be gone")
	}
	if _, ok := cfg.Lookup("dev-new"); !ok {
		t.Errorf("renamed handle dev-new should exist")
	}
	if cfg.Current != "dev-new" {
		t.Errorf("current = %q, want the pin to follow the rename to dev-new", cfg.Current)
	}
}

// TestEnvRemove drops a handle from the saved config and prints what it removed.
func TestEnvRemove(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "remove", "nonprod"}, &out, &errb); err != nil {
		t.Fatalf("env remove: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Lookup("nonprod"); ok {
		t.Errorf("removed handle nonprod should be gone from the config")
	}
	if _, ok := cfg.Lookup("dev"); !ok {
		t.Errorf("unrelated handle dev should remain")
	}
	if !strings.Contains(out.String(), `Removed environment "nonprod".`) {
		t.Errorf("missing removal confirmation\n%s", out.String())
	}
}

// TestEnvRemoveAliasRm confirms `rm` works as an alias for `remove`.
func TestEnvRemoveAliasRm(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "rm", "dev"}, &out, &errb); err != nil {
		t.Fatalf("env rm: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Lookup("dev"); ok {
		t.Errorf("`env rm dev` should have removed the dev handle")
	}
}

// TestEnvRemoveClearsPin confirms removing the pinned handle clears Current (back to follow) and
// tells the user so.
func TestEnvRemoveClearsPin(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "dev")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "remove", "dev"}, &out, &errb); err != nil {
		t.Fatalf("env remove: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Current != "" {
		t.Errorf("removing the pinned handle should clear the pin, current = %q", cfg.Current)
	}
	if !strings.Contains(out.String(), "now following your current kube context") {
		t.Errorf("removal of the pinned handle should report the pin was cleared\n%s", out.String())
	}
}

// TestEnvRemoveUnknown errors on a name that is not registered, pointing at `burrow env list`.
func TestEnvRemoveUnknown(t *testing.T) {
	tempConfig(t)
	twoHandleConfig(t, "")

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"env", "remove", "ghost"}, &out, &errb)
	if err == nil {
		t.Fatalf("env remove of an unregistered handle should error")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "burrow env list") {
		t.Errorf("error should name the handle and point at `burrow env list`, got: %v", err)
	}
}

// TestEnvRemoveDeletesManagedAgentKubeconfig confirms a scoped agent kubeconfig living under
// Burrow's managed ~/.burrow/agents/ dir is deleted when its handle is removed, while a kubeconfig
// pointed outside that dir is left untouched.
func TestEnvRemoveDeletesManagedAgentKubeconfig(t *testing.T) {
	cfgPath := tempConfig(t)
	agentsDir := filepath.Join(filepath.Dir(cfgPath), "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(agentsDir, "scoped")
	if err := os.WriteFile(managed, []byte("kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second kubeconfig outside the managed dir must never be touched.
	outside := filepath.Join(t.TempDir(), "user-kubeconfig")
	if err := os.WriteFile(outside, []byte("kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &localconfig.Config{
		Environments: []localconfig.Environment{
			{Name: "scoped", Context: "ctx-scoped", AgentKubeconfig: managed},
			{Name: "byo", Context: "ctx-byo", AgentKubeconfig: outside},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "remove", "scoped"}, &out, &errb); err != nil {
		t.Fatalf("env remove scoped: %v\n%s", err, errb.String())
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Errorf("managed agent kubeconfig should have been deleted, stat err = %v", err)
	}
	if !strings.Contains(out.String(), "Deleted its scoped agent kubeconfig") {
		t.Errorf("removal should report the deleted scoped kubeconfig\n%s", out.String())
	}

	out.Reset()
	errb.Reset()
	if err := run(context.Background(), []string{"env", "remove", "byo"}, &out, &errb); err != nil {
		t.Fatalf("env remove byo: %v\n%s", err, errb.String())
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a kubeconfig outside ~/.burrow/agents/ must not be deleted, stat err = %v", err)
	}
	if strings.Contains(out.String(), "Deleted its scoped agent kubeconfig") {
		t.Errorf("removal must not report deleting a kubeconfig outside the managed dir\n%s", out.String())
	}
}

// TestEnvAddAppliesRegistersAndRecordsHandle confirms `env add` does the ADR-0035 server-side setup
// (apply namespace+RBAC, register with burrowd) AND records the ADR-0036 local handle.
func TestEnvAddAppliesRegistersAndRecordsHandle(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var addedName, addedNs string
	srv := fakeEnvAPI(t, func(name, ns string) { addedName, addedNs = name, ns })
	defer srv.Close()

	// Stub the privileged kubeconfig-side apply so the test needs no cluster, and capture what it
	// applied.
	var applied string
	orig := applyFn
	applyFn = func(_ context.Context, _ string, _ string, manifests string, _ bool, _, _ io.Writer) error {
		applied = manifests
		return nil
	}
	defer func() { applyFn = orig }()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"env", "add", "staging", "--context", "staging-ctx", "--control-plane", srv.URL, "--token", "t"}, &out, &errb)
	if err != nil {
		t.Fatalf("env add: %v\n%s", err, errb.String())
	}

	// (a) burrowd registration: default namespace is <app-namespace>-<name>.
	if addedName != "staging" || addedNs != "burrow-apps-staging" {
		t.Errorf("registered (%q,%q), want (staging, burrow-apps-staging)", addedName, addedNs)
	}
	if !strings.Contains(applied, "name: burrow-apps-staging") || !strings.Contains(applied, "kind: RoleBinding") {
		t.Errorf("applied manifests missing the env namespace/RBAC:\n%s", applied)
	}
	if !strings.Contains(out.String(), `Environment "staging" created`) {
		t.Errorf("confirmation output = %q", out.String())
	}

	// (b) local handle recorded: name -> targeted context, control-plane + app namespaces.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h, ok := cfg.Lookup("staging")
	if !ok {
		t.Fatalf("env add did not record a local handle for staging\n%s", out.String())
	}
	if h.Context != "staging-ctx" {
		t.Errorf("handle context = %q, want the targeted context staging-ctx", h.Context)
	}
	if h.AppNamespace != "burrow-apps-staging" {
		t.Errorf("handle app namespace = %q, want burrow-apps-staging", h.AppNamespace)
	}
	if h.ControlPlaneNamespace != "burrow" {
		t.Errorf("handle control-plane namespace = %q, want burrow", h.ControlPlaneNamespace)
	}
}

// TestEnvAddNamespaceOverride confirms --namespace overrides the derived env namespace, in both the
// burrowd registration and the local handle.
func TestEnvAddNamespaceOverride(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var addedNs string
	srv := fakeEnvAPI(t, func(_, ns string) { addedNs = ns })
	defer srv.Close()

	orig := applyFn
	applyFn = func(_ context.Context, _ string, _ string, _ string, _ bool, _, _ io.Writer) error { return nil }
	defer func() { applyFn = orig }()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"env", "add", "prod", "--namespace", "team-prod", "--context", "prod-ctx", "--control-plane", srv.URL, "--token", "t"}, &out, &errb)
	if err != nil {
		t.Fatalf("env add: %v\n%s", err, errb.String())
	}
	if addedNs != "team-prod" {
		t.Errorf("--namespace override: registered namespace = %q, want team-prod", addedNs)
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h, ok := cfg.Lookup("prod")
	if !ok || h.AppNamespace != "team-prod" {
		t.Errorf("local handle = %+v (found=%v), want app namespace team-prod", h, ok)
	}
}

// TestEnvHelpConciseNoADR confirms `burrow env -h` shows the concise description, a single Usage
// line, and neither an internal ADR reference nor the unrelated `burrow app config` pointer.
func TestEnvHelpConciseNoADR(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "-h"}, &out, &errb); err != nil {
		t.Fatalf("env -h: %v\n%s", err, errb.String())
	}
	s := out.String() + errb.String()
	if !strings.Contains(s, "Select and manage Burrow environments. An environment is a named handle") {
		t.Errorf("env help missing the concise description:\n%s", s)
	}
	for _, unwanted := range []string{"ADR", "burrow app config"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("env help should not mention %q:\n%s", unwanted, s)
		}
	}
	// A single Usage line at the bottom, not the old two-line `[flags]`/`[command]` pair.
	if !strings.Contains(s, "burrow env [command] [flags]") {
		t.Errorf("env help missing the single Usage line:\n%s", s)
	}
	if strings.Contains(s, "burrow env [flags]\n") {
		t.Errorf("env help still renders the two-line usage:\n%s", s)
	}
}

// TestWriteEnvList covers the kubectx-style rendering directly, including the active-row markers and
// the unregistered follow line.
func TestWriteEnvList(t *testing.T) {
	envs := []localconfig.Environment{
		{Name: "dev", Context: "ctx-dev", AppNamespace: "burrow-apps"},
		{Name: "nonprod", Context: "ctx-nonprod", AppNamespace: "team-x"},
	}

	var pinned bytes.Buffer
	writeEnvList(&pinned, envs, localconfig.Resolved{Name: "dev", Context: "ctx-dev", Mode: localconfig.ModePinned})
	ps := pinned.String()
	// A CURRENT column heads the table and the active env's row leads with "*"; the legend explains
	// the pinned mode in plain words, without the old inline "(pinned)" marker.
	if !strings.Contains(ps, "CURRENT") || !strings.Contains(ps, "NAME") {
		t.Errorf("populated list missing the CURRENT/NAME columns\n%s", ps)
	}
	if !strings.Contains(ps, "* current environment, pinned. Return to following your kube context with `burrow env follow`.") {
		t.Errorf("pinned legend missing\n%s", ps)
	}
	if strings.Contains(ps, "(pinned)") || strings.Contains(ps, "<---") {
		t.Errorf("old inline pinned marker should be gone\n%s", ps)
	}
	// The populated list closes with the help footer too.
	if !strings.Contains(ps, "Run `burrow env -h` for all environment commands.") {
		t.Errorf("populated list missing the env footer\n%s", ps)
	}

	var unreg bytes.Buffer
	writeEnvList(&unreg, envs, localconfig.Resolved{Context: "ctx-orphan", Mode: localconfig.ModeFollowing})
	if !strings.Contains(unreg.String(), "following kubectl: ctx-orphan (unregistered)") {
		t.Errorf("unregistered follow line missing\n%s", unreg.String())
	}

	// The zero-handle empty-state is a structured block: the followed context, the three ways to
	// register an environment, and the help footer.
	var empty bytes.Buffer
	writeEnvList(&empty, nil, localconfig.Resolved{Context: "ctx-dev", Mode: localconfig.ModeFollowing})
	es := empty.String()
	for _, want := range []string{
		"No environments registered yet:",
		"following kubectl context: ctx-dev", "(no handle registered)",
		"burrow env list --discover", "burrow install <context>", "burrow env add <name>",
		"Run `burrow env -h` for all environment commands.",
	} {
		if !strings.Contains(es, want) {
			t.Errorf("empty-state missing %q\n%s", want, es)
		}
	}
}
