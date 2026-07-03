// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/localconfig"
)

// scopedConfig points $BURROW_CONFIG at a temp file holding a single handle for kube context "prod"
// carrying the given scoped credential, and returns the scoped kubeconfig path. When agentKubeconfig
// is empty the handle records no scoped credential (a pre-phase-1 handle).
func scopedConfig(t *testing.T, agentKubeconfig string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("BURROW_CONFIG", path)
	cfg := &localconfig.Config{
		Environments: []localconfig.Environment{{
			Name:            "prod",
			Context:         "prod",
			AgentKubeconfig: agentKubeconfig,
			AgentContext:    "burrow-agent",
		}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// writeScopedKubeconfig writes a placeholder scoped kubeconfig and returns its path.
func writeScopedKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("write scoped kubeconfig: %v", err)
	}
	return path
}

// TestConnectOptionsSelectsScopedForRegisteredContext confirms the MCP factory defaults a registered
// handle's context to its scoped, burrowd-only kubeconfig (ADR-0038 phase 2).
func TestConnectOptionsSelectsScopedForRegisteredContext(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	opts := connectOptions("prod", "", "burrow", &errb)
	if opts.Kubeconfig != scoped {
		t.Errorf("kubeconfig = %q, want the scoped path %q", opts.Kubeconfig, scoped)
	}
	if opts.Context != "burrow-agent" {
		t.Errorf("context = %q, want the scoped context burrow-agent", opts.Context)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errb.String())
	}
}

// TestConnectOptionsUnregisteredContextUsesAmbient confirms a context with no matching handle falls
// back to the ambient kubeconfig.
func TestConnectOptionsUnregisteredContextUsesAmbient(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	opts := connectOptions("staging", "", "burrow", &errb)
	if opts.Kubeconfig != "" {
		t.Errorf("kubeconfig = %q, want ambient (empty) for an unregistered context", opts.Kubeconfig)
	}
	if opts.Context != "staging" {
		t.Errorf("context = %q, want the requested context unchanged", opts.Context)
	}
}

// TestConnectOptionsExplicitKubeconfigWinsOverScoped confirms BURROW_KUBECONFIG outranks the scoped
// per-handle credential (precedence: URL > explicit kubeconfig > scoped > ambient).
func TestConnectOptionsExplicitKubeconfigWinsOverScoped(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	opts := connectOptions("prod", "/explicit/kubeconfig", "burrow", &errb)
	if opts.Kubeconfig != "/explicit/kubeconfig" {
		t.Errorf("kubeconfig = %q, want the explicit BURROW_KUBECONFIG to win over the scoped credential", opts.Kubeconfig)
	}
	if opts.Context != "prod" {
		t.Errorf("context = %q, want the requested context (not the scoped one) under an explicit kubeconfig", opts.Context)
	}
}

// TestConnectOptionsMissingScopedFileFallsBackWithNote confirms a recorded-but-missing scoped file
// falls back to the ambient kubeconfig and prints a note (ADR-0038 phase 2).
func TestConnectOptionsMissingScopedFileFallsBackWithNote(t *testing.T) {
	scopedConfig(t, filepath.Join(t.TempDir(), "does-not-exist"))

	var errb bytes.Buffer
	opts := connectOptions("prod", "", "burrow", &errb)
	if opts.Kubeconfig != "" {
		t.Errorf("kubeconfig = %q, want ambient (empty) when the scoped file is missing", opts.Kubeconfig)
	}
	if opts.Context != "prod" {
		t.Errorf("context = %q, want the requested context after fallback", opts.Context)
	}
	if !strings.Contains(errb.String(), "missing") {
		t.Errorf("stderr note = %q, want it to report the missing scoped kubeconfig", errb.String())
	}
}

// TestConnectOptionsPrePhase1HandleUsesAmbient confirms a handle that records no scoped credential
// (a cluster installed before phase 1) falls back to the ambient kubeconfig, silently.
func TestConnectOptionsPrePhase1HandleUsesAmbient(t *testing.T) {
	scopedConfig(t, "")

	var errb bytes.Buffer
	opts := connectOptions("prod", "", "burrow", &errb)
	if opts.Kubeconfig != "" {
		t.Errorf("kubeconfig = %q, want ambient (empty) for a handle with no scoped credential", opts.Kubeconfig)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr for a pre-phase-1 handle: %q", errb.String())
	}
}

// TestClientFactoryControlPlaneURLWinsOverEverything confirms BURROW_CONTROL_PLANE_URL is the highest
// precedence: the factory returns a single direct-URL client for every context, never resolving a
// scoped kubeconfig (ADR-0038 phase 2 precedence).
func TestClientFactoryControlPlaneURLWinsOverEverything(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "https://burrowd.example.com")
	t.Setenv("BURROW_API_TOKEN", "s3cr3t")

	var errb bytes.Buffer
	factory, err := clientFactory(context.Background(), &errb)
	if err != nil {
		t.Fatalf("clientFactory: %v", err)
	}
	c1, err := factory("prod")
	if err != nil {
		t.Fatalf("factory(prod): %v", err)
	}
	c2, err := factory("staging")
	if err != nil {
		t.Fatalf("factory(staging): %v", err)
	}
	if c1 == nil || c1 != c2 {
		t.Errorf("direct-URL factory returned %p and %p, want one shared client for every context", c1, c2)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr on the direct-URL path: %q", errb.String())
	}
}
