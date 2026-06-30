// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/localconfig"
)

// fakeScanProbe installs a probe seam keyed by context name and restores the original after the
// test, so `burrow env scan` runs without touching a real cluster.
func fakeScanProbe(t *testing.T, byContext map[string]scanProbe) {
	t.Helper()
	orig := probeBurrowdFn
	probeBurrowdFn = func(_ context.Context, _ /*kubeconfig*/, kubeContext, _ /*namespace*/ string) scanProbe {
		return byContext[kubeContext]
	}
	t.Cleanup(func() { probeBurrowdFn = orig })
}

func TestEnvScanListsAndRegistersInstalled(t *testing.T) {
	kc := writeKubeconfig(t, twoContextConfig("https://staging:6443", "https://prod:6443"))
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))

	// staging has burrowd; prod does not. Only staging should get a handle.
	fakeScanProbe(t, map[string]scanProbe{
		"staging": {status: statusInstalled, version: "v0.2.1"},
		"prod":    {status: statusNotInstalled},
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env scan: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{"CONTEXT", "CLUSTER", "BURROWD", "VERSION", "staging", "c-staging", "installed", "v0.2.1", "prod", "not installed"} {
		if !strings.Contains(s, want) {
			t.Errorf("scan output missing %q\n%s", want, s)
		}
	}
	if !strings.Contains(s, `Registered environment "staging"`) {
		t.Errorf("scan should report registering staging\n%s", s)
	}

	// The config now has a handle for staging only.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if env, ok := cfg.Lookup("staging"); !ok {
		t.Errorf("staging handle was not registered: %+v", cfg.Environments)
	} else if env.Context != "staging" {
		t.Errorf("staging handle context = %q, want staging", env.Context)
	}
	if _, ok := cfg.Lookup("prod"); ok {
		t.Errorf("prod has no burrowd; it should not be registered: %+v", cfg.Environments)
	}
}

func TestEnvScanIsIdempotent(t *testing.T) {
	kc := writeKubeconfig(t, twoContextConfig("https://staging:6443", "https://prod:6443"))
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	fakeScanProbe(t, map[string]scanProbe{
		"staging": {status: statusInstalled, version: "v0.2.1"},
		"prod":    {status: statusNotInstalled},
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("first scan: %v\n%s", err, errb.String())
	}

	// A second scan registers nothing new and says so.
	out.Reset()
	errb.Reset()
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("second scan: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "No new environments to register.") {
		t.Errorf("second scan should register nothing new\n%s", out.String())
	}

	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := len(cfg.Environments); got != 1 {
		t.Errorf("after two scans there should be exactly one handle, got %d: %+v", got, cfg.Environments)
	}
}

func TestEnvScanRendersUnreachable(t *testing.T) {
	kc := writeKubeconfig(t, twoContextConfig("https://staging:6443", "https://prod:6443"))
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	fakeScanProbe(t, map[string]scanProbe{
		"staging": {status: statusUnreachable, detail: "connection refused"},
		"prod":    {status: statusNotInstalled},
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env scan: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "unreachable (connection refused)") {
		t.Errorf("scan should render the unreachable reason\n%s", out.String())
	}
	// An unreachable context registers no handle.
	cfg, _ := localconfig.Load()
	if _, ok := cfg.Lookup("staging"); ok {
		t.Errorf("unreachable staging should not be registered: %+v", cfg.Environments)
	}
}
