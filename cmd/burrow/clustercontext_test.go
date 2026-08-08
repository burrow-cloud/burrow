// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
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

// TestLifecycleCommandsTakeAContextFlag. `cluster upgrade` and the `cluster ... install` provisioners
// had no --context at all, so there was no per-invocation way to point them at anything but the
// kubeconfig's current context. `config registry` is in the list because it had the same gap, though
// it is not a lifecycle command — it follows the target, and the flag is its override. Every
// provisioner added since is expected here too: the flag is the surface, not one command's feature.
func TestLifecycleCommandsTakeAContextFlag(t *testing.T) {
	for _, args := range [][]string{
		{"cluster", "upgrade"},
		{"cluster", "ingress", "install"},
		{"cluster", "postgres", "install"},
		{"cluster", "registry", "install"},
		{"cluster", "metrics", "install"},
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

// TestUpgradeActsOnTheClusterItWasGiven is the sharpest of the lifecycle commands, so it is the one
// wired end to end rather than only asserted to carry a flag.
//
// Both halves matter, and both are cloud ADR-0038 §1's two arms. `--context prod` reaches prod, which
// is a person naming a cluster for one invocation. And a cluster target reaches the cluster IT names
// — not the kubeconfig's current context, which is what the command used to follow. The second is
// the reversal: a lifecycle command used to ignore the target on the grounds that installing follows
// the kubeconfig, and what that produced was an upgrade rolling forward whichever cluster `kubectl
// config use-context` was last run on.
//
// Neither run gets far: both fake clusters answer 404, so the upgrade stops at "not installed". That
// is enough, and it is the point — the assertion is about WHICH cluster was asked, and stopping
// there keeps the test off any real one.
func TestUpgradeActsOnTheClusterItWasGiven(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantReason string
	}{
		{
			name:       "--context names the cluster",
			args:       []string{"--context", "prod"},
			wantReason: "the context named on the command line",
		},
		{
			name:       "a cluster target names it",
			wantReason: "the cluster the active target names, not the kubeconfig's current context",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempConfig(t)
			var stagingHit, prodHit bool
			staging := notFoundCluster(t, &stagingHit)
			prod := notFoundCluster(t, &prodHit)
			kubeconfig := writeKubeconfig(t, twoContextConfig(staging, prod))
			selectTarget(t, "prod") // the kubeconfig's current context is staging

			args := append([]string{"cluster", "upgrade", "--kubeconfig", kubeconfig, "--burrowd-image", "ghcr.io/example/burrowd:v0"}, tc.args...)
			var out, errb bytes.Buffer
			err := run(context.Background(), args, &out, &errb)
			if err == nil {
				t.Fatalf("upgrade against an empty cluster succeeded\nstdout: %s", out.String())
			}
			if !strings.Contains(err.Error(), "not installed") {
				t.Fatalf("error = %v, want the not-installed refusal that proves the cluster was read", err)
			}
			if !prodHit || stagingHit {
				t.Errorf("reached staging=%v prod=%v, want %s", stagingHit, prodHit, tc.wantReason)
			}
		})
	}
}

// TestNoLifecycleCommandActsOnTheAmbientContext is the rule itself, and it is asserted over the SET
// rather than over one command on purpose. The defect cloud ADR-0038 §1 removes was never specific to
// `cluster upgrade`; it was the shape every kubeconfig-only command shared, and a test pinned to one
// of them would leave the next provisioner free to reintroduce it. The set is discovered by walking
// the command tree for the flag bindLifecycleContext stamps, so a command added later is covered the
// moment it joins.
//
// Every fake cluster in the kubeconfig records being reached, so the assertion is not only that the
// command failed but that it failed BEFORE touching anything — a refusal printed after the apply
// would pass a message-only check.
func TestNoLifecycleCommandActsOnTheAmbientContext(t *testing.T) {
	paths := lifecycleCommandPaths(t)
	// A walk that finds nothing would pass every assertion below without testing anything. The floor
	// is the set as it stands (`cluster upgrade`, its deprecated top-level alias, the four
	// provisioners, and the registry's status and uninstall).
	if len(paths) < 8 {
		t.Fatalf("the walk found %d lifecycle commands (%v), want at least the eight that exist", len(paths), paths)
	}
	for _, path := range paths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			tempConfig(t) // no target selected: nothing names a cluster
			var stagingHit, prodHit bool
			kubeconfig := writeKubeconfig(t, twoContextConfig(notFoundCluster(t, &stagingHit), notFoundCluster(t, &prodHit)))

			var out, errb bytes.Buffer
			err := run(context.Background(), append(append([]string{}, path...), "--kubeconfig", kubeconfig), &out, &errb)
			if err == nil {
				t.Fatalf("ran with nothing naming a cluster\nstdout: %s", out.String())
			}
			// The refusal names the cluster it would have acted on — the kubeconfig's current
			// context — and the flag that would name it, so the fix is a copy rather than a hunt.
			for _, want := range []string{"nothing names one", "no target is selected", `"staging"`, "--context staging"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %v\nwant it to mention %q", err, want)
				}
			}
			if stagingHit || prodHit {
				t.Errorf("reached staging=%v prod=%v; a lifecycle command must act on no cluster it was not given", stagingHit, prodHit)
			}
		})
	}
}

// lifecycleCommandPaths walks the command tree and returns the argument path of every runnable
// command carrying the `--context` flag bindLifecycleContext registers, local or inherited (the
// registry group binds it once, on the parent, for all three of its commands).
func lifecycleCommandPaths(t *testing.T) [][]string {
	t.Helper()
	var found [][]string
	var walk func(cmd *cobra.Command, path []string)
	walk = func(cmd *cobra.Command, path []string) {
		for _, sub := range cmd.Commands() {
			p := append(append([]string{}, path...), sub.Name())
			if sub.Runnable() && bindsLifecycleContext(sub) {
				found = append(found, p)
			}
			walk(sub, p)
		}
	}
	walk(newRootCmd(), nil)
	return found
}

// bindsLifecycleContext reports whether a command's `--context` is the lifecycle one. The annotation
// is what distinguishes it from the identically named flag the privileged commands bind, which is a
// per-invocation override of a target rather than the only way to name a cluster.
func bindsLifecycleContext(cmd *cobra.Command) bool {
	for _, flags := range []*pflag.FlagSet{cmd.Flags(), cmd.InheritedFlags()} {
		if f := flags.Lookup("context"); f != nil {
			if _, ok := f.Annotations[lifecycleContextFlag]; ok {
				return true
			}
		}
	}
	return false
}

// TestLifecycleContextFlagWinsWhateverTheTargetIs. Installing or upgrading a cluster while the
// managed product is selected stays legal — a person using both is an ordinary state, and refusing it
// outright would trade a real capability for the rule (cloud ADR-0038 §1). What the flag has to do is
// win from every starting point, including the two where nothing else can name a cluster at all.
func TestLifecycleContextFlagWinsWhateverTheTargetIs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{name: "no target is selected", setup: func(*testing.T) {}},
		{name: "the managed product is active", setup: selectCloudTarget},
		{name: "a cluster target names somewhere else", setup: func(t *testing.T) { selectTarget(t, "staging") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempConfig(t)
			var stagingHit, prodHit bool
			kubeconfig := writeKubeconfig(t, twoContextConfig(notFoundCluster(t, &stagingHit), notFoundCluster(t, &prodHit)))
			tc.setup(t)

			var out, errb bytes.Buffer
			err := run(context.Background(), []string{
				"cluster", "upgrade", "--kubeconfig", kubeconfig, "--context", "prod",
				"--burrowd-image", "ghcr.io/example/burrowd:v0",
			}, &out, &errb)
			if err == nil || !strings.Contains(err.Error(), "not installed") {
				t.Fatalf("error = %v, want the not-installed refusal that proves prod was read", err)
			}
			if !prodHit || stagingHit {
				t.Errorf("reached staging=%v prod=%v, want the context named on the command line", stagingHit, prodHit)
			}
		})
	}

	// The flag winning is not the flag being silent: sending a command somewhere the active target
	// does not name is said out loud, the same way it is on the privileged path (ADR-0078 §4).
	t.Run("the divergence is announced", func(t *testing.T) {
		tempConfig(t)
		var stagingHit, prodHit bool
		kubeconfig := writeKubeconfig(t, twoContextConfig(notFoundCluster(t, &stagingHit), notFoundCluster(t, &prodHit)))
		selectTarget(t, "staging")

		var out, errb bytes.Buffer
		_ = run(context.Background(), []string{
			"cluster", "upgrade", "--kubeconfig", kubeconfig, "--context", "prod",
			"--burrowd-image", "ghcr.io/example/burrowd:v0",
		}, &out, &errb)
		if !strings.Contains(errb.String(), "--context overrides it") {
			t.Errorf("stderr = %q, want it to say the flag sent this somewhere the target does not name", errb.String())
		}
	})
}

// TestLifecycleRefusalNamesTheManagedProduct. The managed product names no cluster, so with it
// selected nothing names one and the command refuses. The refusal has to carry both halves of the
// answer: WHY nothing named a cluster (the active target cannot), and which cluster it would have
// acted on under the old rule, which is the one the person almost certainly meant.
func TestLifecycleRefusalNamesTheManagedProduct(t *testing.T) {
	tempConfig(t)
	var stagingHit, prodHit bool
	kubeconfig := writeKubeconfig(t, twoContextConfig(notFoundCluster(t, &stagingHit), notFoundCluster(t, &prodHit)))
	selectCloudTarget(t)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"cluster", "upgrade", "--kubeconfig", kubeconfig, "--burrowd-image", "ghcr.io/example/burrowd:v0"}, &out, &errb)
	if err == nil {
		t.Fatalf("upgrade ran with the managed product selected\nstdout: %s", out.String())
	}
	for _, want := range []string{localconfig.CloudEndpoint, "no cluster of its own", `"staging"`, "--context staging", "burrow auth switch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v\nwant it to mention %q", err, want)
		}
	}
	if stagingHit || prodHit {
		t.Errorf("reached staging=%v prod=%v, want no cluster contacted at all", stagingHit, prodHit)
	}
}

// TestAMalformedConfigRefusesALifecycleCommand is the deliberate contrast with
// TestAMalformedConfigLeavesTheKubeconfigDeciding below. The privileged path tolerates a config it
// cannot parse and follows the kubeconfig, because it has a default to fall back to and refusing
// `guard list` over a file it has no use for would be a regression. A lifecycle command has no such
// default: the config is the only thing that could have named a cluster, so a file that will not load
// is precisely why nothing did, and falling back is the behaviour being removed.
func TestAMalformedConfigRefusesALifecycleCommand(t *testing.T) {
	path := tempConfig(t)
	if err := os.WriteFile(path, []byte("this: is: not: valid: yaml:\n\t- at all\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	kubeconfig := writeKubeconfig(t, twoContextConfig("https://staging.invalid", "https://prod.invalid"))

	if _, err := lifecycleContext(kubeconfig, "", io.Discard); err == nil {
		t.Fatal("lifecycleContext accepted a config it could not read")
	} else if !strings.Contains(err.Error(), "the local config could not be read") || !strings.Contains(err.Error(), "--context staging") {
		t.Errorf("refusal = %v, want it to name the unreadable config and the cluster it would have used", err)
	}

	// An explicit --context is unaffected: the config has nothing to say once a cluster is named, and
	// working through an unparseable ~/.burrow/config is a legitimate escape hatch.
	got, err := lifecycleContext(kubeconfig, "prod", io.Discard)
	if err != nil {
		t.Fatalf("lifecycleContext with --context over a malformed config: %v", err)
	}
	if got != "prod" {
		t.Errorf("context = %q, want the explicitly named cluster", got)
	}
}

// testCluster is the kube context the lifecycle-command tests in this package name. A lifecycle
// command acts on a cluster somebody named or refuses (cloud ADR-0038 §1), and naming one on the
// command line is how a person satisfies that, so it is how the tests do. The value is arbitrary:
// those tests substitute the clientset seam, so it is never resolved against a kubeconfig — and the
// tests that pass no context at all are asserting the refusal itself, or a --dry-run that renders
// without a cluster.
const testCluster = "test-cluster"

// selectCloudTarget makes the managed product the active target, the way `burrow auth login` does.
func selectCloudTarget(t *testing.T) {
	t.Helper()
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
}

// notFoundCluster is a fake API server that records being reached and answers every read with 404.
// It is enough to prove which cluster a command opened without letting it do anything.
func notFoundCluster(t *testing.T, hit *bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hit = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404,
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestNoTargetSelectedIsUnchangedOnTheThreePathsThatMoved is the compatibility test that matters
// most. `guard list` is covered elsewhere, but it only ever had one resolution; these three each had
// a SECOND one — the RBAC apply, the manifest apply, the pull-Secret clientset — and it is the
// second one that moved. Somebody who has never run `burrow auth login` must see none of it: both
// halves of each command still follow the kubeconfig's current context, and nothing announces a
// target that does not exist.
func TestNoTargetSelectedIsUnchangedOnTheThreePathsThatMoved(t *testing.T) {
	// addon install stages RBAC with the kubeconfig, then calls the API.
	t.Run("addon install", func(t *testing.T) {
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

		var appliedContext string
		orig := applyFn
		applyFn = func(_ context.Context, _, kubeContext, _ string, _ bool, _, _ io.Writer) error {
			appliedContext = kubeContext
			return nil
		}
		defer func() { applyFn = orig }()

		var out, errb bytes.Buffer
		if err := run(context.Background(), []string{"addon", "install", "metrics", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
			t.Fatalf("addon install: %v\nstderr: %s", err, errb.String())
		}
		if appliedContext != "" {
			t.Errorf("RBAC named context %q, want the empty value that means the kubeconfig's current context", appliedContext)
		}
		if !stagingHit || prodHit {
			t.Errorf("reached staging=%v prod=%v, want the current context only", stagingHit, prodHit)
		}
		if errb.Len() != 0 {
			t.Errorf("stderr = %q, want silence with no target selected", errb.String())
		}
	})

	// env add applies manifests, registers, then records a handle naming the context.
	t.Run("env add", func(t *testing.T) {
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
		if appliedContext != "" {
			t.Errorf("manifests named context %q, want the kubeconfig's current context", appliedContext)
		}
		if !stagingHit || prodHit {
			t.Errorf("reached staging=%v prod=%v, want the current context only", stagingHit, prodHit)
		}
		// The handle records the context it resolved to, which is the current one by name.
		cfg, err := localconfig.Load()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		env, ok := cfg.Lookup("staging-env")
		if !ok {
			t.Fatal("env add recorded no handle")
		}
		if env.Context != "staging" {
			t.Errorf("handle context = %q, want the kubeconfig's current context", env.Context)
		}
	})

	// config registry writes a pull Secret with a kubeconfig clientset, and its result line must not
	// have grown a target clause for somebody who has selected no target.
	t.Run("config registry login", func(t *testing.T) {
		tempConfig(t)
		forbidCloud(t)
		cs := nsWithDefaultSA("apps")

		var gotContext string
		orig := registryClientset
		registryClientset = func(_, kubeContext string) (kubernetes.Interface, error) {
			gotContext = kubeContext
			return cs, nil
		}
		defer func() { registryClientset = orig }()

		out := runRegistry(t, cs, "login", "ghcr.io", "-u", "alice", "-p", "tok123")
		if gotContext != "" {
			t.Errorf("clientset built for context %q, want the kubeconfig's current context", gotContext)
		}
		if out != "configured registry \"ghcr.io\" for your apps\n" {
			t.Errorf("output = %q, want the line unchanged for somebody with no target selected", out)
		}
	})
}

// TestAMalformedConfigLeavesTheKubeconfigDeciding. This path did not read ~/.burrow/config at all
// before, so a file that will not parse broke nothing for somebody who has never selected a target.
// Refusing `guard list` over it would be a regression, and a worse message than `burrow auth status`
// already gives. It falls back — and says it fell back, because a silent fallback is the thing this
// change exists to remove.
func TestAMalformedConfigLeavesTheKubeconfigDeciding(t *testing.T) {
	path := tempConfig(t)
	if err := os.WriteFile(path, []byte("this: is: not: valid: yaml:\n\t- at all\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// The config really is unreadable, so the test cannot pass by accident on a parseable one.
	if _, err := localconfig.Load(); err == nil {
		t.Fatal("the config under test parsed; it must not")
	}

	o := &commonOpts{kubeconfig: writeKubeconfig(t, twoContextConfig("https://staging.invalid", "https://prod.invalid"))}
	var errb bytes.Buffer
	got, err := o.clusterContext(&errb)
	if err != nil {
		t.Fatalf("clusterContext with a malformed config: %v", err)
	}
	if got != "" {
		t.Errorf("context = %q, want the kubeconfig's current context to decide", got)
	}
	if !strings.Contains(errb.String(), "following the kubeconfig instead") {
		t.Errorf("stderr = %q, want it to say the config could not be read", errb.String())
	}
}

// TestAddonNamespacesComeFromTheClusterTheGrantLandsOn. The context and the namespaces are two
// answers, and taking them from resolutions that disagree is the two-cluster bug in a quieter form:
// the right cluster with another cluster's namespaces. With no target selected and a pin naming a
// different cluster, the grant follows the kubeconfig and the pin's namespaces are left behind.
func TestAddonNamespacesComeFromTheClusterTheGrantLandsOn(t *testing.T) {
	tempConfig(t)
	kubeconfig := writeKubeconfig(t, twoContextConfig("https://staging.invalid", "https://prod.invalid"))

	// A handle for `prod`, pinned, while the kubeconfig's current context is `staging`.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Environments = []localconfig.Environment{{
		Name: "other", Context: "prod", ControlPlaneNamespace: "burrow-elsewhere", AppNamespace: "apps-elsewhere",
	}}
	cfg.Current = "other"
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	o := &commonOpts{kubeconfig: kubeconfig, namespace: connect.DefaultNamespace}
	kubeContext, controlPlaneNamespace, appNamespace, err := o.resolveAddonNamespaces(io.Discard)
	if err != nil {
		t.Fatalf("resolveAddonNamespaces: %v", err)
	}
	if kubeContext != "" {
		t.Errorf("context = %q, want the kubeconfig's current context with no target selected", kubeContext)
	}
	if controlPlaneNamespace == "burrow-elsewhere" || appNamespace == "apps-elsewhere" {
		t.Errorf("namespaces (%q, %q) came from a handle for another cluster", controlPlaneNamespace, appNamespace)
	}
	if controlPlaneNamespace != connect.DefaultNamespace || appNamespace != connect.DefaultAppNamespace {
		t.Errorf("namespaces = (%q, %q), want the defaults", controlPlaneNamespace, appNamespace)
	}

	// The contrast: a handle for the cluster the grant IS landing on is exactly what should be used.
	cfg.Environments[0].Context = "staging"
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	same := &commonOpts{kubeconfig: kubeconfig, namespace: connect.DefaultNamespace}
	_, controlPlaneNamespace, appNamespace, err = same.resolveAddonNamespaces(io.Discard)
	if err != nil {
		t.Fatalf("resolveAddonNamespaces: %v", err)
	}
	if controlPlaneNamespace != "burrow-elsewhere" || appNamespace != "apps-elsewhere" {
		t.Errorf("namespaces = (%q, %q), want the handle's, since it describes this cluster", controlPlaneNamespace, appNamespace)
	}
}

// TestPrivilegedPathCarriesTheTargetsInstallID. Before the target decided this path, a privileged
// command had no target to read and so sent no install id (ADR-0084 §5); against a cluster torn down
// and rebuilt under a reused context name — the case §5 is about, since provider CLIs generate
// deterministic names — `guard set` would have got a bare 401 from a cluster the caller did not know
// they had reached. Now the id rides along and the refusal can name the cause.
//
// The override half is the same rule resolveTarget applies on the per-app path: an explicit
// --context is a deliberate choice of a different cluster, so the target's id no longer describes
// what is on the other end. Carrying it would refuse the override on the grounds that it is an
// override.
//
// Asserted where the id is decided rather than on the wire. A wire assertion is not reliable here:
// client-go reuses transports across connects within one process, so a second connection in the same
// test binary can answer with the first one's headers. That reuse is nothing this path introduces —
// a CLI invocation connects once — but it makes the header a poor witness for a rule about
// resolution.
func TestPrivilegedPathCarriesTheTargetsInstallID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		context string
		want    string
	}{
		{name: "the target's id is sent", want: "install-abc"},
		{name: "an explicit --context drops it", context: "staging", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempConfig(t)
			kubeconfig := writeKubeconfig(t, twoContextConfig("https://staging.invalid", "https://prod.invalid"))
			selectTarget(t, "prod")
			cfg, err := localconfig.Load()
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if !cfg.SetInstallID("prod", "install-abc") {
				t.Fatal("SetInstallID recorded nothing for the selected target")
			}
			if err := cfg.Save(); err != nil {
				t.Fatalf("save config: %v", err)
			}

			o := &commonOpts{kubeconfig: kubeconfig, context: tc.context}
			if _, err := o.clusterContext(io.Discard); err != nil {
				t.Fatalf("clusterContext: %v", err)
			}
			if o.installID != tc.want {
				t.Errorf("installID = %q, want %q", o.installID, tc.want)
			}
		})
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
