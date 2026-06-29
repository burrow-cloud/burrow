// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRenderManifests(t *testing.T) {
	out, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: "apps", Image: "registry.example.com/burrowd:1",
		Token: "tok-123", DBPassword: "pw-456", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}

	for _, want := range []string{
		"kind: Namespace",
		"name: apps", // a custom app namespace is created
		"kind: ServiceAccount",
		"kind: Role",
		"kind: RoleBinding",
		"name: burrowd-api-token",
		`token: "tok-123"`,
		`postgres://burrow:pw-456@postgres:5432/burrow`,
		"image: postgres:18",
		"image: registry.example.com/burrowd:1",
		"{ name: BURROW_NAMESPACE, value: apps }", // burrowd deploys apps into the app namespace
		"-listen=:8080",
		"path: /healthz",
		`verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]`,
		`resources: ["services"]`,               // expose creates Services (ADR-0018)
		`resources: ["ingresses"]`,              // ... and Ingresses
		"name: burrow-credentials",              // the empty vendor-credential Secret (ADR-0023)
		`resourceNames: ["burrow-credentials"]`, // burrowd's credentials grant, scoped to it
		`verbs: ["get", "update"]`,              // get + update on exactly that Secret (ADR-0030)
		"fieldPath: metadata.namespace",         // POD_NAMESPACE: where burrowd reads credentials
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing %q", want)
		}
	}

	// Secrets grants are deliberately limited and documented. There are exactly three:
	//   1. the resourceNames-scoped `get`/`update` on burrow-credentials in the control-plane
	//      namespace (ADR-0023/0030) — burrowd's only access to vendor-token contents, now able to
	//      write a token value it received over its authenticated control-plane API;
	//   2. an app-namespace-scoped grant on app env Secrets (ADR-0028/0029) so burrowd can
	//      list/unset keys, write a secret value it received over the authenticated control-plane
	//      API, and let a provisioned backend write a connection string; and
	//   3. an add-on-namespace-scoped grant (ADR-0031) so burrowd can create/read/delete the
	//      Postgres add-on's burrow-postgres superuser Secret. Secret values still never cross MCP
	//      (no secret-set tool) and are never logged or stored in the DB (ADR-0029/0004).
	if c := strings.Count(out, `resources: ["secrets"]`); c != 3 {
		t.Errorf("expected exactly three secrets grants (scoped credentials + app-namespace env secrets + add-on-namespace postgres secret), found %d", c)
	}
	if !strings.Contains(out, `verbs: ["get", "list", "create", "update"]`) {
		t.Errorf("missing the app-namespace env-secrets grant (ADR-0028/0029)")
	}
	if !strings.Contains(out, `verbs: ["get", "create", "update", "delete"]`) {
		t.Errorf("missing the add-on-namespace postgres-secret grant (ADR-0031)")
	}
}

func TestRenderManifestsDefaultAppNamespace(t *testing.T) {
	out, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: "default", Image: "img:1",
		Token: "t", DBPassword: "p", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	// The default app namespace already exists, so we must not emit a Namespace for it
	// (which would relabel the cluster's default namespace).
	if strings.Contains(out, "name: default") {
		t.Errorf("must not create/relabel the default namespace")
	}
	if !strings.Contains(out, "{ name: BURROW_NAMESPACE, value: default }") {
		t.Errorf("burrowd should deploy apps into the default namespace")
	}
}

func TestRenderManifestsBurrowAppsNamespace(t *testing.T) {
	out, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: "burrow-apps", Image: "img:1",
		Token: "t", DBPassword: "p", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	// burrow-apps is neither the cluster's default namespace nor the control-plane namespace,
	// so install creates it (and labels it Burrow-managed).
	if !strings.Contains(out, "name: burrow-apps") {
		t.Errorf("must create the burrow-apps app namespace")
	}
	if !strings.Contains(out, "{ name: BURROW_NAMESPACE, value: burrow-apps }") {
		t.Errorf("burrowd should deploy apps into the burrow-apps namespace")
	}
}

func TestInstallDefaultAppNamespace(t *testing.T) {
	// Lock the install command's --app-namespace default to burrow-apps: defaulting to the
	// cluster's shared `default` namespace would put burrowd's Secrets grant (ADR-0029) there.
	cmd := newInstallCmd()
	f := cmd.Flags().Lookup("app-namespace")
	if f == nil {
		t.Fatal("install has no --app-namespace flag")
	}
	if f.DefValue != "burrow-apps" {
		t.Errorf("--app-namespace default = %q, want %q", f.DefValue, "burrow-apps")
	}
}

func TestSummarizeApply(t *testing.T) {
	out := `namespace/burrow created
serviceaccount/burrowd unchanged
role.rbac.authorization.k8s.io/burrowd-credentials created
rolebinding.rbac.authorization.k8s.io/burrowd-credentials created
secret/burrow-credentials configured
deployment.apps/burrowd configured`
	var b bytes.Buffer
	summarizeApply(out, &b)
	got := b.String()
	want := "Applied 6 resource(s): 3 created, 2 configured, 1 unchanged.\n"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	// No output (e.g. a no-op) summarizes to nothing.
	var empty bytes.Buffer
	summarizeApply("\n\n", &empty)
	if empty.Len() != 0 {
		t.Errorf("empty apply should print nothing, got %q", empty.String())
	}
}

func TestInstallDryRun(t *testing.T) {
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "--dry-run", "--namespace", "ns1", "--burrowd-image", "my/img:2"}, &out, &errb)
	if err != nil {
		t.Fatalf("install --dry-run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "namespace: ns1") || !strings.Contains(s, "image: my/img:2") {
		t.Errorf("dry-run output did not reflect flags:\n%s", s)
	}
	// dry-run must not require a cluster — it just prints.
	if strings.Contains(s, "installed into namespace") {
		t.Errorf("dry-run should not print the post-apply message")
	}
}

func TestBurrowdTagFor(t *testing.T) {
	cases := []struct {
		version string
		want    string // the resolved burrowd image tag, "" = no published image
	}{
		// Real release tags are used as-is — they are actual published images.
		{"v0.3.0", "v0.3.0"},
		{"v1.0.0", "v1.0.0"},
		{"v0.2.2-rc1", "v0.2.2-rc1"}, // a prerelease release tag is still a real tag
		// A Go pseudo-version (a local `go build` past a tag, what Go 1.24+ stamps) resolves to the
		// release it sits on top of — never to a pseudo tag, for which no image is published.
		{"v0.3.1-0.20260628005014-4b3d4cca70f3", "v0.3.0"},         // built one commit past v0.3.0
		{"v0.3.0-rc1.0.20260628005014-abcdef012345", "v0.3.0-rc1"}, // built past a prerelease tag
		// Build metadata (Go's "+dirty" for an uncommitted tree) is dropped — "+" is invalid in an
		// image tag, so the resolved tag must be clean.
		{"v0.3.1-0.20260628005838-64338d5fade9+dirty", "v0.3.0"},
		{"v0.3.0+dirty", "v0.3.0"},
		// No matching published image: no prior tag, "(devel)", or empty.
		{"v0.0.0-20260101000000-abcdefabcdef", ""},
		{"(devel)", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := burrowdTagFor(c.version); got != c.want {
			t.Errorf("burrowdTagFor(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}

func TestInstallRequiresImageWhenNoDefault(t *testing.T) {
	// With no --burrowd-image and an empty resolved default (an unreleased build with no published
	// image), install must refuse with a clear error rather than deploy an empty/guessed image.
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "--burrowd-image", "", "--dry-run"}, &out, &errb)
	if err == nil {
		t.Fatal("install with an empty burrowd image should error, got nil")
	}
	if !strings.Contains(err.Error(), "--burrowd-image") {
		t.Errorf("error should tell the user to pass --burrowd-image, got: %v", err)
	}
}
