// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/jointoken"
	"github.com/burrow-cloud/burrow/localconfig"
)

// joinTestToken is a valid encoded join token for the tests: a bearer-credential token naming the
// public API server, CA, control-plane namespace, and context.
func joinTestToken(t *testing.T) string {
	t.Helper()
	s, err := jointoken.Encode(jointoken.Token{
		Server:                   "https://203.0.113.10:6443",
		CertificateAuthorityData: []byte("vps-ca-pem"),
		BearerToken:              "admin-bearer",
		Namespace:                "burrow",
		ContextName:              "burrow-vps",
	})
	if err != nil {
		t.Fatalf("encoding test token: %v", err)
	}
	return s
}

// stubJoin points $BURROW_CONFIG and the kubeconfig at temp files and replaces joinConnectFn with a
// fake admin clientset that already holds the burrow-agent-token Secret, plus a REST config carrying
// the token's server and CA (so the scoped kubeconfig gets the right coordinates). It returns the
// kubeconfig path the admin context is recorded into. All seams are restored on cleanup.
func stubJoin(t *testing.T, agentToken, agentCA string) (kubeconfigPath string) {
	t.Helper()
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	kubeconfigPath = filepath.Join(t.TempDir(), "kubeconfig")

	orig := joinConnectFn
	joinConnectFn = func(tok jointoken.Token) (kubernetes.Interface, *rest.Config, error) {
		cs := fake.NewSimpleClientset(tokenSecret(tok.Namespace, agentTokenSecretName, agentToken, agentCA))
		restCfg := &rest.Config{Host: tok.Server}
		restCfg.TLSClientConfig.CAData = tok.CertificateAuthorityData
		return cs, restCfg, nil
	}
	t.Cleanup(func() { joinConnectFn = orig })
	return kubeconfigPath
}

// TestJoinLandsAdminAndScopedCredentials is the end-to-end happy path: `burrow join <token>` records
// admin access into the kubeconfig, writes the scoped agent kubeconfig under ~/.burrow/agents/, and
// registers+pins the environment handle carrying it.
func TestJoinLandsAdminAndScopedCredentials(t *testing.T) {
	kubeconfigPath := stubJoin(t, "scoped-agent-tok", "vps-ca-pem")
	token := joinTestToken(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"join", token, "--kubeconfig", kubeconfigPath}, &out, &errb); err != nil {
		t.Fatalf("burrow join: %v\n%s", err, errb.String())
	}

	// (a) Admin access is recorded into the kubeconfig, current-context set, so the privileged path
	// resolves this cluster by context name.
	kc, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		t.Fatalf("loading recorded kubeconfig: %v", err)
	}
	if kc.CurrentContext != "burrow-vps" {
		t.Errorf("current-context = %q, want burrow-vps", kc.CurrentContext)
	}
	cl := kc.Clusters["burrow-vps"]
	if cl == nil || cl.Server != "https://203.0.113.10:6443" {
		t.Errorf("recorded cluster = %+v, want the public API server URL", cl)
	}
	if cl != nil && string(cl.CertificateAuthorityData) != "vps-ca-pem" {
		t.Errorf("recorded CA = %q, want vps-ca-pem", cl.CertificateAuthorityData)
	}
	if auth := kc.AuthInfos["burrow-vps"]; auth == nil || auth.Token != "admin-bearer" {
		t.Errorf("recorded admin credential = %+v, want the bearer token", auth)
	}
	kctx := kc.Contexts["burrow-vps"]
	if kctx == nil || kctx.Namespace != "burrow" {
		t.Errorf("recorded context = %+v, want namespace burrow", kctx)
	}

	// (b) + (c) The handle is registered and pinned, carrying the scoped credential.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading local config: %v", err)
	}
	if cfg.Current == "" {
		t.Fatal("join did not pin an environment")
	}
	env, ok := cfg.Lookup(cfg.Current)
	if !ok {
		t.Fatalf("pinned environment %q not in config: %+v", cfg.Current, cfg.Environments)
	}
	if env.Context != "burrow-vps" {
		t.Errorf("handle context = %q, want burrow-vps", env.Context)
	}
	if env.ControlPlaneNamespace != "burrow" || env.AppNamespace != connect.DefaultAppNamespace {
		t.Errorf("handle namespaces = (%q,%q), want (burrow,%q)", env.ControlPlaneNamespace, env.AppNamespace, connect.DefaultAppNamespace)
	}
	if env.AgentContext != agentKubeContextName || env.AgentKubeconfig == "" {
		t.Errorf("handle scoped credential = (%q,%q), want it set to the scoped kubeconfig", env.AgentKubeconfig, env.AgentContext)
	}

	// (b) The scoped kubeconfig is written 0600 under ~/.burrow/agents/, with the token's server/CA
	// and the agent-token Secret's token.
	fi, err := os.Stat(env.AgentKubeconfig)
	if err != nil {
		t.Fatalf("stat scoped kubeconfig: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("scoped kubeconfig perms = %o, want 0600", perm)
	}
	if strings.Contains(env.AgentKubeconfig, filepath.Join(".kube", "config")) {
		t.Errorf("scoped kubeconfig must never target ~/.kube/config, got %q", env.AgentKubeconfig)
	}
	scoped, err := clientcmd.LoadFromFile(env.AgentKubeconfig)
	if err != nil {
		t.Fatalf("loading scoped kubeconfig: %v", err)
	}
	sc := scoped.Contexts[scoped.CurrentContext]
	if sc == nil {
		t.Fatal("scoped kubeconfig has no current context")
	}
	if scl := scoped.Clusters[sc.Cluster]; scl == nil || scl.Server != "https://203.0.113.10:6443" {
		t.Errorf("scoped server = %+v, want the public API server URL", scl)
	}
	if scl := scoped.Clusters[sc.Cluster]; scl != nil && string(scl.CertificateAuthorityData) != "vps-ca-pem" {
		t.Errorf("scoped CA = %q, want vps-ca-pem", scl.CertificateAuthorityData)
	}
	if sa := scoped.AuthInfos[sc.AuthInfo]; sa == nil || sa.Token != "scoped-agent-tok" {
		t.Errorf("scoped token = %+v, want the agent-token Secret's token", sa)
	}

	// The success summary names the env/context and both credentials.
	s := out.String()
	for _, want := range []string{"Joined the bootstrapped cluster", `context "burrow-vps"`, "admin access for governance", "scoped agent credential"} {
		if !strings.Contains(s, want) {
			t.Errorf("join summary missing %q:\n%s", want, s)
		}
	}
}

// TestJoinIsIdempotent asserts re-running join updates the recorded credential and handle in place
// without error and without duplicating the handle.
func TestJoinIsIdempotent(t *testing.T) {
	kubeconfigPath := stubJoin(t, "scoped-agent-tok", "vps-ca-pem")
	token := joinTestToken(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"join", token, "--kubeconfig", kubeconfigPath}, &out, &errb); err != nil {
		t.Fatalf("first join: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading config after first join: %v", err)
	}
	firstName := cfg.Current
	if firstName == "" || len(cfg.Environments) != 1 {
		t.Fatalf("first join should register exactly one pinned handle, got current=%q envs=%+v", firstName, cfg.Environments)
	}

	out.Reset()
	errb.Reset()
	if err := run(context.Background(), []string{"join", token, "--kubeconfig", kubeconfigPath}, &out, &errb); err != nil {
		t.Fatalf("second join: %v\n%s", err, errb.String())
	}
	cfg, err = localconfig.Load()
	if err != nil {
		t.Fatalf("loading config after second join: %v", err)
	}
	if len(cfg.Environments) != 1 {
		t.Errorf("re-running join must not duplicate the handle, got %+v", cfg.Environments)
	}
	if cfg.Current != firstName {
		t.Errorf("re-running join changed the environment name from %q to %q", firstName, cfg.Current)
	}
	env, _ := cfg.Lookup(firstName)
	if env.AgentKubeconfig == "" || env.AgentContext != agentKubeContextName {
		t.Errorf("re-run should keep the scoped credential recorded, got %+v", env)
	}
}

// TestJoinRejectsBadToken asserts a malformed token fails clearly before any local state is written.
func TestJoinRejectsBadToken(t *testing.T) {
	stubJoin(t, "scoped-agent-tok", "vps-ca-pem")

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"join", "not-a-real-token"}, &out, &errb)
	if err == nil {
		t.Fatal("join with a malformed token should error")
	}
	cfg, err2 := localconfig.Load()
	if err2 != nil {
		t.Fatalf("loading config: %v", err2)
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a rejected token must record no handle, got %+v", cfg.Environments)
	}
}

// TestJoinRespectsExplicitEnvironmentName asserts --environment names the handle.
func TestJoinRespectsExplicitEnvironmentName(t *testing.T) {
	kubeconfigPath := stubJoin(t, "scoped-agent-tok", "vps-ca-pem")
	token := joinTestToken(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"join", token, "--kubeconfig", kubeconfigPath, "--environment", "vps-prod"}, &out, &errb); err != nil {
		t.Fatalf("burrow join --environment: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if cfg.Current != "vps-prod" {
		t.Errorf("current environment = %q, want vps-prod", cfg.Current)
	}
	if _, ok := cfg.Lookup("vps-prod"); !ok {
		t.Errorf("environment vps-prod was not recorded: %+v", cfg.Environments)
	}
}
