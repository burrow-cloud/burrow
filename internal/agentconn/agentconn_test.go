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

	"github.com/burrow-cloud/burrow/internal/clustercred"
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
	// BURROW_AGENT_REQUIRE_SCOPED is how burrow-agent turns strict mode on, but strict mode is a
	// plain argument here: a caller that spells the switch differently must still get a message it
	// can hand to its own user, so the env var's name must not leak into this package's prose.
	if strings.Contains(err.Error(), "BURROW_AGENT_REQUIRE_SCOPED") {
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

// installIDConfig points $BURROW_CONFIG at a temp file holding a handle for kube context "prod" that
// records an install id, so the agent's resolution can be checked for it.
func installIDConfig(t *testing.T, installID string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("BURROW_CONFIG", path)
	cfg := &localconfig.Config{
		Environments: []localconfig.Environment{{
			Name:      "prod",
			Context:   "prod",
			InstallID: installID,
		}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// TestConnectOptionsCarriesTheInstallID confirms the agent sends the install its handle was
// registered against (ADR-0084 §5). The agent is the primary caller — it is what deploys — so a
// check that covered only the CLI would leave the failure the record is actually about, a deploy
// landing on a cluster rebuilt under a reused context name, unprotected.
func TestConnectOptionsCarriesTheInstallID(t *testing.T) {
	installIDConfig(t, "install-abc")

	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions: %v", err)
	}
	if opts.InstallID != "install-abc" {
		t.Errorf("InstallID = %q, want install-abc from the registered handle", opts.InstallID)
	}
}

// TestConnectOptionsWithoutAnInstallIDSendsNone confirms both ways an agent legitimately has no id —
// a handle registered before ids existed, and a context registered to no handle at all — resolve to
// an empty id, which sends no header and is served.
func TestConnectOptionsWithoutAnInstallIDSendsNone(t *testing.T) {
	installIDConfig(t, "")

	var errb bytes.Buffer
	for _, kubeContext := range []string{"prod", "staging"} {
		opts, err := ConnectOptions(kubeContext, "", "burrow", false, &errb)
		if err != nil {
			t.Fatalf("ConnectOptions(%q): %v", kubeContext, err)
		}
		if opts.InstallID != "" {
			t.Errorf("context %q: InstallID = %q, want empty", kubeContext, opts.InstallID)
		}
	}
}

// TestConnectOptionsExplicitKubeconfigSendsNoInstallID confirms the operator's escape hatch stays an
// escape hatch: naming a kubeconfig by hand chooses the route directly, so an id recorded for that
// context no longer describes what is on the other end and is not asserted.
func TestConnectOptionsExplicitKubeconfigSendsNoInstallID(t *testing.T) {
	installIDConfig(t, "install-abc")

	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "/explicit/kubeconfig", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions: %v", err)
	}
	if opts.InstallID != "" {
		t.Errorf("InstallID = %q, want none under an explicit kubeconfig", opts.InstallID)
	}
}

// TestConnectOptionsCarriesTheAgentsOwnCredential confirms the agent presents the credential issued
// to IT, not the person's, and not the install's shared token (ADR-0084 §3).
//
// The distinction is the whole feature: revoking the agent has to stop the agent and leave the
// person signed in, which is only true while the two are different tokens. Reading the person's file
// here would be a one-line regression with no other symptom.
func TestConnectOptionsCarriesTheAgentsOwnCredential(t *testing.T) {
	installIDConfig(t, "install-abc")
	if _, err := clustercred.Store(clustercred.KindCLI, clustercred.Credential{
		InstallID: "install-abc", Token: "the-persons-token",
	}); err != nil {
		t.Fatalf("storing the person's credential: %v", err)
	}
	if _, err := clustercred.Store(clustercred.KindAgent, clustercred.Credential{
		InstallID: "install-abc", Kind: "agent", Token: "the-agents-token",
	}); err != nil {
		t.Fatalf("storing the agent's credential: %v", err)
	}

	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions: %v", err)
	}
	if opts.Token != "the-agents-token" {
		t.Errorf("Token = %q, want the agent's own", opts.Token)
	}
}

// TestConnectOptionsWithoutAnAgentCredentialPresentsNone: an install nobody has signed in to, and
// every install today, has no agent credential on disk. The agent presents nothing extra and the
// transport reads the install's shared token exactly as it always has.
func TestConnectOptionsWithoutAnAgentCredentialPresentsNone(t *testing.T) {
	installIDConfig(t, "install-abc")

	var errb bytes.Buffer
	opts, err := ConnectOptions("prod", "", "burrow", false, &errb)
	if err != nil {
		t.Fatalf("ConnectOptions: %v", err)
	}
	if opts.Token != "" {
		t.Errorf("Token = %q, want empty: with no credential the shared install token is what gets used", opts.Token)
	}
}
