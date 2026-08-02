// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"strings"
	"testing"
)

// cloudConfig is a config with the managed product selected and nothing else.
func cloudConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{}
	if err := cfg.SetTarget(CloudTarget()); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	return cfg
}

// TestResolveOperateAcceptsACloudTarget is the thing that was missing: a person can sign in to the
// managed product and then have an ordinary command act through it. There is no cluster in the
// answer — no context, no control-plane namespace, no scoped kubeconfig — because a tenant has none.
func TestResolveOperateAcceptsACloudTarget(t *testing.T) {
	kubeconfig := writeKubeconfig(t) // present, and deliberately irrelevant

	got, err := ResolveOperate(cloudConfig(t), kubeconfig)
	if err != nil {
		t.Fatalf("ResolveOperate: %v", err)
	}
	if !got.Cloud() {
		t.Fatalf("got %+v, want a Burrow Cloud resolution", got)
	}
	if got.Endpoint != CloudEndpoint {
		t.Errorf("endpoint = %q, want %q", got.Endpoint, CloudEndpoint)
	}
	if got.Target != CloudEndpoint || got.Mode != ModeTargeted {
		t.Errorf("got target %q mode %q, want the selected target in targeted mode", got.Target, got.Mode)
	}
	if got.Context != "" || got.ControlPlaneNamespace != "" || got.AgentKubeconfig != "" {
		t.Errorf("got %+v, want no cluster fields: a managed tenant has no cluster", got)
	}
	if !strings.Contains(got.Render(), CloudEndpoint) || !strings.Contains(got.Render(), "managed product") {
		t.Errorf("Render() = %q, want it to say the command is going to the managed product", got.Render())
	}
}

// TestResolveOperateIgnoresTheKubeconfigForACloudTarget: nothing about the kubeconfig can change
// where a cloud command goes, including there being no kubeconfig at all. A tenant who has never run
// kubectl must not be stopped by it.
func TestResolveOperateIgnoresTheKubeconfigForACloudTarget(t *testing.T) {
	got, err := ResolveOperate(cloudConfig(t), "/no/such/kubeconfig")
	if err != nil {
		t.Fatalf("ResolveOperate with no kubeconfig: %v", err)
	}
	if !got.Cloud() || got.Endpoint != CloudEndpoint {
		t.Errorf("got %+v, want the cloud target regardless of the kubeconfig", got)
	}
}

// TestResolveStillRefusesACloudTarget is the guard on everything this change did NOT make work.
// Resolve is what the commands that genuinely need a cluster call — install, the cluster and policy
// surfaces, anything that reads a kubeconfig — and they must keep refusing rather than silently
// resolving to an empty context and acting on whatever kubectl last pointed at. If this test ever
// fails because Resolve started accepting a cloud target, a privileged command started running
// somewhere nobody chose.
func TestResolveStillRefusesACloudTarget(t *testing.T) {
	kubeconfig := writeKubeconfig(t)

	got, err := Resolve(cloudConfig(t), kubeconfig)
	if err == nil {
		t.Fatalf("Resolve accepted a cloud target and returned %+v", got)
	}
	for _, want := range []string{CloudEndpoint, "no cluster", "burrow auth switch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	// It must not read as a kubeconfig problem: that sends the reader looking for a cluster they do
	// not have.
	if strings.Contains(err.Error(), "kubeconfig may have moved") {
		t.Errorf("error = %q, want a message about the target rather than a kubeconfig failure", err)
	}
}

// TestResolveOperateLeavesTheClusterPathUntouched pins the self-hosted path against the new entry
// point: for every case that does not involve the managed product, ResolveOperate and Resolve agree
// exactly. It would fail if the cloud branch leaked into a cluster resolution.
func TestResolveOperateLeavesTheClusterPathUntouched(t *testing.T) {
	kubeconfig := writeKubeconfig(t) // current context is do-nyc1-dev

	targeted := &Config{}
	if err := targeted.SetTarget(KubernetesTarget("do-nyc1-nonprod")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	pinnedInside := &Config{
		Current:      "nonprod",
		Environments: []Environment{{Name: "nonprod", Context: "do-nyc1-nonprod", AppNamespace: "team-y", Env: "nonprod"}},
	}
	if err := pinnedInside.SetTarget(KubernetesTarget("do-nyc1-nonprod")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	for name, cfg := range map[string]*Config{
		"a selected cluster target": targeted,
		"a pin inside the target":   pinnedInside,
		"no target at all":          {Environments: []Environment{{Name: "dev", Context: "do-nyc1-dev"}}},
		"a nil config":              nil,
	} {
		t.Run(name, func(t *testing.T) {
			want, wantErr := Resolve(cfg, kubeconfig)
			got, gotErr := ResolveOperate(cfg, kubeconfig)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("Resolve err = %v, ResolveOperate err = %v", wantErr, gotErr)
			}
			if got != want {
				t.Errorf("ResolveOperate = %+v, Resolve = %+v; the cluster path must be identical through both", got, want)
			}
			if got.Cloud() {
				t.Errorf("a cluster resolution reported itself as the managed product: %+v", got)
			}
		})
	}
}
