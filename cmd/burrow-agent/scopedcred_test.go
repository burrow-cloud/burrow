// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/burrow-cloud/burrow/internal/targetname"
	"github.com/burrow-cloud/burrow/localconfig"
)

// writeKubeconfig writes cfg to a temp file and returns its path.
func writeKubeconfig(t *testing.T, cfg *api.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// kubeconfigWithCurrent writes a kubeconfig naming the given contexts, with `current` selected, and
// points $KUBECONFIG at it — which is the ambient kubeconfig resolution reads with no --kubeconfig.
func kubeconfigWithCurrent(t *testing.T, current string, contexts ...string) string {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["c"] = &api.Cluster{Server: "https://x:6443", InsecureSkipTLSVerify: true}
	cfg.AuthInfos["u"] = &api.AuthInfo{Token: "t"}
	for _, c := range contexts {
		cfg.Contexts[c] = &api.Context{Cluster: "c", AuthInfo: "u"}
	}
	cfg.CurrentContext = current
	return writeKubeconfig(t, cfg)
}

// writeScopedCredential writes a scoped kubeconfig of the shape `burrow install` mints (ADR-0038):
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

// savePinnedHandle points $BURROW_CONFIG at a config holding a single pinned handle recording
// kubeContext and carrying the scoped credential at agentKubeconfig.
func savePinnedHandle(t *testing.T, kubeContext, agentKubeconfig string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("BURROW_CONFIG", path)
	cfg := &localconfig.Config{
		Current: "prod",
		Environments: []localconfig.Environment{{
			Name:            "prod",
			Context:         kubeContext,
			Env:             "prod",
			AgentKubeconfig: agentKubeconfig,
			AgentContext:    "burrow-agent",
		}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// resolveHandle loads the local config and resolves it the way resolve() does, returning both so a
// test can name the target without connecting to anything.
func resolveHandle(t *testing.T, kubeconfig string) (*localconfig.Config, localconfig.Resolved) {
	t.Helper()
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	resolved, err := localconfig.ResolveOperate(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("ResolveOperate: %v", err)
	}
	return cfg, resolved
}

// envelopeFor renders the outcome envelope a mutating verb prints for a named target, so a test
// asserts on the JSON the agent actually reads rather than on the struct behind it.
func envelopeFor(t *testing.T, n targetname.Named) string {
	t.Helper()
	encoded, err := json.Marshal(outcome{Outcome: outcomeExecuted, Operation: "deploy", Target: &n})
	if err != nil {
		t.Fatalf("marshal outcome: %v", err)
	}
	return string(encoded)
}

// TestEnvelopeNamesTheHandleWhenTheRecordedContextIsStale is issue #473 in the form an agent reads.
// A pinned handle whose kube context has been renamed away still reaches its cluster, because the
// scoped credential (ADR-0038) needs no context — but the envelope named the context, so the agent
// was told the change landed on a cluster that does not exist. It now names the environment, and the
// stale string appears in no field of the JSON.
func TestEnvelopeNamesTheHandleWhenTheRecordedContextIsStale(t *testing.T) {
	savePinnedHandle(t, "renamed-away", writeScopedCredential(t))
	t.Setenv("KUBECONFIG", kubeconfigWithCurrent(t, "do-nyc1-dev", "do-nyc1-dev"))

	cfg, resolved := resolveHandle(t, "")
	if resolved.ContextStale == nil {
		t.Fatal("resolution reported no staleness; the test is not exercising the renamed-context path")
	}
	o := &connOpts{}
	named := o.nameTarget(cfg, resolved, resolved.Context)

	if got := named.Clause(); got != "on prod" {
		t.Errorf("clause = %q, want the environment name; the recorded kube context is not where this went", got)
	}
	if named.Context != "" {
		t.Errorf("target context = %q, want none: the connection was made with the scoped credential", named.Context)
	}
	envelope := envelopeFor(t, named)
	if strings.Contains(envelope, "renamed-away") {
		t.Errorf("envelope = %s, want it to name no cluster rather than one that does not exist", envelope)
	}
	if !strings.Contains(envelope, `\"prod\" environment`) {
		t.Errorf("envelope = %s, want it to name the environment that was reached", envelope)
	}
}

// TestEnvelopeNamesTheContextWhenTheHandleIsCurrent keeps the ordinary path exactly as it was: a
// handle whose recorded context is still in the kubeconfig is named by that context, which is what
// the connection was made through.
func TestEnvelopeNamesTheContextWhenTheHandleIsCurrent(t *testing.T) {
	savePinnedHandle(t, "do-nyc1-prod", writeScopedCredential(t))
	t.Setenv("KUBECONFIG", kubeconfigWithCurrent(t, "do-nyc1-prod", "do-nyc1-prod"))

	cfg, resolved := resolveHandle(t, "")
	if resolved.ContextStale != nil {
		t.Fatalf("ContextStale = %v, want nil for a handle the kubeconfig still holds", resolved.ContextStale)
	}
	named := (&connOpts{}).nameTarget(cfg, resolved, resolved.Context)

	if named.Context != "do-nyc1-prod" {
		t.Errorf("target context = %q, want the context this invocation connected through", named.Context)
	}
	if got := named.Clause(); got != "on prod" {
		t.Errorf("clause = %q, want the environment registered for that context", got)
	}
	if envelope := envelopeFor(t, named); !strings.Contains(envelope, "do-nyc1-prod") {
		t.Errorf("envelope = %s, want the non-stale path unchanged", envelope)
	}
}

// TestEnvelopeNamesTheContextOverrideOverAStalePin confirms an explicit --context still decides the
// cluster over a stale pin, and is what the envelope names: the person named where this one command
// goes, so the pin — stale or not — decided nothing.
func TestEnvelopeNamesTheContextOverrideOverAStalePin(t *testing.T) {
	savePinnedHandle(t, "renamed-away", writeScopedCredential(t))
	t.Setenv("KUBECONFIG", kubeconfigWithCurrent(t, "do-nyc1-dev", "do-nyc1-dev"))

	cfg, resolved := resolveHandle(t, "")
	o := &connOpts{context: "do-nyc1-dev"}
	named := o.nameTarget(cfg, resolved, "do-nyc1-dev")

	if named.Context != "do-nyc1-dev" || !named.Override {
		t.Errorf("target = %+v, want the --context override named as what was acted on", named)
	}
	if envelope := envelopeFor(t, named); strings.Contains(envelope, "renamed-away") {
		t.Errorf("envelope = %s, want no mention of a pin this invocation did not use", envelope)
	}
}

// TestStalePinnedContextWithoutACredentialStillRefuses keeps the other half of the pinned path as it
// was, for this binary too: with no credential the recorded context is the only route to the
// cluster, so a name the kubeconfig no longer holds refuses in localconfig, before anything is named.
func TestStalePinnedContextWithoutACredentialStillRefuses(t *testing.T) {
	savePinnedHandle(t, "renamed-away", "")
	t.Setenv("KUBECONFIG", kubeconfigWithCurrent(t, "do-nyc1-dev", "do-nyc1-dev"))

	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := localconfig.ResolveOperate(cfg, ""); err == nil {
		t.Fatal("resolution proceeded for a handle with nothing to dial with")
	}
}

// TestScopedCredentialFileIsWhatMakesTheStalenessSurvivable guards the premise the naming rests on:
// the resolution proceeds only because the credential file is on disk and loadable. Removing it puts
// the refusal back, so a target named from the handle always describes a connection that was really
// made with the credential.
func TestScopedCredentialFileIsWhatMakesTheStalenessSurvivable(t *testing.T) {
	scoped := writeScopedCredential(t)
	savePinnedHandle(t, "renamed-away", scoped)
	t.Setenv("KUBECONFIG", kubeconfigWithCurrent(t, "do-nyc1-dev", "do-nyc1-dev"))
	if err := os.Remove(scoped); err != nil {
		t.Fatalf("remove the scoped credential: %v", err)
	}

	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := localconfig.ResolveOperate(cfg, ""); err == nil {
		t.Fatal("resolution proceeded with the scoped credential file gone")
	}
}
