// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// writeKubeconfig builds a two-context kubeconfig (do-nyc1-dev current, do-nyc1-nonprod not)
// and writes it to a temp file, returning the path. The current context sets a namespace; the
// other sets none, so follow-mode namespace behavior is exercised both ways.
func writeKubeconfig(t *testing.T) string {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["dev"] = &api.Cluster{Server: "https://dev.example:6443"}
	cfg.Clusters["nonprod"] = &api.Cluster{Server: "https://nonprod.example:6443"}
	cfg.AuthInfos["user"] = &api.AuthInfo{Token: "t"}
	cfg.Contexts["do-nyc1-dev"] = &api.Context{Cluster: "dev", AuthInfo: "user", Namespace: "team-x"}
	cfg.Contexts["do-nyc1-nonprod"] = &api.Context{Cluster: "nonprod", AuthInfo: "user"}
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
	cfg := &Config{
		Current:      "stage",
		Environments: []Environment{{Name: "stage", Context: "ctx", AppNamespace: "apps"}},
	}
	got, err := Resolve(cfg, "")
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

// TestRender covers the display strings for each shape.
func TestRender(t *testing.T) {
	cases := []struct {
		name string
		r    Resolved
		want string
	}{
		{
			name: "pinned registered with namespace",
			r:    Resolved{Name: "nonprod", Context: "do-nyc1-nonprod", Namespace: "team-x", Mode: ModePinned},
			want: `nonprod (context "do-nyc1-nonprod", namespace "team-x")`,
		},
		{
			name: "pinned with no namespace omits it",
			r:    Resolved{Name: "prod", Context: "do-nyc1-prod", Mode: ModePinned},
			want: `prod (context "do-nyc1-prod")`,
		},
		{
			name: "following unregistered",
			r:    Resolved{Context: "do-nyc1-dev", Mode: ModeFollowing},
			want: "following kubectl: do-nyc1-dev (unregistered)",
		},
		{
			name: "following registered",
			r:    Resolved{Name: "dev", Context: "do-nyc1-dev", Namespace: "team-x", Mode: ModeFollowing},
			want: `dev (context "do-nyc1-dev", namespace "team-x") (following kubectl)`,
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
