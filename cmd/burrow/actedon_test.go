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
// is the path the target model actually decides. --control-plane would bypass it. It returns both
// streams: a per-app command names its target on stderr, ahead of the work, and the result on
// stdout stays clean for `--json`.
func deployAgainst(t *testing.T, kubeconfig string, extra ...string) (stdout, stderr string) {
	t.Helper()
	args := append([]string{"app", "deploy", "web", "--image", "img:1", "--kubeconfig", kubeconfig}, extra...)
	var out, errb bytes.Buffer
	if err := run(context.Background(), args, &out, &errb); err != nil {
		t.Fatalf("deploy: %v\nstderr: %s", err, errb.String())
	}
	return out.String(), errb.String()
}

// TestDeployNamesTheSelectedTarget is ADR-0078 §4 end to end: the change says where it landed, using
// the name the person chose in the picker — ONCE, on the targeting line the per-app path prints
// ahead of the work (ADR-0036, #460).
//
// It used to say it twice: that line, and then a clause appended to the result naming the kube
// context behind the target. Two answers to one question, in two vocabularies, is worse than either
// alone — a reader has to work out whether they disagree, and in issue #473 they did.
func TestDeployNamesTheSelectedTarget(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))
	selectTarget(t, "prod")

	out, errb := deployAgainst(t, kubeconfig)
	if !strings.HasPrefix(errb, "targeting ") || !strings.Contains(errb, "prod") {
		t.Errorf("deploy did not name the active target.\nstderr: %q", errb)
	}
	if strings.Contains(out, "prod") || strings.Contains(out, "kube context") {
		t.Errorf("the result answered the same question a second time.\nstdout: %q\nstderr: %q", out, errb)
	}
	if !strings.Contains(out, "deployed web") {
		t.Errorf("the result no longer says what it did.\nstdout: %q", out)
	}
}

// TestDeployWithNoTargetNamesTheKubeContext covers the case the record deliberately preserves: with
// nothing selected the CLI follows the ambient kubeconfig (ADR-0078 §1). It says exactly that rather
// than inventing a target name for a target that does not exist — and says it once, on the targeting
// line, without the trailing "(no target selected)" that used to ride every changed thing.
func TestDeployWithNoTargetNamesTheKubeContext(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))

	out, errb := deployAgainst(t, kubeconfig)
	if !strings.Contains(errb, `kube context "staging"`) {
		t.Errorf("with no target selected the output should name the kube context it followed.\nstderr: %q", errb)
	}
	if strings.Contains(out+errb, "no target selected") {
		t.Errorf("a command that changed something told the reader what they have not done.\nstdout: %q\nstderr: %q", out, errb)
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

	out, errb := deployAgainst(t, kubeconfig, "--context", "prod")
	if !strings.Contains(errb, `targeting context "prod"`) {
		t.Errorf("an overridden invocation should name what it was overridden to.\nstderr: %q", errb)
	}
	if strings.Contains(out+errb, "staging") {
		t.Errorf("the overridden-away target must not be named as the place the change landed.\nstdout: %q\nstderr: %q", out, errb)
	}
}

// TestPrivilegedCommandNamesWhatItReached covers the other resolution path, and the reason the
// clause survives at all. `guard set` prints no targeting line — the privileged commands never have
// — so for them the clause is the ONLY thing that answers where the change landed, and it is always
// appended. It names the picker name when the context it reached is the selected target's.
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
	if !strings.Contains(out.String(), "on staging") {
		t.Errorf("guard set did not name the target it wrote to.\ngot: %q", out.String())
	}
}

// saveHandle records one environment handle for a kube context and pins it when asked, the way
// `burrow cluster install` and `burrow env list --discover` leave a machine that has never run
// `burrow auth login`. It writes to the $BURROW_CONFIG tempConfig set.
func saveHandle(t *testing.T, name, kubeContext string, pin bool) {
	t.Helper()
	cfg := &localconfig.Config{Environments: []localconfig.Environment{{Name: name, Context: kubeContext}}}
	if pin {
		cfg.Current = name
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// TestChangeOnTheAppPathSaysWhereOnce is the line from issues #465 and #473, end to end:
//
//	targeting prod
//	set KEY on web on kube context "do-nyc1-burrow-test-e2e" (no target selected)
//
// Four things wrong with one line, and all four are the same mistake — answering a question that was
// already answered, in the vocabulary the answer had deliberately stopped using. The targeting line
// is the answer; the result says what it did.
func TestChangeOnTheAppPathSaysWhereOnce(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))
	saveHandle(t, "prod", "staging", true)

	var out, errb bytes.Buffer
	if err := run(context.Background(),
		[]string{"app", "config", "set", "web", "KEY=value", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
		t.Fatalf("app config set: %v\nstderr: %s", err, errb.String())
	}
	if !strings.Contains(errb.String(), "targeting prod") {
		t.Errorf("the targeting line did not name the environment.\nstderr: %q", errb.String())
	}
	for _, unwanted := range []string{"kube context", "no target selected", "staging"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("the result repeated where it went, saying %q.\nstdout: %q", unwanted, out.String())
		}
	}
	if !strings.Contains(out.String(), "set KEY on web") {
		t.Errorf("the result no longer says what it did.\nstdout: %q", out.String())
	}
}

// TestPrivilegedCommandNamesTheRegisteredHandle covers the privileged path for the person who has
// never run `burrow auth login`. Their cluster still has a name — the handle install registered for
// it — and that is what the clause says. It used to reach past the name for the kube context and
// then explain that no target was selected, which is Burrow's own resolution detail followed by a
// remark about something the reader had not been asked to do (issue #465).
func TestPrivilegedCommandNamesTheRegisteredHandle(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)

	var hit bool
	cluster := fakeBurrowdCluster(&hit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))
	saveHandle(t, "prod", "staging", false)

	var out, errb bytes.Buffer
	if err := run(context.Background(),
		[]string{"guard", "set", "app.deploy", "allow", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
		t.Fatalf("guard set: %v\nstderr: %s", err, errb.String())
	}
	if !strings.Contains(out.String(), "on prod") {
		t.Errorf("guard set did not name the cluster the way the rest of the CLI does.\ngot: %q", out.String())
	}
	for _, unwanted := range []string{"kube context", "no target selected"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("guard set said %q.\ngot: %q", unwanted, out.String())
		}
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

	out, _ := deployAgainst(t, kubeconfig, "--json")
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
