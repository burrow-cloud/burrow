// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/internal/targetname"
	"github.com/burrow-cloud/burrow/localconfig"
)

// selectTarget records a Kubernetes target for the given kube context and makes it active, the way
// `burrow auth login --context <name>` does. It writes to the $BURROW_CONFIG tempConfig set, never
// to a real ~/.burrow/config.
func selectTarget(t *testing.T, kubeContext string) {
	t.Helper()
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfg.SetTarget(localconfig.KubernetesTarget(kubeContext)); err != nil {
		t.Fatalf("set target: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// deployAgainst runs `burrow app deploy` against a fake cluster reached through a kubeconfig, which
// is the path the target model actually decides. --control-plane would bypass it.
func deployAgainst(t *testing.T, kubeconfig string, extra ...string) string {
	t.Helper()
	args := append([]string{"app", "deploy", "web", "--image", "img:1", "--kubeconfig", kubeconfig}, extra...)
	var out, errb bytes.Buffer
	if err := run(context.Background(), args, &out, &errb); err != nil {
		t.Fatalf("deploy: %v\nstderr: %s", err, errb.String())
	}
	return out.String()
}

// TestDeployNamesTheSelectedTarget is ADR-0078 §4 end to end: the change says where it landed, using
// the name the person chose in the picker, in the same breath as what it did.
func TestDeployNamesTheSelectedTarget(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))
	selectTarget(t, "prod")

	out := deployAgainst(t, kubeconfig)
	if !strings.Contains(out, `on target "prod"`) {
		t.Errorf("deploy did not name the active target.\ngot: %q", out)
	}
	// Close to the thing it did: the target rides on the line that says what happened, not on a
	// line of its own several lines away.
	head, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(head, "deployed web") || !strings.Contains(head, `on target "prod"`) {
		t.Errorf("the target is not on the line that says what happened.\nfirst line: %q", head)
	}
}

// TestDeployWithNoTargetNamesTheKubeContext covers the case the record deliberately preserves: with
// nothing selected the CLI follows the ambient kubeconfig (ADR-0078 §1). It says exactly that rather
// than inventing a target name for a target that does not exist.
func TestDeployWithNoTargetNamesTheKubeContext(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))

	out := deployAgainst(t, kubeconfig)
	if !strings.Contains(out, `on kube context "staging" (no target selected)`) {
		t.Errorf("with no target selected the output should name the kube context it followed.\ngot: %q", out)
	}
}

// TestDeployWithContextOverrideNamesWhatItWasOverriddenTo: --context stays a per-invocation override
// (ADR-0078 §4), and what it was overridden TO is what the change landed on, so that is what is
// named — even though a different target is recorded and active.
func TestDeployWithContextOverrideNamesWhatItWasOverriddenTo(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))
	selectTarget(t, "staging")

	out := deployAgainst(t, kubeconfig, "--context", "prod")
	if !strings.Contains(out, `on kube context "prod" (--context override)`) {
		t.Errorf("an overridden invocation should name what it was overridden to.\ngot: %q", out)
	}
	if strings.Contains(out, `on target "staging"`) {
		t.Errorf("the overridden-away target must not be named as the place the change landed.\ngot: %q", out)
	}
}

// TestPrivilegedCommandNamesWhatItReached covers the other resolution path. `guard set` connects
// with the raw kube context rather than resolving an environment handle, so it names the context it
// reached — and the picker name only when that context is the selected target's.
func TestPrivilegedCommandNamesWhatItReached(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))
	selectTarget(t, "staging")

	var out, errb bytes.Buffer
	err := run(context.Background(),
		[]string{"guard", "set", "app.deploy", "allow", "--kubeconfig", kubeconfig}, &out, &errb)
	if err != nil {
		t.Fatalf("guard set: %v\nstderr: %s", err, errb.String())
	}
	// The kubeconfig's current context is "staging", which IS the selected target, so the name the
	// person chose is what appears.
	if !strings.Contains(out.String(), `on target "staging"`) {
		t.Errorf("guard set did not name the target it wrote to.\ngot: %q", out.String())
	}
}

// TestJSONCarriesTheTargetAlongsideTheResult: an agent composing a result has to be able to say
// where it happened, so the target is a member of the JSON result and the result's own fields are
// untouched.
func TestJSONCarriesTheTargetAlongsideTheResult(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))
	selectTarget(t, "prod")

	out := deployAgainst(t, kubeconfig, "--json")
	var doc struct {
		Target  targetname.Named `json:"target"`
		Release struct {
			ID string `json:"id"`
		} `json:"release"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
	if doc.Target.Name != "prod" {
		t.Errorf("target = %+v, want the selected target", doc.Target)
	}
	if doc.Release.ID != "r1" {
		t.Errorf("the result's own fields must survive adding the target; release = %+v", doc.Release)
	}
}

// TestEmitJSONWithTargetPreservesTheResult pins the splice itself: a result that is already a JSON
// object keeps every field and its order, and one that is not an object is wrapped rather than
// silently dropped.
func TestEmitJSONWithTargetPreservesTheResult(t *testing.T) {
	n := targetname.ForControlPlane("https://cp.example")

	var buf bytes.Buffer
	if err := emitJSONWithTarget(&buf, json.RawMessage(`{"b":1,"a":2}`), n); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `"b": 1`) || !strings.Contains(got, `"a": 2`) {
		t.Errorf("result fields were lost: %s", got)
	}
	if strings.Index(got, `"b"`) > strings.Index(got, `"a"`) {
		t.Errorf("result field order changed, which a caller parsing the output may notice: %s", got)
	}
	if !strings.Contains(got, `"target"`) {
		t.Errorf("no target member: %s", got)
	}

	buf.Reset()
	if err := emitJSONWithTarget(&buf, []string{"one", "two"}, n); err != nil {
		t.Fatalf("emit list: %v", err)
	}
	var wrapped struct {
		Target targetname.Named `json:"target"`
		Result []string         `json:"result"`
	}
	if err := json.Unmarshal(buf.Bytes(), &wrapped); err != nil {
		t.Fatalf("decoding %q: %v", buf.String(), err)
	}
	if len(wrapped.Result) != 2 || wrapped.Target.Endpoint != "https://cp.example" {
		t.Errorf("got %+v", wrapped)
	}

	buf.Reset()
	if err := emitJSONWithTarget(&buf, map[string]string{}, n); err != nil {
		t.Fatalf("emit empty: %v", err)
	}
	if !strings.Contains(buf.String(), `"target"`) {
		t.Errorf("an empty result still names the target: %s", buf.String())
	}
}

// TestWithTargetClauseKeepsTheBlockBelowTheHeadline: a run's captured output and a deploy's
// dependency report follow the headline, and the target belongs to the headline.
func TestWithTargetClauseKeepsTheBlockBelowTheHeadline(t *testing.T) {
	n := targetname.For(nil, "dev", false)
	got := withTargetClause("ran command in web: exit code 0\noutput (combined stdout+stderr):\nhello", n)
	lines := strings.Split(got, "\n")
	if !strings.HasSuffix(lines[0], n.Clause()) {
		t.Errorf("first line = %q, want the target appended to it", lines[0])
	}
	if lines[2] != "hello" {
		t.Errorf("the block below the headline was disturbed: %q", got)
	}
}
