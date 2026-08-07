// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `burrow addon attach --as NAME` (issue #462): the operator half of naming the variable an
// attachment writes.

// TestAddonAttachAsSendsTheChosenVariable asserts the flag reaches the wire as the route segment and
// that the reported key is the one the control plane says it wrote — never a name the CLI assumed.
func TestAddonAttachAsSendsTheChosenVariable(t *testing.T) {
	isolateConfig(t)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "addon": "postgres", "environment": "prod", "secret_key": "DB_URL",
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "attach", "postgres", "web", "--as", "DB_URL",
		"--control-plane", srv.URL, "--token", "tok"}, &out, &errb)
	if err != nil {
		t.Fatalf("addon attach --as: %v (stderr: %s)", err, errb.String())
	}
	if gotPath != "/v1/addons/attach/env-key/DB_URL" {
		t.Errorf("path = %q, want the chosen variable in the route", gotPath)
	}
	if !strings.Contains(out.String(), `"DB_URL"`) {
		t.Errorf("output does not name the key that was written:\n%s", out.String())
	}
}

// TestAddonAttachReportsAMovedVariable: a rename removes the old name, and the CLI says which one and
// why. A variable disappearing from an app's environment is not something to leave a reader to infer.
func TestAddonAttachReportsAMovedVariable(t *testing.T) {
	isolateConfig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "addon": "postgres", "environment": "prod",
			"secret_key": "DB_URL", "previous_secret_key": "DATABASE_URL",
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "attach", "postgres", "web", "--as", "DB_URL",
		"--control-plane", srv.URL, "--token", "tok"}, &out, &errb)
	if err != nil {
		t.Fatalf("addon attach --as: %v (stderr: %s)", err, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "DATABASE_URL") || !strings.Contains(s, "rotated") {
		t.Errorf("output does not report the removed key and why it went:\n%s", s)
	}
}

// TestAddonAttachWithoutAsIsUnchanged: no flag, no change — the same route clients have always sent.
func TestAddonAttachWithoutAsIsUnchanged(t *testing.T) {
	isolateConfig(t)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "addon": "postgres", "environment": "prod", "secret_key": "DATABASE_URL",
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"addon", "attach", "postgres", "web",
		"--control-plane", srv.URL, "--token", "tok"}, &out, &errb); err != nil {
		t.Fatalf("addon attach: %v (stderr: %s)", err, errb.String())
	}
	if gotPath != "/v1/addons/attach" {
		t.Errorf("path = %q, want the unnarrowed attach", gotPath)
	}
}
