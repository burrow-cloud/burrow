// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/jointoken"
	"github.com/burrow-cloud/burrow/localconfig"
)

// fakeIPDetector is a public-IP detector that returns a fixed IP (or error) without a network call.
type fakeIPDetector struct {
	ip  string
	err error
}

func (f fakeIPDetector) DetectPublicIP(context.Context) (string, error) { return f.ip, f.err }

// TestResolvePublicIPExplicitFlag asserts an explicit public --public-ip is used as-is and the
// detector is never consulted.
func TestResolvePublicIPExplicitFlag(t *testing.T) {
	d := fakeIPDetector{err: errors.New("detector must not be called")}
	ip, err := resolvePublicIP(context.Background(), "203.0.113.10", d)
	if err != nil {
		t.Fatalf("resolvePublicIP: %v", err)
	}
	if ip != "203.0.113.10" {
		t.Errorf("ip = %q, want 203.0.113.10", ip)
	}
}

// TestResolvePublicIPAutoDetect asserts the detector supplies the IP when --public-ip is absent.
func TestResolvePublicIPAutoDetect(t *testing.T) {
	ip, err := resolvePublicIP(context.Background(), "", fakeIPDetector{ip: "198.51.100.7"})
	if err != nil {
		t.Fatalf("resolvePublicIP: %v", err)
	}
	if ip != "198.51.100.7" {
		t.Errorf("ip = %q, want 198.51.100.7", ip)
	}
}

// TestResolvePublicIPErrors asserts the clear stops: a detector failure, a detected private address,
// and an explicit private/invalid --public-ip all error (and mention --public-ip).
func TestResolvePublicIPErrors(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		detector publicIPDetector
	}{
		{"detector fails", "", fakeIPDetector{err: errors.New("no network")}},
		{"detected private", "", fakeIPDetector{ip: "10.0.0.5"}},
		{"explicit private", "192.168.1.20", fakeIPDetector{err: errors.New("must not be called")}},
		{"explicit loopback", "127.0.0.1", fakeIPDetector{err: errors.New("must not be called")}},
		{"explicit garbage", "not-an-ip", fakeIPDetector{err: errors.New("must not be called")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolvePublicIP(context.Background(), c.explicit, c.detector)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// TestBuildK3sInstallCommandFlags asserts the install command carries the critical k3s flags
// (ADR-0044): the TLS SAN, the node external IP, the world-readable kubeconfig mode, and traefik
// disabled — while NOT disabling servicelb (the free single-node LoadBalancer).
func TestBuildK3sInstallCommandFlags(t *testing.T) {
	cmd := buildK3sInstallCommand("203.0.113.10")
	if cmd.PublicIP != "203.0.113.10" {
		t.Errorf("PublicIP = %q, want 203.0.113.10", cmd.PublicIP)
	}
	joined := cmd.Args
	assertFlagValue(t, joined, "--tls-san", "203.0.113.10")
	assertFlagValue(t, joined, "--node-external-ip", "203.0.113.10")
	assertFlagValue(t, joined, "--write-kubeconfig-mode", "0644")
	assertFlagValue(t, joined, "--disable", "traefik")
	for i, a := range joined {
		if a == "--disable" && i+1 < len(joined) && joined[i+1] == "servicelb" {
			t.Error("servicelb must not be disabled: it is the free single-node LoadBalancer")
		}
	}
}

// assertFlagValue asserts args contains flag immediately followed by want.
func assertFlagValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) || args[i+1] != want {
				t.Errorf("flag %s value = %v, want %q", flag, args[i+1:], want)
			}
			return
		}
	}
	t.Errorf("flag %s not found in %v", flag, args)
}

// fakeK3sInstaller records whether Install/WaitForAPI were called and with what command. installErr
// simulates a non-zero installer exit (e.g. systemd's readiness wait timing out on a slow first
// start); waitErr simulates the k3s API never answering within the budget.
type fakeK3sInstaller struct {
	running       bool
	installedWith *k3sInstallCommand
	installErr    error
	waited        bool
	waitBudget    time.Duration
	waitErr       error
}

func (f *fakeK3sInstaller) Running(context.Context) (bool, error) { return f.running, nil }

func (f *fakeK3sInstaller) Install(_ context.Context, cmd k3sInstallCommand) error {
	f.installedWith = &cmd
	return f.installErr
}

func (f *fakeK3sInstaller) WaitForAPI(_ context.Context, budget time.Duration) error {
	f.waited = true
	f.waitBudget = budget
	return f.waitErr
}

// TestEnsureK3sInstalledSkipsWhenRunning asserts an already-running k3s is not reinstalled.
func TestEnsureK3sInstalledSkipsWhenRunning(t *testing.T) {
	f := &fakeK3sInstaller{running: true}
	var out discardWriter
	if err := ensureK3sInstalled(context.Background(), f, buildK3sInstallCommand("203.0.113.10"), defaultK3sAPIReadyBudget, out, out); err != nil {
		t.Fatalf("ensureK3sInstalled: %v", err)
	}
	if f.installedWith != nil {
		t.Error("Install must not be called when k3s is already running")
	}
	if f.waited {
		t.Error("WaitForAPI must not be called when k3s is already running")
	}
}

// TestEnsureK3sInstalledRunsInstaller asserts a fresh box installs k3s (with the built command) and
// waits for the API with the given budget.
func TestEnsureK3sInstalledRunsInstaller(t *testing.T) {
	f := &fakeK3sInstaller{running: false}
	var out discardWriter
	cmd := buildK3sInstallCommand("203.0.113.10")
	if err := ensureK3sInstalled(context.Background(), f, cmd, 90*time.Second, out, out); err != nil {
		t.Fatalf("ensureK3sInstalled: %v", err)
	}
	if f.installedWith == nil {
		t.Fatal("Install was not called on a fresh box")
	}
	assertFlagValue(t, f.installedWith.Args, "--tls-san", "203.0.113.10")
	if !f.waited {
		t.Error("WaitForAPI was not called after a fresh install")
	}
	if f.waitBudget != 90*time.Second {
		t.Errorf("WaitForAPI budget = %s, want 90s (the budget must be threaded through)", f.waitBudget)
	}
}

// TestEnsureK3sInstalledProceedsOnInstallerExitWhenAPIReady is the exact regression from dogfooding on
// a small VPS: the installer exits non-zero (systemd's readiness wait times out on k3s's slow first
// start) but the k3s API answers within the budget. bootstrap must NOT abort — the API answering is
// the success criterion — and must log the installer's exit as a warning.
func TestEnsureK3sInstalledProceedsOnInstallerExitWhenAPIReady(t *testing.T) {
	f := &fakeK3sInstaller{running: false, installErr: errors.New("installing k3s: exit status 1")}
	var out, errb bytes.Buffer
	if err := ensureK3sInstalled(context.Background(), f, buildK3sInstallCommand("203.0.113.10"), defaultK3sAPIReadyBudget, &out, &errb); err != nil {
		t.Fatalf("a non-zero installer exit with a ready API must not abort, got: %v", err)
	}
	if !f.waited {
		t.Error("WaitForAPI must be polled even after a non-zero installer exit")
	}
	if !strings.Contains(errb.String(), "Warning:") || !strings.Contains(errb.String(), "exit status 1") {
		t.Errorf("a tolerated installer exit should be logged as a warning carrying the exit, stderr:\n%s", errb.String())
	}
}

// TestEnsureK3sInstalledFailsWhenAPINeverReady asserts that when the API never answers within the
// budget bootstrap fails, and the error carries the installer's exit and points at journalctl and
// systemctl status for diagnosis.
func TestEnsureK3sInstalledFailsWhenAPINeverReady(t *testing.T) {
	f := &fakeK3sInstaller{
		running:    false,
		installErr: errors.New("installing k3s: exit status 1"),
		waitErr:    errors.New("the k3s API did not answer within 4m0s"),
	}
	var out, errb bytes.Buffer
	err := ensureK3sInstalled(context.Background(), f, buildK3sInstallCommand("203.0.113.10"), defaultK3sAPIReadyBudget, &out, &errb)
	if err == nil {
		t.Fatal("expected an error when the k3s API never answers")
	}
	msg := err.Error()
	for _, want := range []string{"exit status 1", "journalctl -xeu k3s.service", "systemctl status k3s"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message missing %q, got: %v", want, msg)
		}
	}
}

// discardWriter is an io.Writer that drops everything, for tests that only assert seam behavior.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// k3sStyleKubeconfig is a k3s-style admin kubeconfig: a single "default" context/cluster/user with an
// inline CA and admin client cert+key, and the loopback server k3s writes.
const k3sStyleKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: dnBzLWNsdXN0ZXItY2E=
    server: https://127.0.0.1:6443
  name: default
users:
- name: default
  user:
    client-certificate-data: YWRtaW4tY2VydA==
    client-key-data: YWRtaW4ta2V5
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
`

// TestAssembleJoinTokenRewritesServer is the token-assembly round-trip: given a k3s-style admin
// kubeconfig (server 127.0.0.1) and a public IP, the produced token decodes to the public API-server
// URL with the CA and admin cert+key carried over.
func TestAssembleJoinTokenRewritesServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k3s.yaml")
	if err := os.WriteFile(path, []byte(k3sStyleKubeconfig), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}

	encoded, err := assembleJoinToken(path, "203.0.113.10", "burrow", "burrow-vps")
	if err != nil {
		t.Fatalf("assembleJoinToken: %v", err)
	}

	tok, err := jointoken.Decode(encoded)
	if err != nil {
		t.Fatalf("decoding the assembled token: %v", err)
	}
	if tok.Server != "https://203.0.113.10:6443" {
		t.Errorf("token server = %q, want https://203.0.113.10:6443 (rewritten to the public IP)", tok.Server)
	}
	if string(tok.CertificateAuthorityData) != "vps-cluster-ca" {
		t.Errorf("token CA = %q, want the cluster CA carried over", tok.CertificateAuthorityData)
	}
	if string(tok.ClientCertificateData) != "admin-cert" || string(tok.ClientKeyData) != "admin-key" {
		t.Errorf("token admin cert/key = (%q,%q), want the admin credential carried over", tok.ClientCertificateData, tok.ClientKeyData)
	}
	if tok.Namespace != "burrow" || tok.ContextName != "burrow-vps" {
		t.Errorf("token namespace/context = (%q,%q), want (burrow, burrow-vps)", tok.Namespace, tok.ContextName)
	}
}

// TestBootstrapDeploysClusterOnly drives the full `burrow cluster bootstrap` flow with every seam
// faked (no real network, k3s, or cluster) and asserts the cluster-only contract on the VPS: burrowd
// is deployed but NO local ~/.burrow environment handle is recorded and the laptop-oriented "connect
// your agent" guidance is not printed; instead the join-token block (the `burrow join` line, the
// admin-grade warning, and the laptop next steps) is printed.
func TestBootstrapDeploysClusterOnly(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))

	kcPath := filepath.Join(t.TempDir(), "k3s.yaml")
	if err := os.WriteFile(kcPath, []byte(k3sStyleKubeconfig), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}

	// Public IP: fixed, no network. k3s: already running, so install/wait are skipped.
	origIP := newIPDetector
	newIPDetector = func() publicIPDetector { return fakeIPDetector{ip: "203.0.113.10"} }
	origK3s := newK3sInstaller
	newK3sInstaller = func(string, io.Writer, io.Writer) k3sInstaller { return &fakeK3sInstaller{running: true} }

	// Install seams: the k3s context is present, the cluster is empty (not already installed), and the
	// apply is a no-op. mintAgentCredentialFn is deliberately NOT stubbed — cluster-only must not reach it.
	origList := listContexts
	listContexts = func(string) ([]connect.Context, error) {
		return []connect.Context{{Name: "default", Cluster: "default", Current: true}}, nil
	}
	origCS := clientsetFn
	clientsetFn = func(string, string) (kubernetes.Interface, error) { return fake.NewSimpleClientset(), nil }
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }

	t.Cleanup(func() {
		newIPDetector = origIP
		newK3sInstaller = origK3s
		listContexts = origList
		clientsetFn = origCS
		applyFn = origApply
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{
		"cluster", "bootstrap",
		"--public-ip", "203.0.113.10",
		"--kubeconfig", kcPath,
		"--burrowd-image", "img:1",
		"--wait=false",
	}, &out, &errb); err != nil {
		t.Fatalf("cluster bootstrap: %v\n%s", err, errb.String())
	}

	// Cluster-only: no local ~/.burrow handle recorded, nothing pinned.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading local config: %v", err)
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("cluster-only bootstrap must record no local environment handle, got %+v", cfg.Environments)
	}
	if cfg.Current != "" {
		t.Errorf("cluster-only bootstrap must pin nothing, got current %q", cfg.Current)
	}

	s := out.String()
	// The join-token block is printed for the laptop.
	if !strings.Contains(s, "burrow join "+prefixForTest) {
		t.Errorf("bootstrap output missing the `burrow join <token>` line:\n%s", s)
	}
	for _, want := range []string{"ADMIN-grade", "brew install burrow", ":6443"} {
		if !strings.Contains(s, want) {
			t.Errorf("bootstrap output missing %q:\n%s", want, s)
		}
	}
	// The laptop-oriented install guidance must NOT appear on the VPS.
	if strings.Contains(s, "Connect your AI agent") {
		t.Errorf("cluster-only bootstrap must not print the connect-your-agent guidance:\n%s", s)
	}
	if strings.Contains(s, "is now your current environment") {
		t.Errorf("cluster-only bootstrap must not record/announce a local environment:\n%s", s)
	}
}

// prefixForTest is the recognizable join-token prefix (burrowjoin.v1.), asserted in the bootstrap
// output. It is derived from a real encode so the test tracks the codec.
var prefixForTest = func() string {
	s, _ := jointoken.Encode(jointoken.Token{
		Server:                   "https://203.0.113.10:6443",
		CertificateAuthorityData: []byte("ca"),
		BearerToken:              "t",
		Namespace:                "burrow",
		ContextName:              "burrow-vps",
	})
	// "burrowjoin.v1." — everything up to and including the last dot before the payload.
	return s[:strings.LastIndex(s, ".")+1]
}()

// stubBootstrapFullFlow fakes every bootstrap seam (public IP, k3s installer, and the reused install
// path) so a `burrow cluster bootstrap` run completes without a real network, k3s, or cluster. It
// installs the given fake k3s installer, restores all seams on cleanup, and returns the k3s-style
// admin kubeconfig path to pass with --kubeconfig.
func stubBootstrapFullFlow(t *testing.T, inst k3sInstaller) string {
	t.Helper()
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))

	kcPath := filepath.Join(t.TempDir(), "k3s.yaml")
	if err := os.WriteFile(kcPath, []byte(k3sStyleKubeconfig), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}

	origIP := newIPDetector
	newIPDetector = func() publicIPDetector { return fakeIPDetector{ip: "203.0.113.10"} }
	origK3s := newK3sInstaller
	newK3sInstaller = func(string, io.Writer, io.Writer) k3sInstaller { return inst }
	origList := listContexts
	listContexts = func(string) ([]connect.Context, error) {
		return []connect.Context{{Name: "default", Cluster: "default", Current: true}}, nil
	}
	origCS := clientsetFn
	clientsetFn = func(string, string) (kubernetes.Interface, error) { return fake.NewSimpleClientset(), nil }
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }
	// A comfortable RAM reading by default so the RAM preflight is silent and the flow reaches install;
	// the RAM-preflight tests override this after calling the helper.
	origMem := readTotalMemory
	readTotalMemory = func() (uint64, error) { return 4 * (1 << 30), nil }

	t.Cleanup(func() {
		newIPDetector = origIP
		newK3sInstaller = origK3s
		listContexts = origList
		clientsetFn = origCS
		applyFn = origApply
		readTotalMemory = origMem
	})
	return kcPath
}

// runBootstrapCLI runs `burrow cluster bootstrap` against kcPath with the given extra flags and
// returns the error plus captured stdout/stderr.
func runBootstrapCLI(kcPath string, extra ...string) (error, string, string) {
	args := append([]string{
		"cluster", "bootstrap",
		"--public-ip", "203.0.113.10",
		"--kubeconfig", kcPath,
		"--burrowd-image", "img:1",
		"--wait=false",
	}, extra...)
	var out, errb bytes.Buffer
	err := run(context.Background(), args, &out, &errb)
	return err, out.String(), errb.String()
}

// TestBootstrapYesSkipsPrompt asserts --yes bypasses the confirmation entirely (confirmFn is never
// consulted) and the install proceeds.
func TestBootstrapYesSkipsPrompt(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) {
		t.Fatal("confirmFn must not be called when --yes is set")
		return false, nil
	}
	t.Cleanup(func() { confirmFn = origConfirm })

	if err, _, errb := runBootstrapCLI(kcPath, "--yes"); err != nil {
		t.Fatalf("cluster bootstrap --yes: %v\n%s", err, errb)
	}
	if inst.installedWith == nil {
		t.Error("Install must run when --yes is set")
	}
}

// TestBootstrapConfirmYesProceeds asserts a "yes" from the confirmation seam lets the install proceed.
func TestBootstrapConfirmYesProceeds(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { confirmFn = origConfirm })

	if err, _, errb := runBootstrapCLI(kcPath); err != nil {
		t.Fatalf("cluster bootstrap: %v\n%s", err, errb)
	}
	if inst.installedWith == nil {
		t.Error("Install must run after the confirmation is accepted")
	}
}

// TestBootstrapConfirmNoAborts asserts a declined confirmation (a "no", the empty-line default) stops
// before any install: the k3s installer seam is never called, and the abort is clean (no error).
func TestBootstrapConfirmNoAborts(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { confirmFn = origConfirm })

	err, _, errb := runBootstrapCLI(kcPath)
	if err != nil {
		t.Fatalf("a declined confirmation should abort cleanly, got: %v", err)
	}
	if inst.installedWith != nil {
		t.Error("Install must NOT run when the confirmation is declined")
	}
	if !strings.Contains(errb, "Aborted") {
		t.Errorf("declined bootstrap should say it aborted, stderr:\n%s", errb)
	}
}

// TestBootstrapNoTTYWithoutYesAborts asserts that with no controlling terminal (confirmFn reports
// errNoTTY) and no --yes, bootstrap errors with guidance to pass --yes — it does not hang and does
// not install.
func TestBootstrapNoTTYWithoutYesAborts(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) { return false, errNoTTY }
	t.Cleanup(func() { confirmFn = origConfirm })

	err, _, _ := runBootstrapCLI(kcPath)
	if err == nil {
		t.Fatal("expected an error when there is no terminal and --yes was not passed")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("no-tty error should tell the user to pass --yes, got: %v", err)
	}
	if inst.installedWith != nil {
		t.Error("Install must NOT run when there is no terminal to confirm on")
	}
}

// TestBootstrapAlreadyRunningSkipsPrompt asserts that when k3s is already installed and answering the
// whole run is a no-op for the install, so no confirmation is prompted (confirmFn is never called)
// even without --yes.
func TestBootstrapAlreadyRunningSkipsPrompt(t *testing.T) {
	inst := &fakeK3sInstaller{running: true}
	kcPath := stubBootstrapFullFlow(t, inst)

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) {
		t.Fatal("confirmFn must not be called when k3s is already running (a no-op)")
		return false, nil
	}
	t.Cleanup(func() { confirmFn = origConfirm })

	if err, _, errb := runBootstrapCLI(kcPath); err != nil {
		t.Fatalf("cluster bootstrap on an already-running box: %v\n%s", err, errb)
	}
	if inst.installedWith != nil {
		t.Error("Install must not run when k3s is already running")
	}
}

// TestBootstrapProceedsWhenInstallerExitsButAPIReady drives the full `burrow cluster bootstrap` flow
// with a fake installer that returns a non-zero exit while the API answers — the exact false-failure
// seen on a slow VPS. bootstrap must reach the deploy step and print the join-token block rather than
// abort with `installing k3s: exit status 1`.
func TestBootstrapProceedsWhenInstallerExitsButAPIReady(t *testing.T) {
	inst := &fakeK3sInstaller{running: false, installErr: errors.New("installing k3s: exit status 1")}
	kcPath := stubBootstrapFullFlow(t, inst)

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { confirmFn = origConfirm })

	err, out, errb := runBootstrapCLI(kcPath)
	if err != nil {
		t.Fatalf("bootstrap must proceed when the API is ready despite a non-zero installer exit, got: %v\n%s", err, errb)
	}
	if inst.installedWith == nil {
		t.Error("Install must have run")
	}
	if !inst.waited {
		t.Error("WaitForAPI must have been polled")
	}
	// The deploy step is reached: the join-token block is printed for the laptop.
	if !strings.Contains(out, "burrow join "+prefixForTest) {
		t.Errorf("bootstrap must reach the deploy step and print the join line, stdout:\n%s", out)
	}
	if !strings.Contains(errb, "Warning:") {
		t.Errorf("the tolerated installer exit should be logged as a warning, stderr:\n%s", errb)
	}
}

// TestBootstrapFailsWhenAPINeverReady drives the full flow with the API never answering: bootstrap
// must fail (not print a join token) and the error must point at journalctl and systemctl status.
func TestBootstrapFailsWhenAPINeverReady(t *testing.T) {
	inst := &fakeK3sInstaller{
		running:    false,
		installErr: errors.New("installing k3s: exit status 1"),
		waitErr:    errors.New("the k3s API did not answer within 4m0s"),
	}
	kcPath := stubBootstrapFullFlow(t, inst)

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { confirmFn = origConfirm })

	err, out, _ := runBootstrapCLI(kcPath)
	if err == nil {
		t.Fatal("bootstrap must fail when the k3s API never answers")
	}
	for _, want := range []string{"journalctl -xeu k3s.service", "systemctl status k3s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure should point at %q, got: %v", want, err)
		}
	}
	if strings.Contains(out, "burrow join "+prefixForTest) {
		t.Errorf("a failed bootstrap must not print a join token, stdout:\n%s", out)
	}
}

// TestBootstrapRefusesSubGiBRAMWithoutYes asserts that a box below ~1GB with no --yes is refused:
// bootstrap prints the 2GB-minimum refusal with the memory breakdown and the k3s-won't-start reason,
// and aborts before touching the machine — the k3s installer seam is never called and no join token is
// printed.
func TestBootstrapRefusesSubGiBRAMWithoutYes(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)
	readTotalMemory = func() (uint64, error) { return 512 * (1 << 20), nil } // 512 MiB

	// The RAM refusal happens before the confirmation, so confirmFn must never be consulted.
	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) {
		t.Fatal("confirmFn must not be called when the RAM preflight refuses")
		return false, nil
	}
	t.Cleanup(func() { confirmFn = origConfirm })

	err, out, errb := runBootstrapCLI(kcPath)
	if err != nil {
		t.Fatalf("a RAM refusal should abort cleanly, got: %v", err)
	}
	if inst.installedWith != nil {
		t.Error("Install must NOT run when the RAM preflight refuses")
	}
	if !strings.Contains(errb, "512 MiB") {
		t.Errorf("refusal should report the machine's RAM, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "minimum is 2GB") {
		t.Errorf("refusal should state the 2GB minimum, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "cert-manager") {
		t.Errorf("refusal should show the memory breakdown, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "k3s itself likely will not start") {
		t.Errorf("a sub-1GB box should be told k3s will not start, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "--yes") {
		t.Errorf("refusal should tell the user to re-run with --yes, stderr:\n%s", errb)
	}
	if strings.Contains(out, "burrow join "+prefixForTest) {
		t.Errorf("a refused bootstrap must not print a join token, stdout:\n%s", out)
	}
}

// TestBootstrapRefusesTightRAMWithoutYes asserts that a box in the ~1-2GB range with no --yes now
// refuses (changed from the old "warn and proceed"): it runs the control plane but a public site (the
// ingress controller and cert-manager) exhausts it, so bootstrap refuses with that reason and the
// breakdown, and does not install.
func TestBootstrapRefusesTightRAMWithoutYes(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)
	readTotalMemory = func() (uint64, error) { return 1536 * (1 << 20), nil } // 1.5 GiB

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) {
		t.Fatal("confirmFn must not be called when the RAM preflight refuses")
		return false, nil
	}
	t.Cleanup(func() { confirmFn = origConfirm })

	err, out, errb := runBootstrapCLI(kcPath)
	if err != nil {
		t.Fatalf("a tight-box RAM refusal should abort cleanly, got: %v", err)
	}
	if inst.installedWith != nil {
		t.Error("Install must NOT run when a 1-2GB box is refused")
	}
	if !strings.Contains(errb, "1536 MiB") {
		t.Errorf("refusal should report the machine's RAM, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "minimum is 2GB") {
		t.Errorf("refusal should state the 2GB minimum, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "cert-manager") {
		t.Errorf("refusal should show the memory breakdown, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "public site") {
		t.Errorf("a 1-2GB box should be told a public site will exhaust it, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "--yes") {
		t.Errorf("refusal should tell the user to re-run with --yes, stderr:\n%s", errb)
	}
	if strings.Contains(out, "burrow join "+prefixForTest) {
		t.Errorf("a refused bootstrap must not print a join token, stdout:\n%s", out)
	}
}

// TestBootstrapProceedsUndersizedRAMWithYes asserts that --yes overrides the RAM refusal for a box
// below the 2GB minimum: the warning and breakdown are shown but the install is reached (the flag
// already means "I know what I'm doing").
func TestBootstrapProceedsUndersizedRAMWithYes(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)
	readTotalMemory = func() (uint64, error) { return 512 * (1 << 20), nil } // 512 MiB

	err, _, errb := runBootstrapCLI(kcPath, "--yes")
	if err != nil {
		t.Fatalf("cluster bootstrap --yes on a small box: %v\n%s", err, errb)
	}
	if inst.installedWith == nil {
		t.Error("Install must run when --yes overrides the RAM refusal")
	}
	// The warning and breakdown are still shown even though --yes lets it proceed.
	if !strings.Contains(errb, "512 MiB") {
		t.Errorf("the RAM warning should still be shown under --yes, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "cert-manager") {
		t.Errorf("the memory breakdown should still be shown under --yes, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "Proceeding anyway because --yes") {
		t.Errorf("--yes should be acknowledged as the reason for proceeding, stderr:\n%s", errb)
	}
}

// TestBootstrapProceedsTightRAMWithYes asserts that --yes also overrides the refusal for a 1-2GB box:
// the public-site warning and breakdown are shown but the install is reached.
func TestBootstrapProceedsTightRAMWithYes(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)
	readTotalMemory = func() (uint64, error) { return 1536 * (1 << 20), nil } // 1.5 GiB

	err, _, errb := runBootstrapCLI(kcPath, "--yes")
	if err != nil {
		t.Fatalf("cluster bootstrap --yes on a 1-2GB box: %v\n%s", err, errb)
	}
	if inst.installedWith == nil {
		t.Error("Install must run when --yes overrides the RAM refusal")
	}
	if !strings.Contains(errb, "public site") {
		t.Errorf("the public-site warning should still be shown under --yes, stderr:\n%s", errb)
	}
	if !strings.Contains(errb, "cert-manager") {
		t.Errorf("the memory breakdown should still be shown under --yes, stderr:\n%s", errb)
	}
}

// TestBootstrapComfortableRAMNoWarning asserts that a box at or above 2GB proceeds silently — no RAM
// warning is printed.
func TestBootstrapComfortableRAMNoWarning(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)
	readTotalMemory = func() (uint64, error) { return 2 * (1 << 30), nil } // 2 GiB

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { confirmFn = origConfirm })

	err, _, errb := runBootstrapCLI(kcPath)
	if err != nil {
		t.Fatalf("a comfortable box should proceed, got: %v\n%s", err, errb)
	}
	if inst.installedWith == nil {
		t.Error("Install must run on a comfortable box")
	}
	if strings.Contains(errb, "of RAM") {
		t.Errorf("a comfortable box should get no RAM warning, stderr:\n%s", errb)
	}
}

// TestBootstrapSkipsRAMCheckWhenUnreadable asserts that when total RAM cannot be determined (e.g. a
// non-Linux dev box, or an unreadable /proc/meminfo) the preflight is skipped rather than blocking
// bootstrap: the install is reached and no RAM message is printed.
func TestBootstrapSkipsRAMCheckWhenUnreadable(t *testing.T) {
	inst := &fakeK3sInstaller{running: false}
	kcPath := stubBootstrapFullFlow(t, inst)
	readTotalMemory = func() (uint64, error) { return 0, errors.New("no /proc/meminfo") }

	origConfirm := confirmFn
	confirmFn = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { confirmFn = origConfirm })

	err, _, errb := runBootstrapCLI(kcPath)
	if err != nil {
		t.Fatalf("an unreadable meminfo must not block bootstrap, got: %v\n%s", err, errb)
	}
	if inst.installedWith == nil {
		t.Error("Install must run when the RAM check is skipped")
	}
	if strings.Contains(errb, "of RAM") {
		t.Errorf("a skipped RAM check should print no RAM message, stderr:\n%s", errb)
	}
}

// TestParseMemTotal asserts MemTotal (reported in kibibytes) is parsed to bytes, and that a meminfo
// without a MemTotal line errors so the caller skips the check.
func TestParseMemTotal(t *testing.T) {
	const meminfo = "MemTotal:        2048000 kB\nMemFree:          123456 kB\n"
	got, err := parseMemTotal([]byte(meminfo))
	if err != nil {
		t.Fatalf("parseMemTotal: %v", err)
	}
	if want := uint64(2048000) * 1024; got != want {
		t.Errorf("parseMemTotal = %d bytes, want %d", got, want)
	}
	if _, err := parseMemTotal([]byte("MemFree: 100 kB\n")); err == nil {
		t.Error("parseMemTotal should error when there is no MemTotal line")
	}
}

// TestRewriteServerHost asserts the loopback API server URL is rewritten to the public IP while the
// port is preserved.
func TestRewriteServerHost(t *testing.T) {
	got, err := rewriteServerHost("https://127.0.0.1:6443", "203.0.113.10")
	if err != nil {
		t.Fatalf("rewriteServerHost: %v", err)
	}
	if got != "https://203.0.113.10:6443" {
		t.Errorf("rewritten server = %q, want https://203.0.113.10:6443", got)
	}
}
