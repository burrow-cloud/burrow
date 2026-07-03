// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/burrow-cloud/burrow/localconfig"
)

// tokenSecret builds a ServiceAccount-token Secret populated as the token controller would leave it.
func tokenSecret(namespace, name, token, ca string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeServiceAccountToken,
		Data:       map[string][]byte{"token": []byte(token), "ca.crt": []byte(ca)},
	}
}

// TestMintAgentKubeconfig mints a scoped kubeconfig from a token-populated Secret and asserts the
// result is a self-contained, single-context kubeconfig carrying the token, the CA, and the server.
func TestMintAgentKubeconfig(t *testing.T) {
	cs := fake.NewSimpleClientset(tokenSecret("burrow", "burrow-agent-token", "tok-abc", "ca-pem-data"))
	restCfg := &rest.Config{Host: "https://api.example:6443"}

	data, err := mintAgentKubeconfig(context.Background(), cs, restCfg, "burrow", "burrow-agent-token")
	if err != nil {
		t.Fatalf("mintAgentKubeconfig: %v", err)
	}

	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("parsing minted kubeconfig: %v", err)
	}
	if len(cfg.Contexts) != 1 {
		t.Fatalf("expected exactly one context, got %d", len(cfg.Contexts))
	}
	if cfg.CurrentContext != agentKubeContextName {
		t.Errorf("current-context = %q, want %q", cfg.CurrentContext, agentKubeContextName)
	}
	kc := cfg.Contexts[cfg.CurrentContext]
	if kc == nil {
		t.Fatalf("current-context %q is not defined", cfg.CurrentContext)
	}
	auth := cfg.AuthInfos[kc.AuthInfo]
	if auth == nil || auth.Token != "tok-abc" {
		t.Errorf("user token = %+v, want tok-abc", auth)
	}
	cl := cfg.Clusters[kc.Cluster]
	if cl == nil {
		t.Fatalf("cluster %q is not defined", kc.Cluster)
	}
	if cl.Server != "https://api.example:6443" {
		t.Errorf("server = %q, want https://api.example:6443", cl.Server)
	}
	if string(cl.CertificateAuthorityData) != "ca-pem-data" {
		t.Errorf("CA = %q, want ca-pem-data (from the token Secret)", cl.CertificateAuthorityData)
	}
	if kc.Namespace != "burrow" {
		t.Errorf("context namespace = %q, want burrow", kc.Namespace)
	}
}

// TestMintAgentKubeconfigFallsBackToRestCA covers the case where the token Secret carries no ca.crt:
// the CA falls back to the REST config's inline CAData.
func TestMintAgentKubeconfigFallsBackToRestCA(t *testing.T) {
	cs := fake.NewSimpleClientset(tokenSecret("burrow", "burrow-agent-token", "tok-abc", "" /* no ca.crt */))
	restCfg := &rest.Config{Host: "https://api.example:6443"}
	restCfg.TLSClientConfig.CAData = []byte("rest-ca-data")

	data, err := mintAgentKubeconfig(context.Background(), cs, restCfg, "burrow", "burrow-agent-token")
	if err != nil {
		t.Fatalf("mintAgentKubeconfig: %v", err)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("parsing minted kubeconfig: %v", err)
	}
	cl := cfg.Clusters["burrow"]
	if cl == nil || string(cl.CertificateAuthorityData) != "rest-ca-data" {
		t.Errorf("CA = %v, want the REST config's rest-ca-data fallback", cl)
	}
}

// TestMintAgentKubeconfigTimesOut asserts that an unpopulated token Secret (the token controller
// never fills it) fails with a clear timeout error rather than hanging.
func TestMintAgentKubeconfigTimesOut(t *testing.T) {
	// A Secret exists but has no token key: mint must poll and then give up.
	empty := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "burrow-agent-token", Namespace: "burrow"},
		Type:       corev1.SecretTypeServiceAccountToken,
	}
	cs := fake.NewSimpleClientset(empty)
	restCfg := &rest.Config{Host: "https://api.example:6443"}

	origTimeout, origInterval := agentTokenPollTimeout, agentTokenPollInterval
	agentTokenPollTimeout, agentTokenPollInterval = 30*time.Millisecond, 5*time.Millisecond
	defer func() { agentTokenPollTimeout, agentTokenPollInterval = origTimeout, origInterval }()

	_, err := mintAgentKubeconfig(context.Background(), cs, restCfg, "burrow", "burrow-agent-token")
	if err == nil {
		t.Fatal("mintAgentKubeconfig should time out when the token is never populated")
	}
	if !strings.Contains(err.Error(), "was not populated") {
		t.Errorf("timeout error should explain the token was not populated, got: %v", err)
	}
}

// TestWriteAgentKubeconfig asserts the scoped kubeconfig is written under ~/.burrow/agents (never
// ~/.kube/config) at 0600 inside a 0700 directory.
func TestWriteAgentKubeconfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "burrow-config")
	t.Setenv("BURROW_CONFIG", cfgPath)

	path, err := writeAgentKubeconfig("my-prod", []byte("kubeconfig-bytes"))
	if err != nil {
		t.Fatalf("writeAgentKubeconfig: %v", err)
	}

	// It lives in an "agents" directory beside the local config, never in the kube config.
	wantDir := filepath.Join(filepath.Dir(cfgPath), "agents")
	if filepath.Dir(path) != wantDir {
		t.Errorf("kubeconfig written to %q, want it under %q", path, wantDir)
	}
	if strings.Contains(path, filepath.Join(".kube", "config")) {
		t.Fatalf("the scoped kubeconfig must never target ~/.kube/config, got %q", path)
	}

	// The file is 0600 under a 0700 directory.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat kubeconfig: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("kubeconfig perms = %o, want 0600", perm)
	}
	di, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("stat agents dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("agents dir perms = %o, want 0700", perm)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != "kubeconfig-bytes" {
		t.Errorf("kubeconfig contents = %q (err %v), want kubeconfig-bytes", got, err)
	}
}

// TestAgentDirUnderBurrowConfig confirms agentDir sits beside the local config so $BURROW_CONFIG
// keeps them together, and is never the kube config directory.
func TestAgentDirUnderBurrowConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "sub", "config")
	t.Setenv("BURROW_CONFIG", cfgPath)
	dir, err := agentDir()
	if err != nil {
		t.Fatalf("agentDir: %v", err)
	}
	want := filepath.Join(filepath.Dir(cfgPath), "agents")
	if dir != want {
		t.Errorf("agentDir = %q, want %q", dir, want)
	}
	// Cross-check against localconfig.Path so the two never drift.
	p, _ := localconfig.Path()
	if filepath.Dir(dir) != filepath.Dir(p) {
		t.Errorf("agentDir parent %q should equal localconfig dir %q", filepath.Dir(dir), filepath.Dir(p))
	}
}

// TestInstallDryRunDoesNotMint asserts `--dry-run` renders the manifests (including the agent
// resources) but never mints or writes a kubeconfig, since there is no cluster to reach.
func TestInstallDryRunDoesNotMint(t *testing.T) {
	minted := false
	orig := mintAgentCredentialFn
	mintAgentCredentialFn = func(_ context.Context, _ installArgs, _ string, _ kubernetes.Interface, _ io.Writer) (string, string, error) {
		minted = true
		return "", "", nil
	}
	t.Cleanup(func() { mintAgentCredentialFn = orig })

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"install", "--dry-run", "--namespace", "ns1", "--burrowd-image", "img:2"}, &out, &errb); err != nil {
		t.Fatalf("install --dry-run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "name: burrow-agent") || !strings.Contains(s, "name: burrow-agent-token") {
		t.Errorf("dry-run should still RENDER the agent resources:\n%s", s)
	}
	if minted {
		t.Errorf("dry-run must NOT mint or write a kubeconfig (no cluster to reach)")
	}
}
