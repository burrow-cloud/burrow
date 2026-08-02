// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/localconfig"
)

// TestAddonInstallStagesRBACOnTheClusterItRegistersOn is the inconsistency ADR-0084 §4 names, and
// it is the sharpest one because a single command produced it. `addon install` stages the add-on's
// RBAC with the kubeconfig and then registers the add-on over burrowd's API; the two used to
// resolve the cluster differently, so on a machine where the answers differed one operation wrote
// to two clusters. They now resolve it the same way, so the applied context and the API's context
// are the target's.
func TestAddonInstallStagesRBACOnTheClusterItRegistersOn(t *testing.T) {
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	forbidCloud(t)

	var stagingHit, prodHit bool
	staging := fakeGuardCluster(&stagingHit)
	prod := fakeGuardCluster(&prodHit)
	defer staging.Close()
	defer prod.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(staging.URL, prod.URL))
	selectTarget(t, "prod")

	// The applied context is captured rather than applied for real: the assertion is about WHICH
	// cluster the grant would land on, and a fake keeps the test off any cluster at all.
	var appliedContext string
	var applied bool
	orig := applyFn
	applyFn = func(_ context.Context, _, kubeContext, _ string, _ bool, _, _ io.Writer) error {
		appliedContext, applied = kubeContext, true
		return nil
	}
	defer func() { applyFn = orig }()

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"addon", "install", "metrics", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
		t.Fatalf("addon install metrics: %v\nstderr: %s", err, errb.String())
	}
	if !applied {
		t.Fatal("no RBAC was staged for the metrics add-on")
	}
	if appliedContext != "prod" {
		t.Errorf("RBAC was applied to context %q, want the target's context prod", appliedContext)
	}
	if !prodHit {
		t.Error("the install API call did not reach the target's cluster")
	}
	if stagingHit {
		t.Error("the install API call reached the kubeconfig's current context while a target was selected")
	}
}

// TestEnvAddKeepsItsThreeStepsOnOneCluster. `env add` applies a namespace and RBAC with the
// kubeconfig, registers the environment over the API, and records a local handle naming the
// context — three chances to land on three clusters. All three follow the selected target.
func TestEnvAddKeepsItsThreeStepsOnOneCluster(t *testing.T) {
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	forbidCloud(t)

	var stagingHit, prodHit bool
	staging := fakeGuardCluster(&stagingHit)
	prod := fakeGuardCluster(&prodHit)
	defer staging.Close()
	defer prod.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(staging.URL, prod.URL))
	selectTarget(t, "prod")

	var appliedContext string
	orig := applyFn
	applyFn = func(_ context.Context, _, kubeContext, _ string, _ bool, _, _ io.Writer) error {
		appliedContext = kubeContext
		return nil
	}
	defer func() { applyFn = orig }()

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "add", "staging-env", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
		t.Fatalf("env add: %v\nstderr: %s", err, errb.String())
	}
	if appliedContext != "prod" {
		t.Errorf("namespace and RBAC were applied to context %q, want the target's context prod", appliedContext)
	}
	if !prodHit || stagingHit {
		t.Errorf("the registration reached staging=%v prod=%v, want the target's cluster only", stagingHit, prodHit)
	}

	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	env, ok := cfg.Lookup("staging-env")
	if !ok {
		t.Fatal("env add recorded no local handle")
	}
	if env.Context != "prod" {
		t.Errorf("recorded handle names context %q, want the context the other two steps used", env.Context)
	}
}

// TestLifecycleCommandsTakeAContextFlag. `cluster upgrade`, the three `cluster ...  install`
// provisioners and `config registry` had no --context at all, so there was no per-invocation way to
// point them at anything but the kubeconfig's current context. `cluster upgrade` was the sharp one:
// it always upgraded whatever the kubeconfig happened to point at.
func TestLifecycleCommandsTakeAContextFlag(t *testing.T) {
	for _, args := range [][]string{
		{"cluster", "upgrade"},
		{"cluster", "ingress", "install"},
		{"cluster", "postgres", "install"},
		{"cluster", "registry", "install"},
		{"config", "registry", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errb bytes.Buffer
			if err := run(context.Background(), append(args, "--help"), &out, &errb); err != nil {
				t.Fatalf("--help: %v", err)
			}
			if !strings.Contains(out.String(), "--context") {
				t.Errorf("%s carries no --context flag:\n%s", strings.Join(args, " "), out.String())
			}
		})
	}
}

// TestLifecycleNoteNamesTheContextWhenTheTargetNamesAnother. The cluster-lifecycle commands
// deliberately do NOT follow the target (ADR-0078 §3): installing into the managed product is not a
// thing that can be asked for, and an installer that redirected itself to whatever was last
// selected would be the more dangerous behaviour. What they must not do is stay silent about it.
func TestLifecycleNoteNamesTheContextWhenTheTargetNamesAnother(t *testing.T) {
	tempConfig(t)
	kubeconfig := writeKubeconfig(t, twoContextConfig("https://staging.invalid", "https://prod.invalid"))
	selectTarget(t, "prod") // the kubeconfig's current context is staging

	var errb bytes.Buffer
	noteLifecycleContext(kubeconfig, "", &errb)
	for _, want := range []string{`"staging"`, `"prod"`, "pass --context"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("note = %q, want it to mention %q", errb.String(), want)
		}
	}

	// Naming the target's own context is agreement, not divergence, so there is nothing to say.
	errb.Reset()
	noteLifecycleContext(kubeconfig, "prod", &errb)
	if errb.Len() != 0 {
		t.Errorf("note = %q, want silence when the context is the target's own", errb.String())
	}
}

// TestLifecycleNoteIsSilentWithNoTargetSelected. The pre-ADR-0078 world is untouched, and that
// includes its output: someone who has never run `burrow auth login` sees exactly what they saw.
func TestLifecycleNoteIsSilentWithNoTargetSelected(t *testing.T) {
	tempConfig(t)
	kubeconfig := writeKubeconfig(t, twoContextConfig("https://staging.invalid", "https://prod.invalid"))

	var errb bytes.Buffer
	noteLifecycleContext(kubeconfig, "", &errb)
	if errb.Len() != 0 {
		t.Errorf("note = %q, want silence with no target selected", errb.String())
	}
}

// TestLifecycleNoteNamesTheManagedProduct. With the managed product selected there is no cluster the
// target could name, and the lifecycle commands are exempt from the refusal the rest of the
// privileged set gets (ADR-0078 §3). So they run, on a kubeconfig context, and say which.
func TestLifecycleNoteNamesTheManagedProduct(t *testing.T) {
	tempConfig(t)
	kubeconfig := writeKubeconfig(t, twoContextConfig("https://staging.invalid", "https://prod.invalid"))
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfg.SetTarget(localconfig.CloudTarget()); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var errb bytes.Buffer
	noteLifecycleContext(kubeconfig, "", &errb)
	for _, want := range []string{`"staging"`, localconfig.CloudEndpoint, "no cluster of its own"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("note = %q, want it to mention %q", errb.String(), want)
		}
	}
}

// TestControlPlaneFlagResolvesNoTarget. --control-plane names the control plane outright and opens
// no kubeconfig, so a target whose context has been renamed away must not break it — that would turn
// a stale local file into a failure for a scripted or CI invocation that has no kubeconfig at all.
func TestControlPlaneFlagResolvesNoTarget(t *testing.T) {
	tempConfig(t)
	selectTarget(t, "renamed-away")

	o := &commonOpts{controlPlane: "https://burrow.example.com", kubeconfig: "/nonexistent/kubeconfig"}
	var errb bytes.Buffer
	got, err := o.clusterContext(&errb)
	if err != nil {
		t.Fatalf("clusterContext with --control-plane: %v", err)
	}
	if got != "" {
		t.Errorf("context = %q, want none: --control-plane consults no target", got)
	}
	if errb.Len() != 0 {
		t.Errorf("note = %q, want silence", errb.String())
	}
}

// TestClusterContextIsResolvedOnce. A command that resolves the cluster more than once — `env add`
// applies, calls the API and records a handle — must get one answer and print any divergence note
// once, or the note becomes the noise it exists not to be.
func TestClusterContextIsResolvedOnce(t *testing.T) {
	tempConfig(t)
	kubeconfig := writeKubeconfig(t, twoContextConfig("https://staging.invalid", "https://prod.invalid"))
	selectTarget(t, "prod")

	o := &commonOpts{kubeconfig: kubeconfig, context: "staging"}
	var errb bytes.Buffer
	for i := range 3 {
		got, err := o.clusterContext(&errb)
		if err != nil {
			t.Fatalf("clusterContext (call %d): %v", i, err)
		}
		if got != "staging" {
			t.Fatalf("context = %q, want the explicit override", got)
		}
	}
	if n := strings.Count(errb.String(), "--context overrides it"); n != 1 {
		t.Errorf("the override note printed %d times, want once:\n%s", n, errb.String())
	}
}
