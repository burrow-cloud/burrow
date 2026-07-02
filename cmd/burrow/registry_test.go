// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// nsWithDefaultSA returns a fake cluster with just the default ServiceAccount present in ns,
// as every real namespace has.
func nsWithDefaultSA(ns string) *fake.Clientset {
	return fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
	})
}

func registrySecretConfig(t *testing.T, cs *fake.Clientset, ns string) dockerConfig {
	t.Helper()
	s, err := cs.CoreV1().Secrets(ns).Get(context.Background(), registrySecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting registry secret: %v", err)
	}
	if s.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("registry secret type = %q, want %q", s.Type, corev1.SecretTypeDockerConfigJson)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		t.Fatalf("unmarshaling dockerconfigjson: %v", err)
	}
	return cfg
}

func defaultSAPullSecrets(t *testing.T, cs *fake.Clientset, ns string) []string {
	t.Helper()
	sa, err := cs.CoreV1().ServiceAccounts(ns).Get(context.Background(), "default", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting default SA: %v", err)
	}
	var names []string
	for _, ref := range sa.ImagePullSecrets {
		names = append(names, ref.Name)
	}
	return names
}

func TestRegistryLoginCreatesSecretAndPatchesSA(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	if err := registryLogin(context.Background(), cs, "apps", "ghcr.io", "alice", "tok123"); err != nil {
		t.Fatalf("registryLogin: %v", err)
	}

	cfg := registrySecretConfig(t, cs, "apps")
	auth, ok := cfg.Auths["ghcr.io"]
	if !ok {
		t.Fatal("ghcr.io not in the registry secret")
	}
	if auth.Username != "alice" || auth.Password != "tok123" {
		t.Errorf("stored credential = %q/%q, want alice/tok123", auth.Username, auth.Password)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("alice:tok123")); auth.Auth != want {
		t.Errorf("auth field = %q, want %q", auth.Auth, want)
	}

	if got := defaultSAPullSecrets(t, cs, "apps"); len(got) != 1 || got[0] != registrySecretName {
		t.Errorf("default SA imagePullSecrets = %v, want [%s]", got, registrySecretName)
	}
}

func TestRegistryLoginMergesAndDoesNotDuplicateSARef(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	ctx := context.Background()
	if err := registryLogin(ctx, cs, "apps", "ghcr.io", "alice", "t1"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if err := registryLogin(ctx, cs, "apps", "registry.example.com", "bob", "t2"); err != nil {
		t.Fatalf("second login: %v", err)
	}

	cfg := registrySecretConfig(t, cs, "apps")
	if len(cfg.Auths) != 2 || cfg.Auths["ghcr.io"].Password != "t1" || cfg.Auths["registry.example.com"].Password != "t2" {
		t.Errorf("merged auths wrong: %+v", cfg.Auths)
	}
	// The SA must reference the pull secret exactly once, not once per registry.
	if got := defaultSAPullSecrets(t, cs, "apps"); len(got) != 1 {
		t.Errorf("default SA imagePullSecrets = %v, want a single ref", got)
	}
}

func TestRegistryLogoutRemovesHostAndCleansUp(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	ctx := context.Background()
	mustLogin(t, cs, "apps", "ghcr.io", "alice", "t1")
	mustLogin(t, cs, "apps", "registry.example.com", "bob", "t2")

	// Removing one of two leaves the secret with the other and keeps the SA ref.
	if err := registryLogout(ctx, cs, "apps", "ghcr.io"); err != nil {
		t.Fatalf("logout ghcr.io: %v", err)
	}
	cfg := registrySecretConfig(t, cs, "apps")
	if _, ok := cfg.Auths["ghcr.io"]; ok {
		t.Error("ghcr.io should be gone")
	}
	if _, ok := cfg.Auths["registry.example.com"]; !ok {
		t.Error("registry.example.com should remain")
	}
	if got := defaultSAPullSecrets(t, cs, "apps"); len(got) != 1 {
		t.Errorf("SA ref should remain while a registry is configured, got %v", got)
	}

	// Removing the last one deletes the secret and detaches it from the SA.
	if err := registryLogout(ctx, cs, "apps", "registry.example.com"); err != nil {
		t.Fatalf("logout last: %v", err)
	}
	_, err := cs.CoreV1().Secrets("apps").Get(ctx, registrySecretName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("registry secret should be deleted when empty, got err=%v", err)
	}
	if got := defaultSAPullSecrets(t, cs, "apps"); len(got) != 0 {
		t.Errorf("SA ref should be detached when no registries remain, got %v", got)
	}
}

func TestRegistryListSorted(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	ctx := context.Background()
	mustLogin(t, cs, "apps", "registry.example.com", "bob", "t2")
	mustLogin(t, cs, "apps", "ghcr.io", "alice", "t1")

	hosts, err := registryList(ctx, cs, "apps")
	if err != nil {
		t.Fatalf("registryList: %v", err)
	}
	if len(hosts) != 2 || hosts[0] != "ghcr.io" || hosts[1] != "registry.example.com" {
		t.Errorf("registryList = %v, want [ghcr.io registry.example.com]", hosts)
	}

	// An empty cluster lists nothing without erroring.
	empty, err := registryList(ctx, nsWithDefaultSA("apps"), "apps")
	if err != nil || len(empty) != 0 {
		t.Errorf("expected empty list with no error, got %v, %v", empty, err)
	}
}

// runRegistry drives the real registry subcommand RunE against a fake clientset, returning its
// stdout. It pins the app namespace with --app-namespace so the discovery path is skipped, and
// forces a non-interactive stdin so credential resolution is deterministic (no ambient TTY).
func runRegistry(t *testing.T, cs *fake.Clientset, args ...string) string {
	t.Helper()
	out, errb, err := execRegistry(t, cs, "", false, args...)
	if err != nil {
		t.Fatalf("registry %v: %v (stderr: %s)", args, err, errb)
	}
	return out
}

// execRegistry drives the registry command with an explicit stdin and interactive-terminal flag,
// returning stdout, stderr, and the RunE error. The terminal flag drives the stdinIsTerminal seam
// so the prompt paths are exercised without a real TTY.
func execRegistry(t *testing.T, cs *fake.Clientset, stdin string, terminal bool, args ...string) (string, string, error) {
	t.Helper()
	orig := registryClientset
	registryClientset = func(string) (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { registryClientset = orig })

	origTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return terminal }
	t.Cleanup(func() { stdinIsTerminal = origTerm })

	var out, errb bytes.Buffer
	cmd := newRegistryCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"--app-namespace", "apps"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// TestRegistryOutputHasNoNamespaceJargon locks the developer-facing result messages to plain,
// non-Kubernetes language: Burrow's users are not cluster experts, so the raw term "namespace"
// must not leak into login, logout, or empty-list output.
func TestRegistryOutputHasNoNamespaceJargon(t *testing.T) {
	// Empty-list state.
	if got := runRegistry(t, nsWithDefaultSA("apps"), "list"); got != "no image registries configured\n" {
		t.Errorf("empty list output = %q, want %q", got, "no image registries configured\n")
	}

	// Login success.
	cs := nsWithDefaultSA("apps")
	if got := runRegistry(t, cs, "login", "ghcr.io", "-u", "alice", "-p", "tok123"); got != "configured registry \"ghcr.io\" for your apps\n" {
		t.Errorf("login output = %q, want %q", got, "configured registry \"ghcr.io\" for your apps\n")
	}

	// Logout.
	if got := runRegistry(t, cs, "logout", "ghcr.io"); got != "removed registry \"ghcr.io\"\n" {
		t.Errorf("logout output = %q, want %q", got, "removed registry \"ghcr.io\"\n")
	}

	// Belt and braces: none of the result messages may contain the jargon term.
	for _, got := range []string{
		runRegistry(t, nsWithDefaultSA("apps"), "list"),
		runRegistry(t, nsWithDefaultSA("apps"), "login", "ghcr.io", "-u", "a", "-p", "b"),
	} {
		if strings.Contains(got, "namespace") {
			t.Errorf("registry output leaks %q: %q", "namespace", got)
		}
	}
}

// TestRegistryLoginPasswordStdin feeds the token on standard input with the username supplied by
// flag: the non-interactive automation path. The credential must reach the stored Secret.
func TestRegistryLoginPasswordStdin(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	out, errb, err := execRegistry(t, cs, "tok-stdin\n", false, "login", "ghcr.io", "-u", "alice", "--password-stdin")
	if err != nil {
		t.Fatalf("login --password-stdin: %v (stderr: %s)", err, errb)
	}
	if out != "configured registry \"ghcr.io\" for your apps\n" {
		t.Errorf("login output = %q", out)
	}
	cfg := registrySecretConfig(t, cs, "apps")
	if auth := cfg.Auths["ghcr.io"]; auth.Username != "alice" || auth.Password != "tok-stdin" {
		t.Errorf("stored credential = %q/%q, want alice/tok-stdin (trailing newline trimmed)", auth.Username, auth.Password)
	}
}

// TestRegistryLoginFlagPassword keeps the explicit -u/-p path working non-interactively.
func TestRegistryLoginFlagPassword(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	out, errb, err := execRegistry(t, cs, "", false, "login", "ghcr.io", "-u", "alice", "-p", "tok123")
	if err != nil {
		t.Fatalf("login -u/-p: %v (stderr: %s)", err, errb)
	}
	if out != "configured registry \"ghcr.io\" for your apps\n" {
		t.Errorf("login output = %q", out)
	}
	cfg := registrySecretConfig(t, cs, "apps")
	if auth := cfg.Auths["ghcr.io"]; auth.Username != "alice" || auth.Password != "tok123" {
		t.Errorf("stored credential = %q/%q, want alice/tok123", auth.Username, auth.Password)
	}
}

// TestRegistryLoginPasswordStdinAndFlagConflict locks the mutual exclusion of -p and
// --password-stdin: supplying both is a clear error and nothing is written.
func TestRegistryLoginPasswordStdinAndFlagConflict(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	_, _, err := execRegistry(t, cs, "tok\n", false, "login", "ghcr.io", "-u", "alice", "-p", "tok123", "--password-stdin")
	if err == nil {
		t.Fatal("expected an error when -p and --password-stdin are combined")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want it to mention mutual exclusion", err)
	}
	if _, gerr := cs.CoreV1().Secrets("apps").Get(context.Background(), registrySecretName, metav1.GetOptions{}); !apierrors.IsNotFound(gerr) {
		t.Errorf("no secret should be written on a conflict, got err=%v", gerr)
	}
}

// TestRegistryLoginNonInteractiveNoPassword covers the no-terminal-no-password case: with a
// username but no -p and no --password-stdin, and a non-interactive stdin, the command errors
// clearly and points at the non-interactive path.
func TestRegistryLoginNonInteractiveNoPassword(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	_, _, err := execRegistry(t, cs, "", false, "login", "ghcr.io", "-u", "alice")
	if err == nil {
		t.Fatal("expected an error with no password on a non-interactive stdin")
	}
	if !strings.Contains(err.Error(), "no password provided") || !strings.Contains(err.Error(), "--password-stdin") {
		t.Errorf("error = %q, want it to name the missing password and --password-stdin", err)
	}
}

// TestRegistryLoginNonInteractiveNoUsername covers a missing username with no terminal to prompt.
func TestRegistryLoginNonInteractiveNoUsername(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	_, _, err := execRegistry(t, cs, "tok\n", false, "login", "ghcr.io", "--password-stdin")
	if err == nil {
		t.Fatal("expected an error with no username on a non-interactive stdin")
	}
	if !strings.Contains(err.Error(), "no username provided") {
		t.Errorf("error = %q, want it to name the missing username", err)
	}
}

// TestRegistryLoginFlagPasswordWarnsOnTerminal checks that -p on an interactive terminal still
// works but prints the docker-style insecurity warning to stderr.
func TestRegistryLoginFlagPasswordWarnsOnTerminal(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	out, errb, err := execRegistry(t, cs, "", true, "login", "ghcr.io", "-u", "alice", "-p", "tok123")
	if err != nil {
		t.Fatalf("login -u/-p on terminal: %v (stderr: %s)", err, errb)
	}
	if out != "configured registry \"ghcr.io\" for your apps\n" {
		t.Errorf("login output = %q", out)
	}
	if !strings.Contains(errb, "insecure") {
		t.Errorf("stderr = %q, want an insecurity warning for -p on a terminal", errb)
	}
}

// TestRegistryTokenHint locks the provider-aware token guidance to the right page per registry,
// and a URL-free generic line for anything else.
func TestRegistryTokenHint(t *testing.T) {
	cases := []struct {
		host      string
		wantSub   string
		wantNoURL bool
	}{
		{"ghcr.io", "https://github.com/settings/tokens/new?scopes=read:packages", false},
		{"registry.gitlab.com", "https://gitlab.com/-/user_settings/personal_access_tokens", false},
		{"docker.io", "https://app.docker.com/settings/personal-access-tokens", false},
		{"registry.example.com", "", true},
	}
	for _, tc := range cases {
		got := registryTokenHint(tc.host)
		if tc.wantSub != "" && !strings.Contains(got, tc.wantSub) {
			t.Errorf("registryTokenHint(%q) = %q, want it to contain %q", tc.host, got, tc.wantSub)
		}
		if tc.wantNoURL {
			for _, url := range []string{"github.com", "gitlab.com", "docker.com"} {
				if strings.Contains(got, url) {
					t.Errorf("registryTokenHint(%q) = %q, want no provider URL", tc.host, got)
				}
			}
		}
	}
}

func mustLogin(t *testing.T, cs *fake.Clientset, ns, host, user, pass string) {
	t.Helper()
	if err := registryLogin(context.Background(), cs, ns, host, user, pass); err != nil {
		t.Fatalf("registryLogin %s: %v", host, err)
	}
}

// TestRegistryCommandPath locks the advertised command path to the real command tree. The
// private-registry guidance (the burrow_deploy tool description, the ImagePullBackOff status
// Issue, and getting-started) tells users to run `burrow config registry login`; the registry
// command is deliberately nested under `config`, not exposed at the top level. This guards
// against re-advertising the wrong `burrow registry login` path: earlier guidance verified the
// subcommand flags but not the parent path, so the drift slipped through.
func TestRegistryCommandPath(t *testing.T) {
	root := newRootCmd()

	// The documented path resolves to the login command with no leftover args.
	for _, sub := range []string{"login", "logout", "list"} {
		path := []string{"config", "registry", sub}
		cmd, rest, err := root.Find(path)
		if err != nil {
			t.Fatalf("Find(%v): %v", path, err)
		}
		if cmd.Name() != sub || len(rest) != 0 {
			t.Errorf("Find(%v) resolved to %q with leftover %v, want %q with no leftover",
				path, cmd.Name(), rest, sub)
		}
	}

	// A top-level `burrow registry ...` must NOT resolve: there is no top-level registry command,
	// so Find reports an unknown command rather than a login command.
	cmd, _, err := root.Find([]string{"registry", "login"})
	if err == nil && (cmd.Name() == "registry" || cmd.Name() == "login") {
		t.Errorf("`burrow registry login` should not resolve to a top-level command, got %q", cmd.Name())
	}
}
