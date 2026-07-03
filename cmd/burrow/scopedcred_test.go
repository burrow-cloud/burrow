// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/localconfig"
)

// writeScopedKubeconfig writes a placeholder scoped agent kubeconfig to a temp file and returns its
// path, so a test can assert the operate path selects it without needing a real cluster.
func writeScopedKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("write scoped kubeconfig: %v", err)
	}
	return path
}

// saveScopedHandle points $BURROW_CONFIG at a temp file holding a single pinned handle carrying the
// given scoped credential, so resolveTarget resolves it without touching the kubeconfig (a pinned
// handle resolves from the config alone). It returns the handle's kube context.
func saveScopedHandle(t *testing.T, agentKubeconfig, agentContext string) string {
	t.Helper()
	tempConfig(t)
	const kubeContext = "do-nyc1-prod"
	cfg := &localconfig.Config{
		Current: "prod",
		Environments: []localconfig.Environment{{
			Name:            "prod",
			Context:         kubeContext,
			Env:             "prod",
			AgentKubeconfig: agentKubeconfig,
			AgentContext:    agentContext,
		}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return kubeContext
}

// TestOperatePathDefaultsToScopedAgentKubeconfig confirms that with a resolved handle carrying the
// scoped credential and no flag overrides, the operate path builds connect.Options pointing at the
// scoped kubeconfig and its context (ADR-0038 phase 2).
func TestOperatePathDefaultsToScopedAgentKubeconfig(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	saveScopedHandle(t, scoped, "burrow-agent")

	o := &commonOpts{}
	tgt, err := o.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tgt.agentKubeconfig != scoped || tgt.agentContext != "burrow-agent" {
		t.Fatalf("target agent fields = %q/%q, want the scoped credential %q/burrow-agent", tgt.agentKubeconfig, tgt.agentContext, scoped)
	}
	opts := o.connectOptions(tgt)
	if opts.Kubeconfig != scoped {
		t.Errorf("connect kubeconfig = %q, want the scoped path %q", opts.Kubeconfig, scoped)
	}
	if opts.Context != "burrow-agent" {
		t.Errorf("connect context = %q, want the scoped context burrow-agent", opts.Context)
	}
}

// TestKubeconfigFlagOverridesScoped confirms an explicit --kubeconfig keeps the ambient/admin path:
// resolveTarget leaves the agent fields unset and connect uses the flag's kubeconfig with the
// resolved kube context (ADR-0038 phase 2).
func TestKubeconfigFlagOverridesScoped(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	kubeContext := saveScopedHandle(t, scoped, "burrow-agent")

	o := &commonOpts{kubeconfig: "/custom/kubeconfig"}
	tgt, err := o.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tgt.agentKubeconfig != "" {
		t.Errorf("agent kubeconfig = %q, want empty when --kubeconfig is set", tgt.agentKubeconfig)
	}
	opts := o.connectOptions(tgt)
	if opts.Kubeconfig != "/custom/kubeconfig" {
		t.Errorf("connect kubeconfig = %q, want the --kubeconfig override", opts.Kubeconfig)
	}
	if opts.Context != kubeContext {
		t.Errorf("connect context = %q, want the resolved handle context %q", opts.Context, kubeContext)
	}
}

// TestContextFlagOverridesScoped confirms an explicit --context keeps the ambient path: the agent
// fields stay unset and connect targets the override context on the ambient kubeconfig.
func TestContextFlagOverridesScoped(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	saveScopedHandle(t, scoped, "burrow-agent")

	o := &commonOpts{context: "some-other-context"}
	tgt, err := o.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tgt.agentKubeconfig != "" {
		t.Errorf("agent kubeconfig = %q, want empty when --context is set", tgt.agentKubeconfig)
	}
	opts := o.connectOptions(tgt)
	if opts.Kubeconfig != "" {
		t.Errorf("connect kubeconfig = %q, want ambient (empty) under --context override", opts.Kubeconfig)
	}
	if opts.Context != "some-other-context" {
		t.Errorf("connect context = %q, want the --context override", opts.Context)
	}
}

// TestControlPlaneURLOverridesScoped confirms --control-plane bypasses handle resolution entirely,
// so no scoped credential is selected (the direct-URL path is highest precedence).
func TestControlPlaneURLOverridesScoped(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	saveScopedHandle(t, scoped, "burrow-agent")

	o := &commonOpts{controlPlane: "https://burrowd.example.com", token: "t"}
	tgt, err := o.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tgt.agentKubeconfig != "" || tgt.context != "" {
		t.Errorf("target = %+v, want no context/agent fields under --control-plane", tgt)
	}
}

// TestPrivilegedClientPathStaysAmbient confirms the privileged path (client) never picks up the
// scoped credential: it builds a target without agent fields, so connect uses the ambient/admin
// kubeconfig even when the resolved handle records a scoped one (ADR-0038 phase 2, the two-credential
// split). client() constructs exactly this target from --context/--namespace.
func TestPrivilegedClientPathStaysAmbient(t *testing.T) {
	scoped := writeScopedKubeconfig(t)
	saveScopedHandle(t, scoped, "burrow-agent")

	o := &commonOpts{namespace: "burrow"}
	privileged := target{context: o.context, controlPlaneNamespace: o.namespace}
	opts := o.connectOptions(privileged)
	if opts.Kubeconfig != "" {
		t.Errorf("privileged connect kubeconfig = %q, want ambient (empty); it must never use the scoped credential", opts.Kubeconfig)
	}
}

// TestHandleWithoutScopedCredentialUsesAmbient confirms a handle recorded before the scoped
// credential existed (no AgentKubeconfig) falls back to the ambient kubeconfig.
func TestHandleWithoutScopedCredentialUsesAmbient(t *testing.T) {
	saveScopedHandle(t, "", "")

	o := &commonOpts{}
	tgt, err := o.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if tgt.agentKubeconfig != "" {
		t.Errorf("agent kubeconfig = %q, want empty for a handle with no scoped credential", tgt.agentKubeconfig)
	}
	opts := o.connectOptions(tgt)
	if opts.Kubeconfig != "" {
		t.Errorf("connect kubeconfig = %q, want ambient (empty)", opts.Kubeconfig)
	}
}

// TestScopedFallbackWhenFileMissing confirms a recorded-but-missing scoped kubeconfig falls back to
// the ambient kubeconfig and prints a one-line note, rather than failing hard (ADR-0038 phase 2).
func TestScopedFallbackWhenFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	tgt := target{context: "do-nyc1-prod", agentKubeconfig: missing, agentContext: "burrow-agent"}

	var errb bytes.Buffer
	got := applyScopedFallback(tgt, &errb)
	if got.agentKubeconfig != "" || got.agentContext != "" {
		t.Errorf("agent fields = %q/%q, want cleared when the scoped file is missing", got.agentKubeconfig, got.agentContext)
	}
	if !strings.Contains(errb.String(), "missing") {
		t.Errorf("stderr note = %q, want it to report the missing scoped kubeconfig", errb.String())
	}
	opts := (&commonOpts{}).connectOptions(got)
	if opts.Kubeconfig != "" {
		t.Errorf("connect kubeconfig = %q, want ambient (empty) after the missing-file fallback", opts.Kubeconfig)
	}
	if opts.Context != "do-nyc1-prod" {
		t.Errorf("connect context = %q, want the resolved handle context after fallback", opts.Context)
	}
}
