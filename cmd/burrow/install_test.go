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

	t.Cleanup(func() {
		listContexts = origList
		clientsetFn = origCS
		applyFn = origApply
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
	stubScanProbe(t, func(kubeContext string) (string, error) {
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
	// It prints the header/usage, lists the contexts, marks the current one, shows the per-context
	// install status in a BURROWD column, and instructs re-running with a free context.
	for _, want := range []string{
		"Installs Burrow into your cluster.",
		"burrow install <context>",
		"CURRENT", "NAME", "CLUSTER", "BURROWD",
		"dev", "prod", "broken", "*",
		"installed (v0.7.0)", "not installed", "unreachable",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("context listing missing %q:\n%s", want, s)
		}
	}
	// A blank line separates the heading from the table header so they do not run together.
	if !strings.Contains(s, "Your kubeconfig contexts:\n\n") {
		t.Errorf("expected a blank line after the contexts heading:\n%s", s)
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
		`resources: ["nodes"]`,                       // capability reads: nodes (provider inference)
		`resources: ["storageclasses"]`,              // ... storageclasses (default StorageClass)
		`resources: ["ingressclasses"]`,              // ... ingressclasses (ingress controller)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing %q", want)
		}
	}

	// The capability ClusterRole is the ONLY cluster-scoped grant and it is strictly read-only
	// (ADR-0034): exactly one ClusterRole/ClusterRoleBinding, get/list only, on exactly the three
	// non-sensitive capability resources — no secrets, no writes, no other resources.
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
	if c := strings.Count(clusterRole, `verbs: ["get", "list"]`); c != 3 {
		t.Errorf("the capability ClusterRole must be get/list-only on all three resources, found %d such rules", c)
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
	//      (no secret-set tool) and are never logged or stored in the DB (ADR-0029/0004).
	if c := strings.Count(out, `resources: ["secrets"]`); c != 3 {
		t.Errorf("expected exactly three secrets grants (scoped credentials + app-namespace env secrets + add-on-namespace postgres secret), found %d", c)
	}
	if !strings.Contains(out, `verbs: ["get", "list", "create", "update"]`) {
		t.Errorf("missing the app-namespace env-secrets grant (ADR-0028/0029)")
	}
	if !strings.Contains(out, `verbs: ["get", "create", "update", "delete"]`) {
		t.Errorf("missing the add-on-namespace postgres-secret grant (ADR-0031)")
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
	want := "Applied 6 resource(s): 3 created, 2 configured, 1 unchanged.\n"
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
