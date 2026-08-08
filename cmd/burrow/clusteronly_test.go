// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// fakeGuardCluster is a fake API server standing in for one cluster on the privileged path: it
// serves the install token Secret and the guardrail listing, recording that it was reached.
func fakeGuardCluster(hit *bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hit = true
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/secrets/") {
			_ = json.NewEncoder(w).Encode(&corev1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: "burrow"},
				Data:       map[string][]byte{"token": []byte("s3cr3t")},
			})
			return
		}
		cannedGuardrails(w, r)
	}))
}

// unreachableCluster is a kubeconfig whose only context points at a server that fails the test if
// anything opens it. It is how "the command refused" is told apart from "the command ran and
// happened to fail": a refusal that arrives after the connection is not a refusal.
func unreachableCluster(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a cluster was contacted: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return writeKubeconfig(t, twoContextConfig(srv.URL, srv.URL))
}

// clusterOnlyCommands is every command that reaches a cluster and therefore cannot run against the
// managed product. It is derived from the code, not from prose: they are the callers of
// commonOpts.client — the privileged connection path — plus `config registry`, which writes a pull
// Secret with a kubeconfig clientset of its own. All of them now resolve WHICH cluster through the
// selected target (clustercontext.go); this list is about the target kind that has no cluster at all.
//
// `cluster install`, `cluster upgrade`, `cluster bootstrap`, `join` and the whole `cluster
// ingress` / `cluster registry` / `cluster postgres` provisioner surface are deliberately absent:
// they name the cluster they act on and stay kubeconfig-only by ADR-0078 §3.
//
// So are the reads that moved to commonOpts.readClient: they were on this list because they shared a
// connection path with the writes, not because they needed a cluster, and they now answer against
// either kind of target (cloudReadCommands below). What is left divides into two honest reasons, and
// both are still "not through here":
//
//   - It acts on a cluster with a kubeconfig. `config registry` writes a pull Secret;
//     `env add` creates a namespace and RBAC before it registers anything.
//   - It CHANGES the tenant, and whether a tenant may is a product decision rather than a plumbing
//     one: `guard set`, `cluster config set`, `addon` installs and attaches, `provider add`,
//     `domain add`/`remove`, `env add`/`remove`. The managed API answers several of these routes
//     with a refusal of its own; none of them is turned on by a client change.
//
// `cluster` and `cluster capacity` stay for a third reason: the route answers, but what it describes
// is the operator's cluster — its nodes, its capacity, its top consumers across every tenant — so
// what a tenant should see there is a decision nobody has taken.
var clusterOnlyCommands = [][]string{
	{"guard", "set", "app.delete", "deny"},
	{"cluster"},
	{"cluster", "capacity"},
	{"cluster", "config", "set", "replicas.max", "5"},
	{"addon", "list"},
	{"addon", "install"},
	{"addon", "install", "logs"},
	{"addon", "remove", "logs"},
	{"addon", "attach", "postgres", "web"},
	{"addon", "backups", "postgres"},
	{"app", "domain", "add", "example.com", "--address", "203.0.113.1"},
	{"app", "domain", "remove", "example.com"},
	{"config", "provider", "list"},
	// `provider add` refuses before it prompts for the token, which is why it can be driven here with
	// no stdin at all: a credential must not be asked for on behalf of a registration that has
	// nowhere to be written.
	{"config", "provider", "add", "cloudflare"},
	{"config", "registry", "list"},
	{"config", "registry", "login", "ghcr.io", "-u", "me", "-p", "tok"},
	{"config", "registry", "logout", "ghcr.io"},
	{"env", "add", "staging"},
}

// cloudReadCommands is the other half of the sweep: the reads that were refused on a cloud target
// because they shared a connection path with the writes, not because they needed a cluster. Each is
// a GET the managed product already answers, and each now goes there (cloud issue #202). The path is
// listed with the command so the test asserts WHERE it went and not merely that it did not fail —
// a read that quietly fell back to a kubeconfig would still print a table.
var cloudReadCommands = []struct {
	args []string
	path string
}{
	{[]string{"env", "list"}, "/v1/environments"},
	{[]string{"guard", "list"}, "/v1/guard"},
	{[]string{"cluster", "config", "list"}, "/v1/config"},
	{[]string{"audit"}, "/v1/audit"},
	{[]string{"failures"}, "/v1/failures"},
}

// cannedTenantReads answers every route in cloudReadCommands from one body. The keys are disjoint
// across the five decoders, so each command finds its own and ignores the rest — which keeps the
// fake a single handler rather than five that could drift apart.
func cannedTenantReads(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"environments": []map[string]any{{"name": "prod", "namespace": "t-qnu706xej92kkd-run", "default": true}},
		"guardrails":   []map[string]any{{"code": "app.deploy", "disposition": "allow", "description": "deploy a new release of an application"}},
		"limits":       []map[string]any{{"code": "replicas.max", "value": "5", "source": "default", "description": "the replica ceiling"}},
		"entries":      []map[string]any{},
		"failures":     []map[string]any{},
	})
}

// TestCloudTargetServesTheReadsItHasARouteFor is the fix. Each of these refused with a sentence
// about a tenant having no cluster of its own, against an endpoint that had been answering the whole
// time: the client was reaching for a kubeconfig instead of calling a route (cloud issue #202).
//
// No --kubeconfig is passed and no kubeconfig exists — ambienttest gives the package an empty home —
// so a command that still resolved through one could not reach anything, and the assertion that the
// tenant's endpoint was hit is the assertion that it did not try.
func TestCloudTargetServesTheReadsItHasARouteFor(t *testing.T) {
	for _, tc := range cloudReadCommands {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			signedInToCloud(t)

			var gotPath, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
				cannedTenantReads(w, r)
			}))
			defer srv.Close()
			pointCloudAt(t, srv.URL)

			var out, errb bytes.Buffer
			if err := run(context.Background(), tc.args, &out, &errb); err != nil {
				t.Fatalf("burrow %s against a cloud target: %v\nstderr: %s", strings.Join(tc.args, " "), err, errb.String())
			}
			if gotPath != tc.path {
				t.Errorf("reached %q, want the tenant's %q", gotPath, tc.path)
			}
			if gotAuth != "Bearer "+cloudCLIToken {
				t.Errorf("Authorization = %q, want the stored credential", gotAuth)
			}
		})
	}
}

// TestEnvListOnACloudTargetShowsTheTenantsEnvironments is the issue's own acceptance: the tenant has
// one environment, `prod`, and the listing must show it AS the default rather than refuse on the
// grounds that a tenant has none.
func TestEnvListOnACloudTargetShowsTheTenantsEnvironments(t *testing.T) {
	signedInToCloud(t)
	srv := httptest.NewServer(http.HandlerFunc(cannedTenantReads))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "list"}, &out, &errb); err != nil {
		t.Fatalf("burrow env list against a cloud target: %v\nstderr: %s", err, errb.String())
	}
	for _, want := range []string{"prod", "t-qnu706xej92kkd-run", "* default environment"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", out.String(), want)
		}
	}
	// The cluster listing's vocabulary must not leak in: there is no kube context behind a tenant's
	// environment and nothing is pinned or followed, so a column or a legend saying otherwise would
	// be describing a machine that is not there.
	for _, unwanted := range []string{"CONTEXT", "kube context", "pinned"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("stdout = %q, want it not to mention %q", out.String(), unwanted)
		}
	}
}

// TestEnvListJSONOnACloudTargetKeepsTheSharedKeys. A script reading the listing should not have to
// know which kind of target answered, so `environments`, `current` and `mode` mean the same thing on
// both sides. `context` is absent rather than empty, because a tenant is not reached through one.
func TestEnvListJSONOnACloudTargetKeepsTheSharedKeys(t *testing.T) {
	signedInToCloud(t)
	srv := httptest.NewServer(http.HandlerFunc(cannedTenantReads))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "list", "--json"}, &out, &errb); err != nil {
		t.Fatalf("burrow env list --json against a cloud target: %v\nstderr: %s", err, errb.String())
	}
	var got struct {
		Environments []struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Default   bool   `json:"default"`
		} `json:"environments"`
		Current string `json:"current"`
		Mode    string `json:"mode"`
		Target  string `json:"target"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(got.Environments) != 1 || got.Environments[0].Name != "prod" || !got.Environments[0].Default {
		t.Errorf("environments = %+v, want the tenant's default prod", got.Environments)
	}
	if got.Current != "prod" {
		t.Errorf("current = %q, want %q", got.Current, "prod")
	}
	if got.Target != localconfig.CloudEndpoint {
		t.Errorf("target = %q, want %q", got.Target, localconfig.CloudEndpoint)
	}
	if strings.Contains(out.String(), `"context"`) {
		t.Errorf("stdout = %q, want no context key: a tenant is not reached through one", out.String())
	}
}

// TestEnvListDiscoverOnACloudTargetRefusesForTheRightReason. --discover probes the clusters in a
// kubeconfig, and there is no kubeconfig in this path — so it is refused, and refused for what it
// actually is rather than for the tenant supposedly having no environments.
func TestEnvListDiscoverOnACloudTargetRefusesForTheRightReason(t *testing.T) {
	signedInToCloud(t)
	forbidCloud(t)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"env", "list", "--discover"}, &out, &errb)
	if err == nil {
		t.Fatalf("env list --discover ran against a cloud target\nstdout: %s", out.String())
	}
	for _, want := range []string{"--discover", localconfig.CloudEndpoint, "burrow auth switch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestEnvFollowOnACloudTargetSaysWhatItDid. Unpinning reads no cluster, but it named its result
// through the kubeconfig-only resolution and so failed AFTER saving: the pin was cleared and the
// command reported an error. It now succeeds and describes what actually changed.
func TestEnvFollowOnACloudTargetSaysWhatItDid(t *testing.T) {
	signedInToCloud(t)
	forbidCloud(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "follow"}, &out, &errb); err != nil {
		t.Fatalf("burrow env follow against a cloud target: %v\nstderr: %s", err, errb.String())
	}
	if !strings.Contains(out.String(), "Cleared the pinned environment") || !strings.Contains(out.String(), localconfig.CloudEndpoint) {
		t.Errorf("stdout = %q, want it to say the pin was cleared and name the active target", out.String())
	}
}

// TestCloudTargetRefusesTheClusterOnlyCommands is the change. With the managed product selected,
// none of these may quietly act on whatever `kubectl config current-context` points at — which is
// what they did, because they never look at the target at all (issue #429).
func TestCloudTargetRefusesTheClusterOnlyCommands(t *testing.T) {
	for _, args := range clusterOnlyCommands {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			signedInToCloud(t)
			forbidCloud(t)
			kubeconfig := unreachableCluster(t)

			var out, errb bytes.Buffer
			err := run(context.Background(), append(args, "--kubeconfig", kubeconfig), &out, &errb)
			if err == nil {
				t.Fatalf("burrow %s ran against a cloud target\nstdout: %s", strings.Join(args, " "), out.String())
			}
			// Named target, plain reason, and the one command that changes the answer.
			for _, want := range []string{localconfig.CloudEndpoint, "no cluster of its own", "burrow auth switch"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestCloudTargetRefusalNamesWhatItIs pins the wording once, so the message stays the same fact the
// kubeconfig-shaped flags already refuse with rather than drifting into a second phrasing.
func TestCloudTargetRefusalNamesWhatItIs(t *testing.T) {
	signedInToCloud(t)
	forbidCloud(t)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"guard", "set", "app.delete", "deny", "--kubeconfig", unreachableCluster(t)}, &out, &errb)
	if err == nil {
		t.Fatal("guard set ran against a cloud target")
	}
	want := `this command reaches a cluster, and the active target "burrow-cloud.dev" is the managed product ` +
		`at burrow-cloud.dev, which has no cluster of its own. Switch to a cluster target with ` +
		`"burrow auth switch <name>" (see "burrow auth status")`
	if err.Error() != want {
		t.Errorf("error =\n%q\nwant\n%q", err, want)
	}
}

// twoGuardClusters stands up a fake cluster per context and returns the kubeconfig naming both,
// with `staging` as the CURRENT context and `prod` as the other one. The two booleans record which
// was reached, which is the only thing these tests assert.
func twoGuardClusters(t *testing.T) (kubeconfig string, stagingHit, prodHit *bool) {
	t.Helper()
	stagingHit, prodHit = new(bool), new(bool)
	staging := fakeGuardCluster(stagingHit)
	prod := fakeGuardCluster(prodHit)
	t.Cleanup(staging.Close)
	t.Cleanup(prod.Close)
	return writeKubeconfig(t, twoContextConfig(staging.URL, prod.URL)), stagingHit, prodHit
}

// TestTheSelectedTargetDecidesTheClusterOnThePrivilegedPath is the change (ADR-0084 §4). The
// kubeconfig's current context is `staging` and the selected target names `prod`, so `prod` is what
// must be reached. Before this, the privileged path built its connection from the raw --context
// flag, which is empty unless typed, and fell through to `staging` — the person configuring a
// guardrail on the target they had selected was writing it to whatever `kubectx` had last set.
func TestTheSelectedTargetDecidesTheClusterOnThePrivilegedPath(t *testing.T) {
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	forbidCloud(t)
	kubeconfig, stagingHit, prodHit := twoGuardClusters(t)
	selectTarget(t, "prod")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"guard", "list", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
		t.Fatalf("burrow guard list: %v\nstderr: %s", err, errb.String())
	}
	if !*prodHit {
		t.Error("guard list did not reach the cluster the selected target names")
	}
	if *stagingHit {
		t.Error("guard list reached the kubeconfig's current context while a target was selected")
	}
	if !strings.Contains(out.String(), "app.deploy") {
		t.Errorf("stdout = %q, want the guardrail listing", out.String())
	}
}

// TestNoTargetSelectedStillFollowsTheKubeconfig is the compatibility half. A person who never runs
// `burrow auth login` is in the pre-ADR-0078 world, it is still the default, and nothing about it
// changes: the current kube context decides, exactly as it always did.
func TestNoTargetSelectedStillFollowsTheKubeconfig(t *testing.T) {
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	forbidCloud(t)
	kubeconfig, stagingHit, prodHit := twoGuardClusters(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"guard", "list", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
		t.Fatalf("burrow guard list: %v\nstderr: %s", err, errb.String())
	}
	if !*stagingHit {
		t.Error("guard list did not follow the kubeconfig's current context with no target selected")
	}
	if *prodHit {
		t.Error("guard list reached a cluster nothing had selected")
	}
	if !strings.Contains(out.String(), "app.deploy") {
		t.Errorf("stdout = %q, want the guardrail listing", out.String())
	}
}

// TestExplicitContextStillBeatsTheSelectedTarget. An explicit --context is a person being
// deliberate about one invocation, which is the opposite of the silent ambient fall-through this
// record removes, so it keeps winning (ADR-0078 §4). What it does NOT get to be is quiet: acting on
// a cluster the selected target does not name is said out loud, in the same breath.
func TestExplicitContextStillBeatsTheSelectedTarget(t *testing.T) {
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	forbidCloud(t)
	kubeconfig, stagingHit, prodHit := twoGuardClusters(t)
	selectTarget(t, "prod")

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"guard", "list", "--kubeconfig", kubeconfig, "--context", "staging"}, &out, &errb); err != nil {
		t.Fatalf("burrow guard list --context staging: %v\nstderr: %s", err, errb.String())
	}
	if !*stagingHit {
		t.Error("--context did not override the selected target")
	}
	if *prodHit {
		t.Error("guard list reached the selected target despite an explicit --context")
	}
	for _, want := range []string{`"staging"`, `"prod"`, "--context overrides it"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr = %q, want it to mention %q", errb.String(), want)
		}
	}
}

// TestAStaleTargetIsReportedRatherThanSilentlyIgnored. A target naming a context the kubeconfig no
// longer holds used to be invisible on this path — the command simply used the current context
// instead. Now it is the error the app commands already give, naming the target, the context, and
// the two commands that fix it.
func TestAStaleTargetIsReportedRatherThanSilentlyIgnored(t *testing.T) {
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	forbidCloud(t)
	kubeconfig, stagingHit, prodHit := twoGuardClusters(t)
	selectTarget(t, "renamed-away")

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"guard", "list", "--kubeconfig", kubeconfig}, &out, &errb)
	if err == nil {
		t.Fatalf("guard list ran with a stale target\nstdout: %s", out.String())
	}
	if *stagingHit || *prodHit {
		t.Error("a stale target fell through to a cluster instead of being reported")
	}
	for _, want := range []string{"renamed-away", "not in your kubeconfig", "burrow auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestInstallIsExemptFromTheClusterOnlyRefusal. Installing into Burrow Cloud is not a thing that can
// be asked for (ADR-0078 §3), so `cluster install` keeps acting on a kubeconfig context whatever is
// selected — including its no-argument form, which lists the contexts available to install into.
func TestInstallIsExemptFromTheClusterOnlyRefusal(t *testing.T) {
	signedInToCloud(t)
	forbidCloud(t)

	origContexts, origProbe := listContexts, probeContextFn
	listContexts = func(string) ([]connect.Context, error) {
		return []connect.Context{{Name: "staging", Cluster: "c-staging", Current: true}, {Name: "prod", Cluster: "c-prod"}}, nil
	}
	probeContextFn = func(context.Context, string, string, string) (string, error) { return "", nil }
	t.Cleanup(func() { listContexts, probeContextFn = origContexts, origProbe })

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "install", "--burrowd-image", "ghcr.io/example/burrowd:v0"}, &out, &errb); err != nil {
		t.Fatalf("cluster install with a cloud target selected: %v\nstderr: %s", err, errb.String())
	}
	if !strings.Contains(out.String(), "staging") || !strings.Contains(out.String(), "prod") {
		t.Errorf("stdout = %q, want the kubeconfig contexts available to install into", out.String())
	}
}

// TestAuthIsExemptFromTheClusterOnlyRefusal. `burrow auth` is how a person sees which target is
// active and leaves it. Catching it in a blanket refusal would strand someone with the managed
// product selected and no way out, so it reads and writes the local config and refuses nothing.
func TestAuthIsExemptFromTheClusterOnlyRefusal(t *testing.T) {
	signedInToCloud(t)
	forbidCloud(t)
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfg.SetTarget(localconfig.KubernetesTarget("prod")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := cfg.SwitchTarget(localconfig.CloudEndpoint); err != nil {
		t.Fatalf("SwitchTarget: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"auth", "status"}, &out, &errb); err != nil {
		t.Fatalf("burrow auth status with a cloud target selected: %v\nstderr: %s", err, errb.String())
	}
	if !strings.Contains(out.String(), localconfig.CloudEndpoint) {
		t.Errorf("stdout = %q, want the target listing", out.String())
	}

	out.Reset()
	errb.Reset()
	if err := run(context.Background(), []string{"auth", "switch", "prod"}, &out, &errb); err != nil {
		t.Fatalf("burrow auth switch out of a cloud target: %v\nstderr: %s", err, errb.String())
	}
}

// TestControlPlaneFlagIsExemptFromTheClusterOnlyRefusal. --control-plane names the control plane
// outright, so no target is consulted for it anywhere — the per-app path returns early on it for
// the same reason, and a refusal here would break the scripted and CI use it exists for.
func TestControlPlaneFlagIsExemptFromTheClusterOnlyRefusal(t *testing.T) {
	signedInToCloud(t)
	forbidCloud(t)

	out, _, err := runCLI(t, cannedGuardrails, "guard", "list")
	if err != nil {
		t.Fatalf("guard list --control-plane with a cloud target selected: %v", err)
	}
	if !strings.Contains(out, "app.deploy") {
		t.Errorf("stdout = %q, want the guardrail listing", out)
	}
}
