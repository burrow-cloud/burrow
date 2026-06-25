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
		`resourceNames: ["burrow-credentials"]`, // burrowd's only secrets grant, scoped to it
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing %q", want)
		}
	}

	// Least privilege on Secrets (ADR-0023): burrowd's only access to a Secret's contents is
	// `get` on the single burrow-credentials object. There must be exactly one secrets grant,
	// and it must be the resourceNames-scoped one — no second, broader grant.
	if c := strings.Count(out, `resources: ["secrets"]`); c != 1 {
		t.Errorf("expected exactly one (scoped) secrets grant, found %d", c)
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
