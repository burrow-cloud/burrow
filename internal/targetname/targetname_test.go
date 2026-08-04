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
	// "on prod-cluster", not "on target \"prod-cluster\"": the same words the targeting line uses,
	// because a person reading two lines about one place should not have to reconcile two
	// vocabularies (issue #465).
	if c := got.Clause(); c != "on prod-cluster" {
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
	if c := got.Clause(); c != `on kube context "dev-cluster"` {
		t.Errorf("Clause() = %q, want the context it actually reached", c)
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
// which ADR-0078 §1 preserves. Naming the context it followed is better than inventing a name for
// it — and better than the trailing "(no target selected)" this used to carry, which reported the
// absence of a thing nobody had asked the reader to do, on every command that changed anything.
// `burrow auth status` says it once, to somebody who asked.
func TestNoTargetSelectedStaysTruthful(t *testing.T) {
	got := For(nil, "minikube", false)
	if got.Name != "" || got.Kind != KindNone || got.Context != "minikube" {
		t.Errorf("got %+v", got)
	}
	if c := got.Clause(); c != `on kube context "minikube"` {
		t.Errorf("Clause() = %q", c)
	}
	if got.Decided() {
		t.Error("Decided() is true with nothing selected and no handle registered; a line that never carried a target should not gain one here")
	}

	// And with no current context either, there is nothing to name and it says that rather than
	// printing an empty pair of quotes.
	bare := For(nil, "", false)
	if strings.Contains(bare.Clause(), `""`) {
		t.Errorf("Clause() = %q, want no empty quoted name", bare.Clause())
	}
}

// TestForNamesTheRegisteredHandle covers the person who has never run `burrow auth login` and has an
// environment registered anyway — which is everybody who installed Burrow before targets existed,
// and everybody who ran `burrow env list --discover`. Their cluster HAS a name in Burrow; a
// privileged command that reached for Kubernetes vocabulary instead was answering with the
// mechanism rather than the thing (issue #465).
func TestForNamesTheRegisteredHandle(t *testing.T) {
	cfg := &localconfig.Config{Environments: []localconfig.Environment{
		{Name: "prod", Context: "do-nyc1-burrow-cloud"},
	}}

	got := For(cfg, "do-nyc1-burrow-cloud", false)
	if c := got.Clause(); c != "on prod" {
		t.Errorf("Clause() = %q, want the handle's name", c)
	}
	if !got.Decided() {
		t.Error("Decided() is false though a registered handle names the cluster that was reached")
	}
	// The handle is a display name, not a target: the JSON `target` member answers what a TARGET
	// decided, and agents parse it. It must be exactly what it was.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "prod") {
		t.Errorf("the handle name leaked into the JSON target: %s", raw)
	}

	// And it is bound by the same rule as a target: a handle for a DIFFERENT context says nothing
	// about the cluster this command reached.
	elsewhere := For(cfg, "some-other-context", false)
	if c := elsewhere.Clause(); c != `on kube context "some-other-context"` {
		t.Errorf("Clause() = %q, want the context reached rather than an unrelated handle", c)
	}

	// An override names what the person typed. They chose a cluster for this one command, and the
	// line reflects that choice back rather than translating it.
	overridden := For(cfg, "do-nyc1-burrow-cloud", true)
	if c := overridden.Clause(); c != `on kube context "do-nyc1-burrow-cloud" (--context override)` {
		t.Errorf("Clause() = %q, want the overridden-to context", c)
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
