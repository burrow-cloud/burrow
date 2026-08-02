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
	if got.Cloud() {
		t.Error("a Kubernetes target reported as the managed product")
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

// TestResolveClusterReportsTheManagedProduct. A Burrow Cloud target names no cluster at all, so it
// resolves to no context and says which it is, leaving the caller to refuse or to say out loud which
// cluster it fell back to.
func TestResolveClusterReportsTheManagedProduct(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{}
	if err := cfg.SetTarget(CloudTarget()); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	got, err := ResolveCluster(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("ResolveCluster: %v", err)
	}
	if !got.Cloud() || !got.Selected() {
		t.Errorf("got %+v, want the managed product reported", got)
	}
	if got.Context != "" {
		t.Errorf("context = %q, want none: a managed tenant has no cluster", got.Context)
	}
	if got.Endpoint != CloudEndpoint {
		t.Errorf("endpoint = %q, want %q", got.Endpoint, CloudEndpoint)
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
