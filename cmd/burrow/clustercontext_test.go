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

// TestLifecycleCommandsTakeAContextFlag. `cluster upgrade` and the three `cluster ... install`
// provisioners had no --context at all, so there was no per-invocation way to point them at anything
// but the kubeconfig's current context. `config registry` is in the list because it had the same
// gap, though it is not a lifecycle command — it follows the target, and the flag is its override.
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

// TestUpgradeActsOnTheContextItIsGiven is the sharpest of the four lifecycle commands, so it is the
// one wired end to end rather than only asserted to carry a flag.
//
// Both halves matter. `--context prod` must reach prod, which is the capability that did not exist
// before. And with a target selected and no flag, it must still reach the kubeconfig's CURRENT
// context — because a lifecycle command deliberately does not follow the target (ADR-0078 §3), and a
// test that only pinned the flag would not notice that rule quietly reversing.
//
// Neither run gets far: both fake clusters answer 404, so the upgrade stops at "not installed". That
// is enough, and it is the point — the assertion is about WHICH cluster was asked, and stopping
// there keeps the test off any real one.
func TestUpgradeActsOnTheContextItIsGiven(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantProd   bool
		wantReason string
	}{
		{
			name:       "--context names the cluster",
			args:       []string{"--context", "prod"},
			wantProd:   true,
			wantReason: "the context named on the command line",
		},
		{
			name:       "a selected target does not redirect it",
			wantProd:   false,
			wantReason: "the kubeconfig's current context, which a lifecycle command keeps following",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempConfig(t)
			var stagingHit, prodHit bool
			staging := notFoundCluster(t, &stagingHit)
			prod := notFoundCluster(t, &prodHit)
			kubeconfig := writeKubeconfig(t, twoContextConfig(staging, prod))
			selectTarget(t, "prod")

			args := append([]string{"cluster", "upgrade", "--kubeconfig", kubeconfig, "--burrowd-image", "ghcr.io/example/burrowd:v0"}, tc.args...)
			var out, errb bytes.Buffer
			err := run(context.Background(), args, &out, &errb)
			if err == nil {
				t.Fatalf("upgrade against an empty cluster succeeded\nstdout: %s", out.String())
			}
			if !strings.Contains(err.Error(), "not installed") {
				t.Fatalf("error = %v, want the not-installed refusal that proves the cluster was read", err)
			}
			if prodHit != tc.wantProd || stagingHit == tc.wantProd {
				t.Errorf("reached staging=%v prod=%v, want %s", stagingHit, prodHit, tc.wantReason)
			}
		})
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
