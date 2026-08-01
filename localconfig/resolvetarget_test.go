// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"strings"
	"testing"
)

// TestResolveTargetDecidesTheCluster confirms a selected Kubernetes target decides the context a
// command acts on, rather than whatever kubectl last pointed at (ADR-0078 §1).
func TestResolveTargetDecidesTheCluster(t *testing.T) {
	kubeconfig := writeKubeconfig(t) // current context is do-nyc1-dev
	cfg := &Config{}
	if err := cfg.SetTarget(KubernetesTarget("do-nyc1-nonprod")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	got, err := Resolve(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Context != "do-nyc1-nonprod" {
		t.Errorf("context = %q, want the target's context do-nyc1-nonprod, not the kubectl current one", got.Context)
	}
	if got.Mode != ModeTargeted {
		t.Errorf("mode = %q, want targeted", got.Mode)
	}
	if got.Target != "do-nyc1-nonprod" {
		t.Errorf("target = %q, want the selected target's name", got.Target)
	}
	if !strings.Contains(got.Render(), "do-nyc1-nonprod") {
		t.Errorf("Render() = %q, want it to name the target", got.Render())
	}
}

// TestResolveTargetKeepsAPinInsideIt confirms a pinned handle still applies when it is a handle in
// the target's own cluster: the target says which cluster, the handle says which environment inside
// it.
func TestResolveTargetKeepsAPinInsideIt(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{
		Current: "nonprod",
		Environments: []Environment{
			{Name: "nonprod", Context: "do-nyc1-nonprod", AppNamespace: "team-y", Env: "nonprod"},
		},
	}
	if err := cfg.SetTarget(KubernetesTarget("do-nyc1-nonprod")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	got, err := Resolve(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Mode != ModePinned {
		t.Errorf("mode = %q, want pinned: a pin inside the target still narrows it", got.Mode)
	}
	if got.Name != "nonprod" || got.Env != "nonprod" {
		t.Errorf("resolved handle = %q/%q, want the pinned nonprod handle", got.Name, got.Env)
	}
}

// TestResolveTargetIgnoresAPinForAnotherCluster confirms a pin left over from a different cluster
// does not redirect a command out of the selected target. Acting on the wrong target is the failure
// this design introduces, so a stale pin must not be the thing that causes it.
func TestResolveTargetIgnoresAPinForAnotherCluster(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{
		Current: "dev",
		Environments: []Environment{
			{Name: "dev", Context: "do-nyc1-dev", AppNamespace: "team-x"},
		},
	}
	if err := cfg.SetTarget(KubernetesTarget("do-nyc1-nonprod")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	got, err := Resolve(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Context != "do-nyc1-nonprod" {
		t.Errorf("context = %q, want the target's cluster; a pin for another cluster must not win", got.Context)
	}
	if got.Mode != ModeTargeted {
		t.Errorf("mode = %q, want targeted", got.Mode)
	}
}

// TestResolveStaleTargetIsLegible confirms a target naming a context the kubeconfig no longer holds
// produces an error that says exactly that and names the way out (ADR-0078 "Consequences").
func TestResolveStaleTargetIsLegible(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{}
	if err := cfg.SetTarget(KubernetesTarget("a-cluster-that-moved")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	_, err := Resolve(cfg, kubeconfig)
	if err == nil {
		t.Fatal("Resolve: want an error for a target whose context is gone")
	}
	for _, want := range []string{"a-cluster-that-moved", "not in your kubeconfig", "burrow auth"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestResolveCloudTargetSaysSo confirms a Burrow Cloud target does not silently fall back to some
// cluster: a command that reaches a control plane through a kubeconfig is told which target is
// active and how to switch.
func TestResolveCloudTargetSaysSo(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{}
	if err := cfg.SetTarget(CloudTarget()); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	_, err := Resolve(cfg, kubeconfig)
	if err == nil {
		t.Fatal("Resolve: want an error for a Burrow Cloud target reached through a kubeconfig")
	}
	if !strings.Contains(err.Error(), CloudEndpoint) {
		t.Errorf("error = %q, want it to name the active target", err)
	}
}

// TestResolveWithoutTargetIsUnchanged confirms the pre-ADR-0078 behaviour is exactly preserved for
// anyone who has never run `burrow auth login`: no target means follow the kube context.
func TestResolveWithoutTargetIsUnchanged(t *testing.T) {
	kubeconfig := writeKubeconfig(t)

	got, err := Resolve(&Config{Environments: []Environment{{Name: "dev", Context: "do-nyc1-dev"}}}, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Mode != ModeFollowing {
		t.Errorf("mode = %q, want following", got.Mode)
	}
	if got.Context != "do-nyc1-dev" {
		t.Errorf("context = %q, want the kubectl current context", got.Context)
	}
	if got.Target != "" {
		t.Errorf("target = %q, want empty when none is selected", got.Target)
	}
}
