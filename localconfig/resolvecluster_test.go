// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"strings"
	"testing"
)

// TestResolveClusterFollowsTheSelectedTarget is the property ADR-0084 §4 turns on: the target
// decides the cluster, and the kubeconfig's current context is not consulted for it.
func TestResolveClusterFollowsTheSelectedTarget(t *testing.T) {
	kubeconfig := writeKubeconfig(t) // current context is do-nyc1-dev
	cfg := &Config{}
	if err := cfg.SetTarget(KubernetesTarget("do-nyc1-nonprod")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	got, err := ResolveCluster(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("ResolveCluster: %v", err)
	}
	if got.Context != "do-nyc1-nonprod" {
		t.Errorf("context = %q, want the target's context rather than the kubectl current one", got.Context)
	}
	if !got.Selected() || got.Target != "do-nyc1-nonprod" {
		t.Errorf("got %+v, want the selected target named", got)
	}
	if got.Kind != TargetKindKubernetes {
		t.Errorf("kind = %q, want a Kubernetes target", got.Kind)
	}
}

// TestResolveClusterWithNoTargetLeavesTheContextEmpty pins the compatibility half. With nothing
// selected there is nothing to say, and an empty context is what makes the caller fall through to
// the kubeconfig's current context — the pre-ADR-0078 behaviour, which is still the default.
func TestResolveClusterWithNoTargetLeavesTheContextEmpty(t *testing.T) {
	kubeconfig := writeKubeconfig(t)

	for name, cfg := range map[string]*Config{"nil config": nil, "empty config": {}} {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveCluster(cfg, kubeconfig)
			if err != nil {
				t.Fatalf("ResolveCluster: %v", err)
			}
			if got.Context != "" || got.Selected() {
				t.Errorf("got %+v, want nothing decided", got)
			}
		})
	}
}

// TestResolveClusterIgnoresAPinnedHandle is the reason this is a third entry point rather than a
// call to Resolve. A pin is a statement about which apps are being operated; it must never redirect
// a cluster-wide privileged command (ADR-0036), and Resolve would hand back the pinned handle's
// cluster here.
func TestResolveClusterIgnoresAPinnedHandle(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{
		Current: "elsewhere",
		Environments: []Environment{
			{Name: "elsewhere", Context: "do-nyc1-nonprod", AppNamespace: "team-y", Env: "nonprod"},
		},
	}

	got, err := ResolveCluster(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("ResolveCluster: %v", err)
	}
	if got.Context != "" {
		t.Errorf("context = %q, want the pin ignored so the kubeconfig's current context applies", got.Context)
	}

	// And the contrast: the environment resolution DOES follow the pin, which is what it is for.
	resolved, err := Resolve(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Context != "do-nyc1-nonprod" {
		t.Errorf("Resolve context = %q, want the pinned handle's cluster", resolved.Context)
	}
}

// TestResolveClusterRefusesTheManagedProduct is the fix for
// [cloud#209](https://github.com/burrow-cloud/cloud/issues/209), and the reason it has to be a
// refusal rather than a reported fact.
//
// A Burrow Cloud target names no cluster, so the honest context for it is the empty string — which is
// also what "no target selected" resolves to, and that means "follow the kubeconfig's current
// context". One value, two meanings, and every caller that did not think to check the second one
// silently acted on, or reported about, whatever cluster kubectl happened to point at. The value has
// to stop carrying the second meaning; asking callers to be careful is what was already being done.
func TestResolveClusterRefusesTheManagedProduct(t *testing.T) {
	kubeconfig := writeKubeconfig(t) // current context is do-nyc1-dev
	cfg := &Config{}
	if err := cfg.SetTarget(CloudTarget()); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	got, err := ResolveCluster(cfg, kubeconfig)
	if err == nil {
		t.Fatalf("the managed product resolved to %+v; a caller reading Context there acts on the ambient kube context", got)
	}
	if got.Context != "" {
		t.Errorf("context = %q on the error path, want none", got.Context)
	}
	// The message has to name the target and the way out, because the reader's question is "why is
	// this command refusing" and the answer is a selection they made in another session.
	for _, want := range []string{CloudEndpoint, "burrow auth switch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	// And the negative that names the bug: whatever it says, it must not hand back the kubeconfig's
	// current context under any name.
	if strings.Contains(err.Error(), "do-nyc1-dev") {
		t.Errorf("error = %q, want no mention of the ambient kube context", err)
	}
}

// TestResolveClusterRefusesAStaleTarget. A context name is user-controlled and can be renamed away,
// and the failure worth avoiding is a privileged command quietly using a different cluster because
// the one it was told about is gone. It is caught here, where the message can name both.
func TestResolveClusterRefusesAStaleTarget(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{}
	if err := cfg.SetTarget(KubernetesTarget("renamed-away")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	_, err := ResolveCluster(cfg, kubeconfig)
	if err == nil {
		t.Fatal("a target naming a missing context resolved")
	}
	for _, want := range []string{"renamed-away", "not in your kubeconfig", "burrow auth login", "burrow auth switch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestResolveClusterRefusesAStaleTargetEvenWithAScopedHandle. Issue #488 relaxes the PINNED path,
// where a handle's own scoped credential reaches the cluster without a context. Nothing about that
// reaches here: a privileged command is not scoped to an environment, a pin is never consulted for
// which cluster it acts on, and the credential a handle happens to carry is not the target's to use.
func TestResolveClusterRefusesAStaleTargetEvenWithAScopedHandle(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{
		Current: "prod",
		Environments: []Environment{
			{Name: "prod", Context: "renamed-away", AgentKubeconfig: writeScopedCredential(t), AgentContext: "burrow-agent"},
		},
	}
	if err := cfg.SetTarget(KubernetesTarget("renamed-away")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	if _, err := ResolveCluster(cfg, kubeconfig); err == nil {
		t.Fatal("a target naming a missing context resolved because a pinned handle carried a credential")
	}
}
