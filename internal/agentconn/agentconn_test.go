// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package agentconn

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
// carrying the given scoped credential. When agentKubeconfig is empty the handle records no scoped
// credential (a pre-scoped-credential handle).
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

// TestConnectOptionsSelectsScopedForRegisteredContext confirms the factory defaults a registered
// handle's context to its scoped, burrowd-only kubeconfig (ADR-0038).
func TestConnectOptionsSelectsScopedForRegisteredContext(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions: %v", err)
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
	opts, err := ConnectOptions("staging", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions: %v", err)
	}
	if opts.Kubeconfig != "" {
		t.Errorf("kubeconfig = %q, want ambient (empty) for an unregistered context", opts.Kubeconfig)
	}
	if opts.Context != "staging" {
		t.Errorf("context = %q, want the requested context unchanged", opts.Context)
	}
}

// TestConnectOptionsExplicitKubeconfigWinsOverScoped confirms an explicit kubeconfig outranks the
// scoped per-handle credential (precedence: URL > explicit kubeconfig > scoped > ambient).
func TestConnectOptionsExplicitKubeconfigWinsOverScoped(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "/explicit/kubeconfig", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions: %v", err)
	}
	if opts.Kubeconfig != "/explicit/kubeconfig" {
		t.Errorf("kubeconfig = %q, want the explicit kubeconfig to win over the scoped credential", opts.Kubeconfig)
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
	opts, err := ConnectOptions("prod", "", "burrow", false, &errb)
	if err == nil {
		t.Fatalf("ConnectOptions returned opts %+v, want an error for a recorded-but-missing scoped file", opts)
	}
	if opts.Kubeconfig != "" {
		t.Errorf("opts.Kubeconfig = %q, want empty (no ambient options) on error", opts.Kubeconfig)
	}
	if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "Refusing") {
		t.Errorf("error = %q, want it to report the missing scoped kubeconfig and the refusal to fall back", err)
	}
}

// TestConnectOptionsPreScopedHandleUsesAmbient confirms a handle that records no scoped credential
// (a cluster installed before the scoped credential existed) falls back to the ambient kubeconfig,
// silently, in non-strict mode (backward compatibility).
func TestConnectOptionsPreScopedHandleUsesAmbient(t *testing.T) {
	scopedConfig(t, "")

	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions: %v", err)
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
	opts, err := ConnectOptions("prod", "", "burrow", true, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions (strict): %v", err)
	}
	if opts.Kubeconfig != scoped {
		t.Errorf("kubeconfig = %q, want the scoped path %q in strict mode", opts.Kubeconfig, scoped)
	}
	if opts.Context != "burrow-agent" {
		t.Errorf("context = %q, want the scoped context burrow-agent in strict mode", opts.Context)
	}
}

// TestConnectOptionsStrictNoScopedCredentialErrors confirms strict mode refuses the ambient fallback
// for a handle with no scoped credential (unregistered or pre-scoped-credential). The message is
// binary-neutral: it names no environment variable.
func TestConnectOptionsStrictNoScopedCredentialErrors(t *testing.T) {
	scopedConfig(t, "")

	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "", "burrow", true, &errb)
	if err == nil {
		t.Fatalf("ConnectOptions returned opts %+v, want a strict-mode error with no scoped credential", opts)
	}
	if opts.Kubeconfig != "" {
		t.Errorf("opts.Kubeconfig = %q, want empty (no ambient options) on strict-mode error", opts.Kubeconfig)
	}
	if !strings.Contains(err.Error(), "strict mode") {
		t.Errorf("error = %q, want it to report strict mode", err)
	}
	if strings.Contains(err.Error(), "BURROW_MCP_REQUIRE_SCOPED") {
		t.Errorf("error = %q, must not hardcode a binary-specific env var (this package is binary-neutral)", err)
	}
}

// TestConnectOptionsStrictExplicitKubeconfigAllowed confirms strict mode still honors an explicit
// kubeconfig (the operator's deliberate choice, not the implicit ambient fallback).
func TestConnectOptionsStrictExplicitKubeconfigAllowed(t *testing.T) {
	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "/explicit/kubeconfig", "burrow", true, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions (strict, explicit kubeconfig): %v", err)
	}
	if opts.Kubeconfig != "/explicit/kubeconfig" {
		t.Errorf("kubeconfig = %q, want the explicit kubeconfig honored in strict mode", opts.Kubeconfig)
	}
}

// TestNewFactoryControlPlaneURLWinsOverEverything confirms a direct control-plane URL is the highest
// precedence: the factory returns a single direct-URL client for every context, never resolving a
// scoped kubeconfig (ADR-0038 precedence).
func TestNewFactoryControlPlaneURLWinsOverEverything(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	scopedConfig(t, scoped)

	var errb bytes.Buffer
	factory, err := NewFactory(context.Background(), Config{
		ControlPlaneURL: "https://burrowd.example.com",
		Token:           "s3cr3t",
		Strict:          true, // strict must not defeat the explicit direct-URL escape hatch
		Version:         "v0.1.0",
	}, &errb)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
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

// TestNewFactoryControlPlaneURLRequiresToken confirms a direct URL with no token is a hard error.
func TestNewFactoryControlPlaneURLRequiresToken(t *testing.T) {
	var errb bytes.Buffer
	_, err := NewFactory(context.Background(), Config{ControlPlaneURL: "https://burrowd.example.com"}, &errb)
	if err == nil {
		t.Fatal("NewFactory with a URL and no token returned nil error, want an error")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q, want it to name the missing token", err)
	}
}
