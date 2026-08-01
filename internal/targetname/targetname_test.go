// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package targetname

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/localconfig"
)

func configWith(current string, targets ...localconfig.Target) *localconfig.Config {
	return &localconfig.Config{Targets: targets, CurrentTarget: current}
}

// TestForNamesTheChosenName pins the property the whole record turns on: what a person sees is the
// name they picked in the picker, not a server URL or an internal identifier (ADR-0078 §4).
func TestForNamesTheChosenName(t *testing.T) {
	cfg := configWith("prod-cluster", localconfig.KubernetesTarget("prod-cluster"))
	got := For(cfg, "prod-cluster", false)
	if got.Name != "prod-cluster" {
		t.Errorf("Name = %q, want the name chosen in the picker", got.Name)
	}
	if got.Kind != string(localconfig.TargetKindKubernetes) {
		t.Errorf("Kind = %q", got.Kind)
	}
	// The sentence is the target's own Describe, which is what `burrow auth status` prints, so the
	// two cannot drift into describing the same target differently.
	if want := localconfig.KubernetesTarget("prod-cluster").Describe(); got.Detail != want {
		t.Errorf("Detail = %q, want %q (the same string `burrow auth status` prints)", got.Detail, want)
	}
	if c := got.Clause(); c != `on target "prod-cluster"` {
		t.Errorf("Clause() = %q", c)
	}
}

// TestForRefusesToNameATargetItDidNotReach is the load-bearing negative. Naming the recorded target
// on a command that resolved somewhere else would manufacture the exact mistake the naming exists to
// catch: a person would read "prod-cluster" while the change landed on a different cluster.
func TestForRefusesToNameATargetItDidNotReach(t *testing.T) {
	cfg := configWith("prod-cluster", localconfig.KubernetesTarget("prod-cluster"))

	got := For(cfg, "dev-cluster", false)
	if got.Name != "" {
		t.Errorf("Name = %q for a command that connected to a different context; it must not claim the recorded target", got.Name)
	}
	if got.Kind != KindNone || got.Context != "dev-cluster" {
		t.Errorf("got %+v, want the context it actually reached", got)
	}
	if c := got.Clause(); !strings.Contains(c, `"dev-cluster"`) || !strings.Contains(c, "no target selected") {
		t.Errorf("Clause() = %q", c)
	}
}

// TestForDoesNotNameACloudTargetForAKubeConnection covers the same rule for the other target kind: a
// Burrow Cloud target has no kube context, so a kubeconfig connection cannot have reached it.
func TestForDoesNotNameACloudTargetForAKubeConnection(t *testing.T) {
	cfg := configWith(localconfig.CloudEndpoint, localconfig.CloudTarget())
	got := For(cfg, "dev-cluster", false)
	if got.Name != "" || got.Kind != KindNone {
		t.Errorf("got %+v, want the kube context named rather than the cloud target", got)
	}
}

// TestOverrideNamesWhatItWasOverriddenTo: --context is a per-invocation override (ADR-0078 §4), so
// the output names what the person overrode the target TO, not what they overrode.
func TestOverrideNamesWhatItWasOverriddenTo(t *testing.T) {
	cfg := configWith("prod-cluster", localconfig.KubernetesTarget("prod-cluster"))
	got := For(cfg, "staging", true)
	if got.Name != "" {
		t.Errorf("Name = %q, want no target name on an overridden invocation", got.Name)
	}
	if got.Context != "staging" || !got.Override {
		t.Errorf("got %+v, want the overridden-to context", got)
	}
	if c := got.Clause(); c != `on kube context "staging" (--context override)` {
		t.Errorf("Clause() = %q", c)
	}
}

// TestNoTargetSelectedStaysTruthful: with nothing selected the CLI follows the ambient kubeconfig,
// which ADR-0078 §1 preserves. Saying so is better than inventing a name for it.
func TestNoTargetSelectedStaysTruthful(t *testing.T) {
	got := For(nil, "minikube", false)
	if got.Name != "" || got.Kind != KindNone || got.Context != "minikube" {
		t.Errorf("got %+v", got)
	}
	if c := got.Clause(); c != `on kube context "minikube" (no target selected)` {
		t.Errorf("Clause() = %q", c)
	}

	// And with no current context either, there is nothing to name and it says that rather than
	// printing an empty pair of quotes.
	bare := For(nil, "", false)
	if strings.Contains(bare.Clause(), `""`) {
		t.Errorf("Clause() = %q, want no empty quoted name", bare.Clause())
	}
}

// TestForControlPlane names the URL a --control-plane invocation addressed, since that flag bypasses
// the target model rather than selecting within it. A URL is not a credential; the token that goes
// with it is never rendered.
func TestForControlPlane(t *testing.T) {
	got := ForControlPlane("https://burrow.example.com")
	if got.Endpoint != "https://burrow.example.com" || got.Name != "" {
		t.Errorf("got %+v", got)
	}
	if c := got.Clause(); c != "on the control plane at https://burrow.example.com" {
		t.Errorf("Clause() = %q", c)
	}
}

// TestKindIsAlwaysPresentInJSON: a reader of the JSON must never have to tell "no target" apart from
// "this field was not written", so kind is always emitted.
func TestKindIsAlwaysPresentInJSON(t *testing.T) {
	for _, n := range []Named{For(nil, "dev", false), ForControlPlane("https://x"), For(configWith("p", localconfig.KubernetesTarget("p")), "p", false)} {
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := decoded["kind"]; !ok {
			t.Errorf("%s carries no kind", raw)
		}
		if _, ok := decoded["detail"]; !ok {
			t.Errorf("%s carries no detail", raw)
		}
	}
}
