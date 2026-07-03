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
// handle's context to its scoped, burrowd-only kubeconfig (ADR-0038).
func TestConnectOptionsSelectsScopedForRegisteredContext(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	opts, err := connectOptions("prod", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("connectOptions: %v", err)
	}
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
// back to the ambient kubeconfig in non-strict mode.
func TestConnectOptionsUnregisteredContextUsesAmbient(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	opts, err := connectOptions("staging", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("connectOptions: %v", err)
	}
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
	opts, err := connectOptions("prod", "/explicit/kubeconfig", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("connectOptions: %v", err)
	}
	if opts.Kubeconfig != "/explicit/kubeconfig" {
		t.Errorf("kubeconfig = %q, want the explicit BURROW_KUBECONFIG to win over the scoped credential", opts.Kubeconfig)
	}
	if opts.Context != "prod" {
		t.Errorf("context = %q, want the requested context (not the scoped one) under an explicit kubeconfig", opts.Context)
	}
}

// TestConnectOptionsMissingScopedFileErrors confirms a recorded-but-missing scoped file is a hard
// error and does NOT fall back to the ambient kubeconfig, even in non-strict mode (ADR-0038: a handle
// that declares a scoped credential and can't find it is a misconfiguration, never an escalation).
func TestConnectOptionsMissingScopedFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	scopedConfig(t, missing)

	var errb bytes.Buffer
	opts, err := connectOptions("prod", "", "burrow", false, &errb)
	if err == nil {
		t.Fatalf("connectOptions returned opts %+v, want an error for a recorded-but-missing scoped file", opts)
	}
	if opts.Kubeconfig != "" {
		t.Errorf("opts.Kubeconfig = %q, want empty (no ambient options) on error", opts.Kubeconfig)
	}
	if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "Refusing") {
		t.Errorf("error = %q, want it to report the missing scoped kubeconfig and the refusal to fall back", err)
	}
}

// TestConnectOptionsPrePhase1HandleUsesAmbient confirms a handle that records no scoped credential
// (a cluster installed before the scoped credential existed) falls back to the ambient kubeconfig,
// silently, in non-strict mode (backward compatibility).
func TestConnectOptionsPrePhase1HandleUsesAmbient(t *testing.T) {
	scopedConfig(t, "")

	var errb bytes.Buffer
	opts, err := connectOptions("prod", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("connectOptions: %v", err)
	}
	if opts.Kubeconfig != "" {
		t.Errorf("kubeconfig = %q, want ambient (empty) for a handle with no scoped credential", opts.Kubeconfig)
	}
	if errb.Len() != 0 {
		t.Errorf("unexpected stderr for a pre-scoped-credential handle: %q", errb.String())
	}
}

// TestConnectOptionsStrictUsesScopedCredential confirms strict mode uses a valid scoped credential.
func TestConnectOptionsStrictUsesScopedCredential(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	opts, err := connectOptions("prod", "", "burrow", true, &errb)
	if err != nil {
		t.Fatalf("connectOptions (strict): %v", err)
	}
	if opts.Kubeconfig != scoped {
		t.Errorf("kubeconfig = %q, want the scoped path %q in strict mode", opts.Kubeconfig, scoped)
	}
	if opts.Context != "burrow-agent" {
		t.Errorf("context = %q, want the scoped context burrow-agent in strict mode", opts.Context)
	}
}

// TestConnectOptionsStrictNoScopedCredentialErrors confirms strict mode refuses the ambient fallback
// for a handle with no scoped credential (unregistered or pre-scoped-credential).
func TestConnectOptionsStrictNoScopedCredentialErrors(t *testing.T) {
	scopedConfig(t, "")

	var errb bytes.Buffer
	opts, err := connectOptions("prod", "", "burrow", true, &errb)
	if err == nil {
		t.Fatalf("connectOptions returned opts %+v, want a strict-mode error with no scoped credential", opts)
	}
	if opts.Kubeconfig != "" {
		t.Errorf("opts.Kubeconfig = %q, want empty (no ambient options) on strict-mode error", opts.Kubeconfig)
	}
	if !strings.Contains(err.Error(), "BURROW_MCP_REQUIRE_SCOPED") {
		t.Errorf("error = %q, want it to name BURROW_MCP_REQUIRE_SCOPED", err)
	}
}

// TestConnectOptionsStrictExplicitKubeconfigAllowed confirms strict mode still honors an explicit
// BURROW_KUBECONFIG (the operator's deliberate choice, not the implicit ambient fallback).
func TestConnectOptionsStrictExplicitKubeconfigAllowed(t *testing.T) {
	var errb bytes.Buffer
	opts, err := connectOptions("prod", "/explicit/kubeconfig", "burrow", true, &errb)
	if err != nil {
		t.Fatalf("connectOptions (strict, explicit kubeconfig): %v", err)
	}
	if opts.Kubeconfig != "/explicit/kubeconfig" {
		t.Errorf("kubeconfig = %q, want the explicit BURROW_KUBECONFIG honored in strict mode", opts.Kubeconfig)
	}
}

// TestClientFactoryStrictControlPlaneURLAllowed confirms strict mode still honors an explicit
// BURROW_CONTROL_PLANE_URL (a direct control-plane URL, not cluster admin).
func TestClientFactoryStrictControlPlaneURLAllowed(t *testing.T) {
	t.Setenv("BURROW_MCP_REQUIRE_SCOPED", "1")
	t.Setenv("BURROW_CONTROL_PLANE_URL", "https://burrowd.example.com")
	t.Setenv("BURROW_API_TOKEN", "s3cr3t")

	var errb bytes.Buffer
	factory, err := clientFactory(context.Background(), &errb)
	if err != nil {
		t.Fatalf("clientFactory: %v", err)
	}
	c, err := factory("prod")
	if err != nil {
		t.Fatalf("factory(prod): %v", err)
	}
	if c == nil {
		t.Errorf("factory returned nil client, want the direct-URL client in strict mode")
	}
}

// TestTruthy confirms BURROW_MCP_REQUIRE_SCOPED truthiness parsing: 1/true/yes (case-insensitive,
// whitespace-trimmed) enable it; empty, 0, and anything else leave it off.
func TestTruthy(t *testing.T) {
	on := []string{"1", "true", "yes", "TRUE", "Yes", " true "}
	for _, v := range on {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false, want true", v)
		}
	}
	off := []string{"", "0", "false", "no", "off", "2", "enabled"}
	for _, v := range off {
		if truthy(v) {
			t.Errorf("truthy(%q) = true, want false", v)
		}
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
