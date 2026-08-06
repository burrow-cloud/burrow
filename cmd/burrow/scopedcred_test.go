// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd/api"

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
// given scoped credential, and $KUBECONFIG at a fixture holding the handle's context. It returns
// the handle's kube context.
//
// The fixture is what makes these tests say the same thing everywhere (issue #486). Resolution
// checks a pinned handle's context against the kubeconfig, and with only $BURROW_CONFIG isolated
// that check read whatever kubeconfig the developer happened to have: on a bare machine, and on
// CI's runners, there are no contexts and the check is skipped, so the tests passed; on a machine
// with a real kubeconfig the pinned context is absent from it and every one of them failed. A test
// whose answer depends on who runs it is not testing what it claims to.
func saveScopedHandle(t *testing.T, agentKubeconfig, agentContext string) string {
	t.Helper()
	tempConfig(t)
	const kubeContext = "do-nyc1-prod"
	t.Setenv("KUBECONFIG", kubeconfigWithCurrent(t, kubeContext, kubeContext))
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

// saveStaleHandle is saveScopedHandle for a handle whose recorded kube context has been renamed
// away: the ambient kubeconfig ($KUBECONFIG, which is what resolveTarget reads with no --kubeconfig)
// holds a different context entirely. It returns the scoped credential's path.
func saveStaleHandle(t *testing.T, agentKubeconfig string) {
	t.Helper()
	tempConfig(t)
	t.Setenv("KUBECONFIG", kubeconfigWithCurrent(t, "do-nyc1-dev", "do-nyc1-dev"))
	cfg := &localconfig.Config{
		Current: "prod",
		Environments: []localconfig.Environment{{
			Name:            "prod",
			Context:         "renamed-away",
			Env:             "prod",
			AgentKubeconfig: agentKubeconfig,
			AgentContext:    "burrow-agent",
		}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// writeScopedCredential writes a scoped kubeconfig of the shape `burrow cluster install` mints (ADR-0038):
// self-contained, one context, everything needed to reach burrowd with no ambient kubeconfig. It is
// what makes a renamed kube context harmless, so it has to be a real loadable one here.
func writeScopedCredential(t *testing.T) string {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["burrow"] = &api.Cluster{Server: "https://prod.example:6443", CertificateAuthorityData: []byte("ca")}
	cfg.AuthInfos["burrow-agent"] = &api.AuthInfo{Token: "scoped-token"}
	cfg.Contexts["burrow-agent"] = &api.Context{Cluster: "burrow", AuthInfo: "burrow-agent", Namespace: "burrow"}
	cfg.CurrentContext = "burrow-agent"
	return writeKubeconfig(t, cfg)
}

// TestStalePinnedContextConnectsOnTheScopedCredential is issue #488: `burrow app list` refused
// outright after a kube context was renamed, although the handle's scoped credential reaches the
// cluster without one. The command now resolves, connects with the credential, and — because the
// recorded name is NOT where it went — names the environment rather than a context that no longer
// exists (issue #473, which the refusal was protecting).
func TestStalePinnedContextConnectsOnTheScopedCredential(t *testing.T) {
	scoped := writeScopedCredential(t)
	saveStaleHandle(t, scoped)

	o := &commonOpts{}
	tgt, err := o.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget should proceed on the scoped credential, got: %v", err)
	}
	if tgt.agentKubeconfig != scoped || tgt.agentContext != "burrow-agent" {
		t.Fatalf("target agent fields = %q/%q, want the scoped credential %q/burrow-agent", tgt.agentKubeconfig, tgt.agentContext, scoped)
	}
	opts := o.connectOptions(tgt)
	if opts.Kubeconfig != scoped || opts.Context != "burrow-agent" {
		t.Errorf("connect = %q/%q, want the scoped credential's own kubeconfig and context", opts.Kubeconfig, opts.Context)
	}
	if got := o.acted.Clause(); got != "on prod" {
		t.Errorf("acted-on clause = %q, want the environment name; the recorded kube context is not where this went", got)
	}
	if strings.Contains(o.acted.Detail, "renamed-away") || o.acted.Context == "renamed-away" {
		t.Errorf("acted = %+v, want no mention of the stale kube context", o.acted)
	}
	encoded, err := json.Marshal(o.acted)
	if err != nil {
		t.Fatalf("marshal target: %v", err)
	}
	if strings.Contains(string(encoded), "renamed-away") {
		t.Errorf("--json target = %s, want it to name no cluster rather than one that does not exist", encoded)
	}
}

// TestStalePinnedContextIsNotedOnce confirms the staleness is said out loud — a note on stderr
// naming the environment, the recorded context and the command that re-points it — and said only
// once per invocation, since nothing about the configuration changes between two resolutions.
func TestStalePinnedContextIsNotedOnce(t *testing.T) {
	saveStaleHandle(t, writeScopedCredential(t))

	o := &commonOpts{}
	tgt, err := o.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	var errb bytes.Buffer
	o.noteStaleHandleContext(tgt, &errb)
	note := errb.String()
	for _, want := range []string{"prod", "renamed-away", `burrow env use prod --context`} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to contain %q", note, want)
		}
	}
	o.noteStaleHandleContext(tgt, &errb)
	if errb.String() != note {
		t.Errorf("note printed twice:\n%s", errb.String())
	}
}

// TestStalePinnedContextStillRefusesWithoutAScopedCredential keeps the other half of the pinned path
// as it was: with no credential the recorded context is the only route to the cluster, so a name the
// kubeconfig no longer holds refuses, naming the handle and the fix.
func TestStalePinnedContextStillRefusesWithoutAScopedCredential(t *testing.T) {
	saveStaleHandle(t, "")

	_, err := (&commonOpts{}).resolveTarget()
	var missing *localconfig.ContextMissingError
	if !errors.As(err, &missing) || !missing.Pinned {
		t.Fatalf("error = %v, want the pinned refusal for a handle with nothing else to dial with", err)
	}
}

// TestStalePinnedContextRefusesUnderAKubeconfigOverride covers the one flag that turns the credential
// off without naming a cluster instead. An explicit --kubeconfig sends the connection through the
// recorded context, which is the very name that kubeconfig does not have, so the refusal stands —
// and it is the message that names the handle rather than a connection error that names neither.
func TestStalePinnedContextRefusesUnderAKubeconfigOverride(t *testing.T) {
	saveStaleHandle(t, writeScopedCredential(t))

	o := &commonOpts{kubeconfig: kubeconfigWithCurrent(t, "do-nyc1-dev", "do-nyc1-dev")}
	_, err := o.resolveTarget()
	var missing *localconfig.ContextMissingError
	if !errors.As(err, &missing) || !missing.Pinned {
		t.Fatalf("error = %v, want the pinned refusal when --kubeconfig turns the scoped credential off", err)
	}
}

// TestStalePinnedContextHonoursAContextOverride confirms an explicit --context still decides the
// cluster over a stale pin. The person named where this one command goes; the pin decided nothing,
// so there is no note to print and nothing to refuse.
func TestStalePinnedContextHonoursAContextOverride(t *testing.T) {
	saveStaleHandle(t, writeScopedCredential(t))

	o := &commonOpts{context: "do-nyc1-dev"}
	tgt, err := o.resolveTarget()
	if err != nil {
		t.Fatalf("resolveTarget with --context: %v", err)
	}
	if tgt.context != "do-nyc1-dev" {
		t.Errorf("context = %q, want the --context override", tgt.context)
	}
	if tgt.stale != nil {
		t.Errorf("stale = %v, want nothing said about a pin this invocation did not use", tgt.stale)
	}
	var errb bytes.Buffer
	o.noteStaleHandleContext(tgt, &errb)
	if errb.Len() != 0 {
		t.Errorf("stderr = %q, want silence under a --context override", errb.String())
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
