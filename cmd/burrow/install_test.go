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
		Namespace: "burrow", Image: "registry.example.com/burrowd:1",
		Token: "tok-123", DBPassword: "pw-456", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}

	for _, want := range []string{
		"kind: Namespace",
		"kind: ServiceAccount",
		"kind: Role",
		"kind: RoleBinding",
		"name: burrowd-api-token",
		`token: "tok-123"`,
		`postgres://burrow:pw-456@postgres:5432/burrow`,
		"image: postgres:18",
		"image: registry.example.com/burrowd:1",
		"name: BURROW_NAMESPACE",
		"-listen=:8080",
		"path: /healthz",
		`verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing %q", want)
		}
	}

	// Least privilege: the ServiceAccount Role must not grant access to secrets.
	if strings.Contains(out, `["secrets"]`) {
		t.Errorf("RBAC should not grant secrets access")
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
