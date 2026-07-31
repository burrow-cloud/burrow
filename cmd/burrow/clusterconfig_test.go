// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// cannedLimits answers /v1/config with one limit, so a test can assert what the command renders
// around it.
func cannedLimits(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"limits": []map[string]any{
		{
			"code":        "app.replica_ceiling",
			"value":       "80",
			"description": "the largest replica count a deploy, a scale, or an autoscaler's maximum may ask for",
			"kind":        "count",
			"scope":       "cluster",
			"env_scoped":  true,
			"default":     "50",
		},
	}})
}

// TestClusterConfigList renders the limits table: the effective value, the tier it was set at, and
// the built-in default it reverts to (ADR-0068 §3). The tier is the column that matters — it is
// what tells an operator whether the bound they are reading is one somebody chose.
func TestClusterConfigList(t *testing.T) {
	out, _, err := runCLI(t, cannedLimits, "cluster", "config", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"app.replica_ceiling", "80", "cluster", "50"} {
		if !strings.Contains(out, want) {
			t.Errorf("cluster config list output is missing %q:\n%s", want, out)
		}
	}
}

// TestClusterConfigSetTiers pins which tier a set lands in, which is the whole of ADR-0068 §1's
// distinction on this surface: no --env writes the cluster value, --env writes that environment's.
// The environment travels as a query parameter, exactly as it does for `guard set`.
func TestClusterConfigSetTiers(t *testing.T) {
	var gotMethod, gotPath, gotEnv, gotBody string
	h := func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotEnv = r.Method, r.URL.Path, r.URL.Query().Get("env")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		cannedLimits(w, r)
	}

	out, _, err := runCLI(t, h, "cluster", "config", "set", "app.replica_ceiling", "80")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/config/app.replica_ceiling" {
		t.Errorf("request = %s %s, want PUT /v1/config/app.replica_ceiling", gotMethod, gotPath)
	}
	if gotEnv != "" {
		t.Errorf("a set without --env carried env=%q; the cluster tier names no environment", gotEnv)
	}
	if !strings.Contains(gotBody, `"value":"80"`) {
		t.Errorf("body = %s, want the value", gotBody)
	}
	if !strings.Contains(out, "for the cluster") {
		t.Errorf("output = %q, want it to say which tier the value landed in", out)
	}

	out, _, err = runCLI(t, h, "cluster", "config", "set", "--env", "staging", "app.replica_ceiling", "200")
	if err != nil {
		t.Fatalf("run --env: %v", err)
	}
	if gotEnv != "staging" {
		t.Errorf("env = %q, want staging", gotEnv)
	}
	if !strings.Contains(out, `environment "staging"`) {
		t.Errorf("output = %q, want it to name the environment", out)
	}
}

// TestClusterConfigIsUnderCluster confirms the command is reachable where ADR-0068 §4 puts it,
// rather than under `burrow config` (external credentials) or `burrow app config` (the developer's
// config vars, which reach the pod). Two things named `config` differing in who may set them is how
// the ceiling ended up inside the guardrail set in the first place.
func TestClusterConfigIsUnderCluster(t *testing.T) {
	root := newRootCmd()
	cluster, _, err := root.Find([]string{"cluster", "config", "set"})
	if err != nil {
		t.Fatalf("`cluster config set` is not registered: %v", err)
	}
	if cluster.Name() != "set" || cluster.Parent().Name() != "config" || cluster.Parent().Parent().Name() != "cluster" {
		t.Errorf("`cluster config set` resolved to %q under %q", cluster.CommandPath(), cluster.Parent().CommandPath())
	}
	// `burrow config` stays the credentials group: the two must not have merged.
	creds, _, err := root.Find([]string{"config", "provider"})
	if err != nil || creds.Name() != "provider" {
		t.Errorf("`burrow config provider` should still be the credentials group, got %v (%v)", creds.CommandPath(), err)
	}
}
