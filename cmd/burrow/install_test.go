// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// stubInstall replaces install's cluster-touching seams with fakes for a real (non-dry-run) install
// path: a fixed set of kubeconfig contexts, a fake clientset (empty cluster: not already installed),
// and an apply that records the kube context it targeted. It points $BURROW_CONFIG at a temp file
// so the recorded environment handle is asserted without touching the user's real config. It
// returns a pointer to the captured target context. All seams are restored on cleanup.
func stubInstall(t *testing.T, contexts []connect.Context, contextsErr error) *string {
	t.Helper()
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))

	origList := listContexts
	listContexts = func(string) ([]connect.Context, error) { return contexts, contextsErr }

	origCS := clientsetFn
	clientsetFn = func(string, string) (kubernetes.Interface, error) { return fake.NewSimpleClientset(), nil }

	var targeted string
	origApply := applyFn
	applyFn = func(_ context.Context, _ string, kubeContext string, _ string, _ bool, _, _ io.Writer) error {
		targeted = kubeContext
		return nil
	}

	// The scoped-agent mint (ADR-0038) is faked here: it needs a real REST config and a
	// token-controller-populated Secret, which the fake clientset has not got. mintAgentKubeconfig /
	// writeAgentKubeconfig are exercised directly in their own tests; here we only record a fixed path.
	origMint := mintAgentCredentialFn
	mintAgentCredentialFn = func(_ context.Context, _ installArgs, envName string, _ kubernetes.Interface, _ io.Writer) (string, string, error) {
		return filepath.Join(t.TempDir(), "agents", envName), agentKubeContextName, nil
	}

	t.Cleanup(func() {
		listContexts = origList
		clientsetFn = origCS
		applyFn = origApply
		mintAgentCredentialFn = origMint
	})
	return &targeted
}

func twoContexts() []connect.Context {
	return []connect.Context{
		{Name: "dev", Cluster: "dev-cluster", Current: true},
		{Name: "prod", Cluster: "prod-cluster"},
	}
}

func TestInstallRecordsAndPinsEnvironment(t *testing.T) {
	targeted := stubInstall(t, twoContexts(), nil)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "prod", "--environment", "my-prod", "--burrowd-image", "img:1", "--wait=false"}, &out, &errb)
	if err != nil {
		t.Fatalf("install prod: %v\n%s", err, errb.String())
	}

	// The apply targeted exactly the named context (never the current/dev one).
	if *targeted != "prod" {
		t.Errorf("apply targeted context %q, want prod", *targeted)
	}

	// The environment is recorded and pinned as current.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if cfg.Current != "my-prod" {
		t.Errorf("current environment = %q, want my-prod (the new handle must be pinned)", cfg.Current)
	}
	env, ok := cfg.Lookup("my-prod")
	if !ok {
		t.Fatalf("environment my-prod was not recorded: %+v", cfg.Environments)
	}
	if env.Context != "prod" {
		t.Errorf("recorded context = %q, want prod", env.Context)
	}
	if env.ControlPlaneNamespace != connect.DefaultNamespace || env.AppNamespace != connect.DefaultAppNamespace {
		t.Errorf("recorded namespaces = (%q,%q), want (%q,%q)", env.ControlPlaneNamespace, env.AppNamespace, connect.DefaultNamespace, connect.DefaultAppNamespace)
	}

	// The confirmation names the environment and points at rename.
	if !strings.Contains(out.String(), `Environment "my-prod" is now your current environment`) {
		t.Errorf("missing the environment confirmation:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "burrow env rename my-prod") {
		t.Errorf("missing the rename hint:\n%s", out.String())
	}
}

// stubInstallJoin sets up install's seams for the JOIN path (ADR-0038 §4): a fixed context set, a
// fake clientset that already holds the burrowd-api-token Secret (so alreadyInstalled reports true),
// an applyFn that flips a flag if it is ever called (join must NOT apply), and a stubbed
// joinAgentCredentialFn returning joinErr or a fixed local path. $BURROW_CONFIG points at a temp
// file. It returns pointers to the apply flag and the recorded join write-name.
func stubInstallJoin(t *testing.T, joinErr error) (applied *bool, joinName *string) {
	t.Helper()
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))

	origList := listContexts
	listContexts = func(string) ([]connect.Context, error) { return twoContexts(), nil }

	origCS := clientsetFn
	clientsetFn = func(string, string) (kubernetes.Interface, error) {
		return fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: connect.DefaultTokenSecret, Namespace: connect.DefaultNamespace},
			Data:       map[string][]byte{connect.DefaultTokenKey: []byte("existing-token")},
		}), nil
	}

	var appliedFlag bool
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error {
		appliedFlag = true
		return nil
	}

	var name string
	origJoin := joinAgentCredentialFn
	joinAgentCredentialFn = func(_ context.Context, _, _, _, envName string) (string, string, error) {
		name = envName
		if joinErr != nil {
			return "", "", joinErr
		}
		return filepath.Join(t.TempDir(), "agents", envName), agentKubeContextName, nil
	}

	t.Cleanup(func() {
		listContexts = origList
		clientsetFn = origCS
		applyFn = origApply
		joinAgentCredentialFn = origJoin
	})
	return &appliedFlag, &name
}

// TestInstallJoinsExistingInstall covers a second user (or a re-run) installing into an
// already-installed cluster: install does NOT re-apply the manifests, it joins locally — reading the
// existing scoped agent credential and recording the handle with it — and prints the distinct
// "joined" message (ADR-0038 §4).
func TestInstallJoinsExistingInstall(t *testing.T) {
	applied, joinName := stubInstallJoin(t, nil)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"install", "prod", "--environment", "shared-prod", "--burrowd-image", "img:1", "--wait=false"}, &out, &errb); err != nil {
		t.Fatalf("install join: %v\n%s", err, errb.String())
	}

	// Join makes no cluster changes: apply must never run.
	if *applied {
		t.Error("install into an already-installed cluster must not apply manifests (join is local-only)")
	}
	if *joinName != "shared-prod" {
		t.Errorf("join wrote under %q, want the environment name shared-prod", *joinName)
	}

	// The handle is recorded and pinned, carrying the scoped credential from the join.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if cfg.Current != "shared-prod" {
		t.Errorf("current environment = %q, want shared-prod", cfg.Current)
	}
	env, ok := cfg.Lookup("shared-prod")
	if !ok {
		t.Fatalf("joined environment was not recorded: %+v", cfg.Environments)
	}
	if env.Context != "prod" || env.AgentKubeconfig == "" || env.AgentContext != agentKubeContextName {
		t.Errorf("joined handle = %+v, want context prod with a scoped agent credential", env)
	}

	// The message clearly says JOINED and no cluster changes, distinct from a fresh "Installed."
	s := out.String()
	for _, want := range []string{"Joined the existing Burrow install", "no cluster changes were made", `Environment "shared-prod" is now your current environment`} {
		if !strings.Contains(s, want) {
			t.Errorf("join output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\nInstalled. Environment") {
		t.Errorf("join must not print the fresh-install confirmation:\n%s", s)
	}
}

// TestInstallJoinUnreadableCredentialErrors covers a joining user who cannot read the agent token
// Secret: the join surfaces the actionable error rather than silently recording a handle (ADR-0038 §4).
func TestInstallJoinUnreadableCredentialErrors(t *testing.T) {
	applied, _ := stubInstallJoin(t, errors.New("cannot read the scoped agent credential: needs read access; ask an operator"))

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "prod", "--burrowd-image", "img:1", "--wait=false"}, &out, &errb)
	if err == nil {
		t.Fatal("join with an unreadable agent credential should error")
	}
	if !strings.Contains(err.Error(), "read access") {
		t.Errorf("join error should surface the actionable read-access message, got: %v", err)
	}
	if *applied {
		t.Error("a failed join must still not have applied manifests")
	}
	// Nothing is recorded when the join fails.
	cfg, err2 := localconfig.Load()
	if err2 != nil {
		t.Fatalf("loading config: %v", err2)
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a failed join should record no handle, got %+v", cfg.Environments)
	}
}

func TestInstallGeneratesEnvironmentName(t *testing.T) {
	stubInstall(t, twoContexts(), nil)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "dev", "--burrowd-image", "img:1", "--wait=false"}, &out, &errb)
	if err != nil {
		t.Fatalf("install dev: %v\n%s", err, errb.String())
	}

	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	// Omitting --environment generates an adjective-animal name, recorded and pinned.
	if cfg.Current == "" {
		t.Fatal("no environment was pinned")
	}
	if !strings.Contains(cfg.Current, "-") {
		t.Errorf("generated name %q is not adjective-animal", cfg.Current)
	}
	if _, ok := cfg.Lookup(cfg.Current); !ok {
		t.Errorf("pinned environment %q is not in the config", cfg.Current)
	}
}

func TestInstallNoArgListsContextsAndDoesNotInstall(t *testing.T) {
	contexts := []connect.Context{
		{Name: "dev", Cluster: "dev-cluster", Current: true},
		{Name: "prod", Cluster: "prod-cluster"},
		{Name: "broken", Cluster: "broken-cluster"},
	}
	targeted := stubInstall(t, contexts, nil)
	// Probe outcomes spanning all three install statuses, so the BURROWD column is exercised end
	// to end without a real cluster.
	stubProbe(t, func(kubeContext string) (string, error) {
		switch kubeContext {
		case "dev":
			return "ghcr.io/burrow-cloud/burrowd:v0.7.0", nil
		case "prod":
			return "", notFoundErr()
		default: // broken: an unreachable cluster
			return "", &net.DNSError{Err: "no such host", Name: "broken.invalid", IsNotFound: true}
		}
	})

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "--burrowd-image", "img:1"}, &out, &errb)
	if err != nil {
		t.Fatalf("bare install: %v\n%s", err, errb.String())
	}

	s := out.String()
	// It prints the intro, lists the contexts under a CONTEXT column, marks the current one, shows the
	// per-context install status in a BURROWD column, then the Examples block and a single Usage line.
	for _, want := range []string{
		"Install the Burrow control plane into your cluster.",
		"CURRENT", "CONTEXT", "CLUSTER", "BURROWD",
		"dev", "prod", "broken", "*",
		"installed (v0.7.0)", "not installed", "unreachable",
		"# Install Burrow into a context with the defaults",
		"burrow install do-nyc1-cluster",
		"burrow install <context> [flags]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("context listing missing %q:\n%s", want, s)
		}
	}
	// A blank line separates the heading from the table header so they do not run together.
	if !strings.Contains(s, "Detected Kubernetes contexts:\n\n") {
		t.Errorf("expected a blank line after the contexts heading:\n%s", s)
	}
	// Usage sits at the bottom, after the Examples block (kubectl-style layout).
	if i, j := strings.Index(s, "Examples:"), strings.Index(s, "Usage:"); i < 0 || j < 0 || i > j {
		t.Errorf("Examples should appear before the Usage line at the bottom:\n%s", s)
	}
	// It must NOT install: nothing applied, nothing recorded.
	if *targeted != "" {
		t.Errorf("bare install should not apply anything, but targeted %q", *targeted)
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if cfg.Current != "" || len(cfg.Environments) != 0 {
		t.Errorf("bare install should record nothing, got current=%q envs=%+v", cfg.Current, cfg.Environments)
	}
}

func TestInstallNoKubeconfigStops(t *testing.T) {
	stubInstall(t, nil, errors.New("no configuration has been provided"))

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "--burrowd-image", "img:1"}, &out, &errb)
	if err == nil {
		t.Fatal("install with no kubeconfig should error")
	}
	for _, want := range []string{"no kubeconfig found", "$KUBECONFIG", "~/.kube/config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing-kubeconfig stop missing %q, got: %v", want, err)
		}
	}
}

func TestInstallUnknownContextErrors(t *testing.T) {
	stubInstall(t, twoContexts(), nil)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "staging", "--burrowd-image", "img:1", "--wait=false"}, &out, &errb)
	if err == nil {
		t.Fatal("install into an unknown context should error")
	}
	if !strings.Contains(err.Error(), "staging") || !strings.Contains(err.Error(), "dev, prod") {
		t.Errorf("unknown-context error should name the bad context and list available ones, got: %v", err)
	}
}

func TestClusterIngressReplacesSystem(t *testing.T) {
	// `system` is gone.
	var sysOut, sysErr bytes.Buffer
	if err := run(context.Background(), []string{"system", "ingress", "install", "--dry-run"}, &sysOut, &sysErr); err == nil {
		t.Error("the `system` command group should be removed")
	}
	// `cluster ingress install` exists (dry-run needs no cluster).
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "ingress", "install", "--dry-run"}, &out, &errb); err != nil {
		t.Fatalf("cluster ingress install --dry-run: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "kind: ClusterIssuer") {
		t.Errorf("cluster ingress install should print the ingress plan:\n%s", out.String())
	}
}

func TestRenderManifests(t *testing.T) {
	out, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: "apps", Image: "registry.example.com/burrowd:1",
		Token: "tok-123", DBPassword: "pw-456", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}

	for _, want := range []string{
		"kind: Namespace",
		"name: apps", // a custom app namespace is created
		"kind: ServiceAccount",
		"kind: Role",
		"kind: RoleBinding",
		"name: burrowd-api-token",
		`token: "tok-123"`,
		`postgres://burrow:pw-456@postgres:5432/burrow`,
		"image: postgres:18",
		"image: registry.example.com/burrowd:1",
		// The control-plane Postgres runs with lean server settings for a low-traffic metadata
		// store (matching LeanPostgresSettings in controlplane/kube/addons.go) and declares a
		// memory footprint so the stack fits a small VPS predictably.
		`"shared_buffers=64MB"`,
		`"max_connections=30"`,
		`"work_mem=4MB"`,
		`"maintenance_work_mem=32MB"`,
		`"effective_cache_size=256MB"`,
		"limits: { memory: 320Mi }",               // Postgres memory limit (headroom over ~100-150MB steady state)
		"requests: { cpu: 50m, memory: 96Mi }",    // Postgres request
		"limits: { memory: 192Mi }",               // burrowd memory limit
		"requests: { cpu: 25m, memory: 64Mi }",    // burrowd request
		"{ name: BURROW_NAMESPACE, value: apps }", // burrowd deploys apps into the app namespace
		"-listen=:8080",
		"path: /healthz",
		`verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]`,
		`resources: ["services"]`,                    // expose creates Services (ADR-0018)
		`resources: ["ingresses"]`,                   // ... and Ingresses
		`resources: ["jobs"]`,                        // Postgres backup/restore run as Jobs (ADR-0032)
		`verbs: ["get", "list", "create", "delete"]`, // ... created and reaped by burrowd, namespace-scoped
		"name: burrow-credentials",                   // the empty vendor-credential Secret (ADR-0023)
		`resourceNames: ["burrow-credentials"]`,      // burrowd's credentials grant, scoped to it
		`verbs: ["get", "update"]`,                   // get + update on exactly that Secret (ADR-0030)
		"fieldPath: metadata.namespace",              // POD_NAMESPACE: where burrowd reads credentials
		"kind: ClusterRole",                          // the one read-only capability ClusterRole (ADR-0034)
		"kind: ClusterRoleBinding",                   // ... bound to the burrowd ServiceAccount
		"name: burrowd-cluster-capabilities",         // ... its name
		`resources: ["nodes"]`,                       // capability reads: nodes (provider inference, allocatable)
		`resources: ["pods"]`,                        // ... pods (sum of requests for scheduling headroom, #275)
		`resources: ["storageclasses"]`,              // ... storageclasses (default StorageClass)
		`resources: ["ingressclasses"]`,              // ... ingressclasses (IngressClass names)
		`resources: ["deployments"]`,                 // ... deployments (ingress-nginx controller readiness)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing %q", want)
		}
	}

	// The capability ClusterRole is the ONLY cluster-scoped grant and it is strictly read-only
	// (ADR-0034): exactly one ClusterRole/ClusterRoleBinding, get/list only, on exactly the five
	// non-sensitive capability resources (nodes, pods, storageclasses, ingressclasses, deployments) —
	// no secrets, no writes, no other resources.
	// Anchor to a line start so the ClusterRoleBinding's roleRef (an indented `kind: ClusterRole`)
	// is not miscounted as a second ClusterRole.
	if c := strings.Count(out, "\nkind: ClusterRole\n"); c != 1 {
		t.Errorf("expected exactly one ClusterRole (the read-only capability grant), found %d", c)
	}
	if c := strings.Count(out, "kind: ClusterRoleBinding"); c != 1 {
		t.Errorf("expected exactly one ClusterRoleBinding, found %d", c)
	}
	clusterRole := out[strings.Index(out, "kind: ClusterRole\n"):]
	clusterRole = clusterRole[:strings.Index(clusterRole, "kind: ClusterRoleBinding")]
	if c := strings.Count(clusterRole, `verbs: ["get", "list"]`); c != 5 {
		t.Errorf("the capability ClusterRole must be get/list-only on all five resources, found %d such rules", c)
	}
	for _, banned := range []string{"create", "update", "patch", "delete", "watch"} {
		if strings.Contains(clusterRole, banned) {
			t.Errorf("the capability ClusterRole must be read-only but mentions %q", banned)
		}
	}
	if strings.Contains(clusterRole, "secrets") {
		t.Errorf("the capability ClusterRole must not grant any access to secrets")
	}

	// Secrets grants are deliberately limited and documented. There are exactly three:
	//   1. the resourceNames-scoped `get`/`update` on burrow-credentials in the control-plane
	//      namespace (ADR-0023/0030) — burrowd's only access to vendor-token contents, now able to
	//      write a token value it received over its authenticated control-plane API;
	//   2. an app-namespace-scoped grant on app env Secrets (ADR-0028/0029) so burrowd can
	//      list/unset keys, write a secret value it received over the authenticated control-plane
	//      API, and let a provisioned backend write a connection string; and
	//   3. an add-on-namespace-scoped grant (ADR-0031) so burrowd can create/read/delete the
	//      Postgres add-on's burrow-postgres superuser Secret. Secret values still never cross MCP
	//      (no secret-set tool) and are never logged or stored in the DB (ADR-0029/0004); and
	//   4. a build-namespace-scoped grant (ADR-0057, issue #278) so burrowd can create/read/delete
	//      the short-lived Secret holding a private source's provider token for the git clone and
	//      registry login — scoped to burrow-builds only.
	// Plus the agent's resourceNames-scoped `get` on burrowd-api-token, making five in total.
	if c := strings.Count(out, `resources: ["secrets"]`); c != 5 {
		t.Errorf("expected exactly five secrets grants (scoped credentials + app-namespace env secrets + add-on-namespace postgres secret + build-namespace provider-token secret + the agent's get on burrowd-api-token), found %d", c)
	}
	if !strings.Contains(out, `verbs: ["get", "list", "create", "update"]`) {
		t.Errorf("missing the app-namespace env-secrets grant (ADR-0028/0029)")
	}
	if !strings.Contains(out, `verbs: ["get", "create", "update", "delete"]`) {
		t.Errorf("missing the add-on-namespace postgres-secret grant (ADR-0031)")
	}

	// The metrics vmagent RBAC is no longer part of the base install: it is staged per-add-on by the
	// CLI at `burrow addon install metrics` (least privilege), so a fresh install grants no metrics
	// scraper RBAC. The base install only adds a read-only serviceaccounts:get so burrowd can verify
	// the CLI staged that grant before deploying the scraper — never create one.
	if strings.Contains(out, "burrow-vmagent") {
		t.Errorf("the vmagent RBAC must be removed from the base install (staged per-add-on by the CLI)")
	}
	if !strings.Contains(out, `resources: ["serviceaccounts"]`) || !strings.Contains(out, `verbs: ["get"]`) {
		t.Errorf("missing the read-only serviceaccounts:get grant on the burrowd-addons Role")
	}
}

// TestRenderManifestsAgentCredential pins the scoped agent credential the install manifest renders
// (ADR-0038): a burrow-agent ServiceAccount, a name-scoped Role granting EXACTLY proxy on the
// burrowd Service and get on the burrowd-api-token Secret (and nothing else), a RoleBinding of that
// Role to the SA, and a long-lived ServiceAccount-token Secret. The AgentServiceAccount defaults to
// "burrow-agent".
func TestRenderManifestsAgentCredential(t *testing.T) {
	out, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: "apps", Image: "img:1",
		Token: "t", DBPassword: "p", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}

	for _, want := range []string{
		"name: burrow-agent",       // the ServiceAccount / Role / RoleBinding name
		"name: burrow-agent-token", // the long-lived token Secret
		"type: kubernetes.io/service-account-token",
		"kubernetes.io/service-account.name: burrow-agent",
		`resources: ["services/proxy"]`,
		`resourceNames: ["burrowd", "burrowd:8080", "http:burrowd:", "https:burrowd:"]`,
		`verbs: ["get", "create", "update", "patch", "delete"]`,
		`resourceNames: ["burrowd-api-token"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing agent-credential fragment %q", want)
		}
	}

	// The agent Role is EXACTLY two rules — the proxy grant and the token-secret get — and no more.
	// Slice out the burrow-agent Role block (from its metadata to the next RoleBinding) and assert
	// its shape: exactly one services/proxy rule and one resourceNames:["burrowd-api-token"] rule,
	// with no broadening verbs on the secret grant and no other resources.
	roleStart := strings.Index(out, "kind: Role\nmetadata:\n  name: burrow-agent\n")
	if roleStart < 0 {
		t.Fatalf("no burrow-agent Role rendered:\n%s", out)
	}
	roleBlock := out[roleStart:]
	roleBlock = roleBlock[:strings.Index(roleBlock, "kind: RoleBinding")]
	if c := strings.Count(roleBlock, "- apiGroups:"); c != 2 {
		t.Errorf("the burrow-agent Role must have EXACTLY two rules, found %d:\n%s", c, roleBlock)
	}
	if strings.Count(roleBlock, `resources: ["services/proxy"]`) != 1 {
		t.Errorf("the burrow-agent Role must have exactly one services/proxy rule:\n%s", roleBlock)
	}
	if strings.Count(roleBlock, `resources: ["secrets"]`) != 1 {
		t.Errorf("the burrow-agent Role must have exactly one secrets rule:\n%s", roleBlock)
	}
	// The agent Role must reach nothing beyond the proxy and the one token Secret: no list/watch (so
	// it cannot enumerate secrets), no pods, no deployments, no other namespaces. (create/update/
	// patch/delete appear legitimately on the services/proxy rule, so they are not banned here.)
	for _, banned := range []string{"list", "watch", "pods", "deployments", "services\n", `resources: ["services"]`} {
		if strings.Contains(roleBlock, banned) {
			t.Errorf("the burrow-agent Role must not grant %q:\n%s", banned, roleBlock)
		}
	}

	// The AgentServiceAccount defaults to burrow-agent when installOptions leaves it empty.
	o := installOptions{Namespace: "burrow", AppNamespace: "apps", Image: "img:1", Token: "t", DBPassword: "p", Port: 8080}
	if o.AgentServiceAccount != "" {
		t.Fatalf("test precondition: AgentServiceAccount should start empty")
	}
	if _, err := renderManifests(o); err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
}

func TestRenderManifestsDefaultAppNamespace(t *testing.T) {
	out, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: "default", Image: "img:1",
		Token: "t", DBPassword: "p", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	// The default app namespace already exists, so we must not emit a Namespace for it
	// (which would relabel the cluster's default namespace).
	if strings.Contains(out, "name: default") {
		t.Errorf("must not create/relabel the default namespace")
	}
	if !strings.Contains(out, "{ name: BURROW_NAMESPACE, value: default }") {
		t.Errorf("burrowd should deploy apps into the default namespace")
	}
}

func TestRenderManifestsBurrowAppsNamespace(t *testing.T) {
	out, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: "burrow-apps", Image: "img:1",
		Token: "t", DBPassword: "p", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	// burrow-apps is neither the cluster's default namespace nor the control-plane namespace,
	// so install creates it (and labels it Burrow-managed).
	if !strings.Contains(out, "name: burrow-apps") {
		t.Errorf("must create the burrow-apps app namespace")
	}
	if !strings.Contains(out, "{ name: BURROW_NAMESPACE, value: burrow-apps }") {
		t.Errorf("burrowd should deploy apps into the burrow-apps namespace")
	}
}

// TestRenderManifestsWithoutRegistry asserts `burrow install` provisions ONLY the control plane
// (ADR-0054): the optional in-cluster registry (ADR-0053 §5) is never part of the install manifests —
// no registry resources and no default-push-target env — so a plain install costs no extra
// PersistentVolume or memory. The registry is a standalone `burrow cluster registry install`.
func TestRenderManifestsWithoutRegistry(t *testing.T) {
	out, err := renderManifests(installOptions{
		Namespace: "burrow", AppNamespace: "burrow-apps", Image: "img:1",
		Token: "t", DBPassword: "p", Port: 8080,
	})
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	for _, unwanted := range []string{
		"name: burrow-registry",
		"BURROW_BUILD_REGISTRY",
		"project-zot",
		"kind: NodePort",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a plain install must not render the in-cluster registry, but found %q", unwanted)
		}
	}
}

func TestInstallDefaultAppNamespace(t *testing.T) {
	// Lock the install command's --app-namespace default to burrow-apps: defaulting to the
	// cluster's shared `default` namespace would put burrowd's Secrets grant (ADR-0029) there.
	cmd := newInstallCmd()
	f := cmd.Flags().Lookup("app-namespace")
	if f == nil {
		t.Fatal("install has no --app-namespace flag")
	}
	if f.DefValue != "burrow-apps" {
		t.Errorf("--app-namespace default = %q, want %q", f.DefValue, "burrow-apps")
	}
}

func TestSummarizeApply(t *testing.T) {
	results := []applyResult{
		{"namespace/burrow", "created"},
		{"serviceaccount/burrowd", "unchanged"},
		{"role/burrowd-credentials", "created"},
		{"rolebinding/burrowd-credentials", "created"},
		{"secret/burrow-credentials", "configured"},
		{"deployment/burrowd", "configured"},
	}
	var b bytes.Buffer
	summarizeApplyResults(results, &b)
	got := b.String()
	want := "✓ Applied 6 resource(s): 3 created, 2 configured, 1 unchanged.\n"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	// No results (e.g. a no-op) summarizes to nothing.
	var empty bytes.Buffer
	summarizeApplyResults(nil, &empty)
	if empty.Len() != 0 {
		t.Errorf("empty apply should print nothing, got %q", empty.String())
	}
}

func TestInstallDryRun(t *testing.T) {
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "--dry-run", "--namespace", "ns1", "--burrowd-image", "my/img:2"}, &out, &errb)
	if err != nil {
		t.Fatalf("install --dry-run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "namespace: ns1") || !strings.Contains(s, "image: my/img:2") {
		t.Errorf("dry-run output did not reflect flags:\n%s", s)
	}
	// dry-run must not require a cluster — it just prints.
	if strings.Contains(s, "installed into namespace") {
		t.Errorf("dry-run should not print the post-apply message")
	}
}

func TestBurrowdTagFor(t *testing.T) {
	cases := []struct {
		version string
		want    string // the resolved burrowd image tag, "" = no published image
	}{
		// Real release tags are used as-is — they are actual published images.
		{"v0.3.0", "v0.3.0"},
		{"v1.0.0", "v1.0.0"},
		{"v0.2.2-rc1", "v0.2.2-rc1"}, // a prerelease release tag is still a real tag
		// A Go pseudo-version (a local `go build` past a tag, what Go 1.24+ stamps) resolves to the
		// release it sits on top of — never to a pseudo tag, for which no image is published.
		{"v0.3.1-0.20260628005014-4b3d4cca70f3", "v0.3.0"},         // built one commit past v0.3.0
		{"v0.3.0-rc1.0.20260628005014-abcdef012345", "v0.3.0-rc1"}, // built past a prerelease tag
		// Build metadata (Go's "+dirty" for an uncommitted tree) is dropped — "+" is invalid in an
		// image tag, so the resolved tag must be clean.
		{"v0.3.1-0.20260628005838-64338d5fade9+dirty", "v0.3.0"},
		{"v0.3.0+dirty", "v0.3.0"},
		// No matching published image: no prior tag, "(devel)", or empty.
		{"v0.0.0-20260101000000-abcdefabcdef", ""},
		{"(devel)", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := burrowdTagFor(c.version); got != c.want {
			t.Errorf("burrowdTagFor(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}

func TestInstallRequiresImageWhenNoDefault(t *testing.T) {
	// With no --burrowd-image and an empty resolved default (an unreleased build with no published
	// image), install must refuse with a clear error rather than deploy an empty/guessed image.
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"install", "--burrowd-image", "", "--dry-run"}, &out, &errb)
	if err == nil {
		t.Fatal("install with an empty burrowd image should error, got nil")
	}
	if !strings.Contains(err.Error(), "--burrowd-image") {
		t.Errorf("error should tell the user to pass --burrowd-image, got: %v", err)
	}
}

// TestDeploymentRolledOut pins the readiness predicate waitForDeployment uses so `burrow upgrade`
// (a rolling update) only reports the control plane ready once the NEW revision is fully rolled
// out. The regression it guards: Status.ReadyReplicas counts ready pods across BOTH the old and
// new ReplicaSets, so the old pod satisfied it while the new pod was still ContainerCreating and
// the watcher greenlit the old revision. Mid-rollout states must be false; only the completed
// state is true.
func TestDeploymentRolledOut(t *testing.T) {
	one := int32(1)
	dep := func(gen, obsGen, replicas, updated, available, ready int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: int64(gen)},
			Spec:       appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: int64(obsGen),
				Replicas:           replicas,
				UpdatedReplicas:    updated,
				AvailableReplicas:  available,
				ReadyReplicas:      ready,
			},
		}
	}
	cases := []struct {
		name string
		d    *appsv1.Deployment
		want bool
	}{
		// Fresh deploy still in progress: no new-template pod created yet.
		{"fresh deploy in progress", dep(1, 1, 0, 0, 0, 0), false},
		// Mid-rollout surge: old+new both Ready (ReadyReplicas=2) but the new revision is not yet
		// the only one — this is the exact state the old ReadyReplicas>=desired check mis-read.
		{"mid-rollout surge", dep(2, 2, 2, 1, 1, 2), false},
		// New pod created but not yet Available (still ContainerCreating).
		{"new pod not available", dep(2, 2, 1, 1, 0, 1), false},
		// Fully rolled out: new revision is the only one, created and available.
		{"fully rolled out", dep(2, 2, 1, 1, 1, 1), true},
		// Stale status: the controller has not observed the new spec yet.
		{"stale observedGeneration", dep(2, 1, 1, 1, 1, 1), false},
	}
	for _, c := range cases {
		if got := deploymentRolledOut(c.d); got != c.want {
			t.Errorf("%s: deploymentRolledOut = %v, want %v", c.name, got, c.want)
		}
	}

	// desired == 0 (scaled to zero) is never "rolled out" for readiness purposes.
	zero := int32(0)
	scaledDown := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: &zero},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1},
	}
	if deploymentRolledOut(scaledDown) {
		t.Errorf("desired=0 should not be considered rolled out")
	}
}

// TestWaitForDeploymentMarksReady drives waitForDeployment against a fake clientset holding a
// fully rolled-out Deployment and asserts the success line ends with the ✓ glyph. The output
// buffer is not a terminal, so the mark must be the plain glyph with no ANSI escape — color must
// never leak into captured/piped output.
func TestWaitForDeploymentMarksReady(t *testing.T) {
	one := int32(1)
	cs := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "burrowd", Namespace: "burrow", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ReadyReplicas:      1,
		},
	})
	var out bytes.Buffer
	if err := waitForDeployment(context.Background(), cs, "burrow", "burrowd", "control plane", &out, time.Minute); err != nil {
		t.Fatalf("waitForDeployment: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "✓") {
		t.Errorf("ready output should contain the ✓ glyph, got %q", s)
	}
	if strings.Contains(s, "\x1b") {
		t.Errorf("non-TTY ready output must not contain an ANSI escape, got %q", s)
	}
}

// TestWaitForDeploymentMarksTimeout drives waitForDeployment against a Deployment that never rolls
// out with an immediately-elapsed timeout and asserts the failure line carries the ✗ glyph, again
// with no ANSI escape leaking into the non-terminal buffer.
func TestWaitForDeploymentMarksTimeout(t *testing.T) {
	one := int32(1)
	cs := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "burrowd", Namespace: "burrow", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		// No status: never rolled out, so the wait falls through to the timeout branch.
	})
	var out bytes.Buffer
	err := waitForDeployment(context.Background(), cs, "burrow", "burrowd", "control plane", &out, 0)
	if err == nil {
		t.Fatal("waitForDeployment should time out when the deployment never rolls out")
	}
	s := out.String()
	if !strings.Contains(s, "✗") {
		t.Errorf("timeout output should contain the ✗ glyph, got %q", s)
	}
	if strings.Contains(s, "\x1b") {
		t.Errorf("non-TTY timeout output must not contain an ANSI escape, got %q", s)
	}
}

// TestPostInstallGuidancePointsAtAgent confirms the post-install tail routes the user to wire their
// agent to the scoped `burrow-agent` CLI (Burrow is agent-driven, not CLI-deploy-driven) rather than
// at the old `burrow app deploy` message or the retired MCP server (ADR-0049), and stays free of
// em-dashes as user-facing copy.
func TestPostInstallGuidancePointsAtAgent(t *testing.T) {
	for _, want := range []string{
		"Burrow is ready. Wire your AI agent to operate it:",
		"burrow agent claude install",
		"Then open your agent and ask it to deploy your app.",
	} {
		if !strings.Contains(postInstallGuidance, want) {
			t.Errorf("post-install guidance missing %q:\n%s", want, postInstallGuidance)
		}
	}
	if strings.Contains(postInstallGuidance, "burrow app deploy") {
		t.Errorf("post-install guidance should no longer point at `burrow app deploy`:\n%s", postInstallGuidance)
	}
	if strings.Contains(postInstallGuidance, "burrow mcp") {
		t.Errorf("post-install guidance should no longer point at the retired MCP server (ADR-0049):\n%s", postInstallGuidance)
	}
	if strings.Contains(postInstallGuidance, "—") {
		t.Errorf("post-install guidance must not contain an em-dash:\n%s", postInstallGuidance)
	}
}
