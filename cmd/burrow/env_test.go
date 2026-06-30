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

	"github.com/burrow-cloud/burrow/client"
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
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"environments": []map[string]any{
					{"name": "default", "namespace": "burrow-apps", "default": true},
					{"name": "staging", "namespace": "burrow-apps-staging"},
				},
			})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
}

func TestEnvAddAppliesAndRegisters(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

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
	err := run(context.Background(), []string{"env", "add", "staging", "--control-plane", srv.URL, "--token", "t"}, &out, &errb)
	if err != nil {
		t.Fatalf("env add: %v\n%s", err, errb.String())
	}

	// The default namespace is <app-namespace>-<name>.
	if addedName != "staging" || addedNs != "burrow-apps-staging" {
		t.Errorf("registered (%q,%q), want (staging, burrow-apps-staging)", addedName, addedNs)
	}
	if !strings.Contains(applied, "name: burrow-apps-staging") || !strings.Contains(applied, "kind: RoleBinding") {
		t.Errorf("applied manifests missing the env namespace/RBAC:\n%s", applied)
	}
	if !strings.Contains(out.String(), `Environment "staging" created`) {
		t.Errorf("confirmation output = %q", out.String())
	}
}

func TestEnvAddNamespaceOverride(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

	var addedNs string
	srv := fakeEnvAPI(t, func(_, ns string) { addedNs = ns })
	defer srv.Close()

	orig := applyFn
	applyFn = func(_ context.Context, _ string, _ string, _ string, _ bool, _, _ io.Writer) error { return nil }
	defer func() { applyFn = orig }()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"env", "add", "prod", "--namespace", "team-prod", "--control-plane", srv.URL, "--token", "t"}, &out, &errb)
	if err != nil {
		t.Fatalf("env add: %v\n%s", err, errb.String())
	}
	if addedNs != "team-prod" {
		t.Errorf("--namespace override: registered namespace = %q, want team-prod", addedNs)
	}
}

func TestWriteEnvList(t *testing.T) {
	var b bytes.Buffer
	writeEnvList(&b, []client.Environment{
		{Name: "default", Namespace: "burrow-apps", Default: true},
		{Name: "staging", Namespace: "burrow-apps-staging"},
	})
	out := b.String()
	for _, want := range []string{"DEFAULT", "NAME", "NAMESPACE", "default", "staging", "burrow-apps-staging"} {
		if !strings.Contains(out, want) {
			t.Errorf("env list output missing %q\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		// The default row carries the * marker; the staging row does not.
		if strings.Contains(line, "default") && strings.Contains(line, "burrow-apps") && !strings.Contains(line, "*") {
			t.Errorf("default environment row not marked with *: %q", line)
		}
		if strings.Contains(line, "staging") && strings.Contains(line, "*") {
			t.Errorf("non-default row wrongly marked: %q", line)
		}
	}
}

func TestEnvListCommand(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

	srv := fakeEnvAPI(t, nil)
	defer srv.Close()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"env", "list", "--control-plane", srv.URL, "--token", "t"}, &out, &errb)
	if err != nil {
		t.Fatalf("env list: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{"default", "staging", "burrow-apps-staging"} {
		if !strings.Contains(s, want) {
			t.Errorf("env list output missing %q\n%s", want, s)
		}
	}
}
