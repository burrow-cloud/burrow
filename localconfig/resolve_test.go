// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// writeKubeconfig builds a three-context kubeconfig (do-nyc1-dev current, do-nyc1-nonprod and
// do-nyc1-prod not) and writes it to a temp file, returning the path. The current context sets a
// namespace; the others set none, so follow-mode namespace behavior is exercised both ways. The
// non-current contexts are what a pinned handle points at, and they have to be REAL contexts: a
// pinned handle naming a context the kubeconfig does not hold is now an error in its own right.
func writeKubeconfig(t *testing.T) string {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["dev"] = &api.Cluster{Server: "https://dev.example:6443"}
	cfg.Clusters["nonprod"] = &api.Cluster{Server: "https://nonprod.example:6443"}
	cfg.AuthInfos["user"] = &api.AuthInfo{Token: "t"}
	cfg.Contexts["do-nyc1-dev"] = &api.Context{Cluster: "dev", AuthInfo: "user", Namespace: "team-x"}
	cfg.Contexts["do-nyc1-nonprod"] = &api.Context{Cluster: "nonprod", AuthInfo: "user"}
	cfg.Contexts["do-nyc1-prod"] = &api.Context{Cluster: "nonprod", AuthInfo: "user"}
	cfg.CurrentContext = "do-nyc1-dev"

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// TestResolveFollowUnregistered confirms follow mode picks the current context and its
// namespace, leaves Name empty when no handle matches, and defaults the control-plane namespace.
func TestResolveFollowUnregistered(t *testing.T) {
	kubeconfig := writeKubeconfig(t)

	got, err := Resolve(&Config{}, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Mode != ModeFollowing {
		t.Errorf("mode = %q, want following", got.Mode)
	}
	if got.Context != "do-nyc1-dev" {
		t.Errorf("context = %q, want the current context do-nyc1-dev", got.Context)
	}
	if got.Namespace != "team-x" {
		t.Errorf("namespace = %q, want the current context's namespace team-x", got.Namespace)
	}
	if got.Name != "" {
		t.Errorf("name = %q, want empty for an unregistered current context", got.Name)
	}
	if got.ControlPlaneNamespace != DefaultControlPlaneNamespace {
		t.Errorf("control-plane namespace = %q, want default %q", got.ControlPlaneNamespace, DefaultControlPlaneNamespace)
	}
}

// TestResolveFollowRegistered confirms follow mode surfaces a matching handle's Name while
// still taking the namespace from the kube context.
func TestResolveFollowRegistered(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{Environments: []Environment{
		{Name: "dev", Context: "do-nyc1-dev", AppNamespace: "ignored-in-follow"},
	}}

	got, err := Resolve(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "dev" {
		t.Errorf("name = %q, want the matching handle dev", got.Name)
	}
	if got.Namespace != "team-x" {
		t.Errorf("namespace = %q, want the kube context namespace team-x (not the handle's)", got.Namespace)
	}
}

// TestResolvePinned confirms a pinned handle resolves to that handle's context/namespaces
// regardless of the current kube context.
func TestResolvePinned(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{
		Current: "prod",
		Environments: []Environment{
			{Name: "prod", Context: "do-nyc1-prod", ControlPlaneNamespace: "burrow-prod", AppNamespace: "apps"},
		},
	}

	got, err := Resolve(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Mode != ModePinned {
		t.Errorf("mode = %q, want pinned", got.Mode)
	}
	if got.Name != "prod" || got.Context != "do-nyc1-prod" {
		t.Errorf("got %+v, want the pinned prod handle", got)
	}
	if got.Namespace != "apps" || got.ControlPlaneNamespace != "burrow-prod" {
		t.Errorf("got namespaces %q/%q, want apps/burrow-prod", got.Namespace, got.ControlPlaneNamespace)
	}
}

// TestResolvePinnedControlPlaneDefault confirms a pinned handle with no control-plane
// namespace falls back to the default.
func TestResolvePinnedControlPlaneDefault(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{
		Current:      "stage",
		Environments: []Environment{{Name: "stage", Context: "do-nyc1-prod", AppNamespace: "apps"}},
	}
	got, err := Resolve(cfg, kubeconfig)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ControlPlaneNamespace != DefaultControlPlaneNamespace {
		t.Errorf("control-plane namespace = %q, want default %q", got.ControlPlaneNamespace, DefaultControlPlaneNamespace)
	}
}

// TestResolvePinnedMissing confirms a pin to an unregistered name errors clearly.
func TestResolvePinnedMissing(t *testing.T) {
	cfg := &Config{Current: "ghost"}
	_, err := Resolve(cfg, "")
	if err == nil {
		t.Fatal("Resolve should error when the pinned handle is not registered")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q, want it to name the missing handle", err)
	}
}

// TestResolvePinnedContextRenamedAway is issue #473. A handle recorded before its kube context was
// renamed names a cluster that does not exist, and nothing used to notice: the name flowed into the
// display while the connection was made through the handle's scoped credential, so a command
// succeeded and reported a cluster the reader does not have. It now refuses, and the refusal names
// the handle, the context, and the one command that corrects it.
func TestResolvePinnedContextRenamedAway(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	cfg := &Config{
		Current: "prod",
		Environments: []Environment{
			{Name: "prod", Context: "renamed-away", AppNamespace: "apps", AgentKubeconfig: "/somewhere/agent.kubeconfig"},
		},
	}

	_, err := Resolve(cfg, kubeconfig)
	if err == nil {
		t.Fatal("Resolve should refuse a pinned handle whose kube context is not in the kubeconfig")
	}
	var missing *ContextMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %v, want a *ContextMissingError so a listing can report it instead of failing", err)
	}
	if missing.Name != "prod" || missing.Context != "renamed-away" || !missing.Pinned {
		t.Errorf("got %+v, want the pinned handle and the context it records", missing)
	}
	for _, want := range []string{"prod", "renamed-away", "burrow env use prod --context"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestResolvePinnedToleratesAnUncheckableKubeconfig covers the two states that are not staleness.
//
// A handle carrying a scoped, burrowd-only credential (ADR-0038) reaches its cluster without a
// kubeconfig context at all, so a kubeconfig that cannot be read, or that holds no contexts, is not
// evidence that the handle is wrong — and refusing on it would take working access away over a file
// this resolution otherwise has no use for.
func TestResolvePinnedToleratesAnUncheckableKubeconfig(t *testing.T) {
	cfg := &Config{
		Current:      "prod",
		Environments: []Environment{{Name: "prod", Context: "do-nyc1-prod", AppNamespace: "apps"}},
	}

	got, err := Resolve(cfg, filepath.Join(t.TempDir(), "no-such-kubeconfig"))
	if err != nil {
		t.Fatalf("Resolve with an unreadable kubeconfig: %v", err)
	}
	if got.Context != "do-nyc1-prod" {
		t.Errorf("context = %q, want the pinned handle's", got.Context)
	}

	empty := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*api.NewConfig(), empty); err != nil {
		t.Fatalf("write empty kubeconfig: %v", err)
	}
	if _, err := Resolve(cfg, empty); err != nil {
		t.Fatalf("Resolve with a kubeconfig holding no contexts: %v", err)
	}
}

// TestRender covers the display strings for each shape.
//
// The through-line: where a resolution has an environment NAME, that name is the whole answer. The
// kube context and namespace are how Burrow found the environment, not what it is, and they belong
// to `burrow auth status` rather than to every command's target line. Where there is no environment
// name, the resolution says what it did resolve through and that nothing is registered for it.
func TestRender(t *testing.T) {
	cases := []struct {
		name string
		r    Resolved
		want string
	}{
		{
			name: "pinned registered names the environment, not the cluster coordinates",
			r:    Resolved{Name: "nonprod", Context: "do-nyc1-nonprod", Namespace: "team-x", Mode: ModePinned},
			want: "nonprod",
		},
		{
			name: "following registered names the environment too",
			r:    Resolved{Name: "dev", Context: "do-nyc1-dev", Namespace: "team-x", Mode: ModeFollowing},
			want: "dev",
		},
		{
			name: "targeted with a handle inside it names the environment",
			r:    Resolved{Name: "prod", Context: "do-nyc1", Target: "do-nyc1", Mode: ModeTargeted},
			want: "prod",
		},
		{
			name: "following unregistered has only the context to name",
			r:    Resolved{Context: "do-nyc1-dev", Mode: ModeFollowing},
			want: `kube context "do-nyc1-dev" (no environment registered)`,
		},
		{
			name: "targeted with no environment names the target",
			r:    Resolved{Context: "do-nyc1", Target: "do-nyc1", Mode: ModeTargeted},
			want: `"do-nyc1" (no environment registered)`,
		},
		{
			name: "following with no current context",
			r:    Resolved{Mode: ModeFollowing},
			want: "no current kube context",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Render(); got != tc.want {
				t.Errorf("Render() = %q, want %q", got, tc.want)
			}
		})
	}
}
