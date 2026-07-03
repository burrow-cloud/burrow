// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/burrow-cloud/burrow/internal/jointoken"
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

// fakeK3sInstaller records whether Install/WaitForAPI were called and with what command.
type fakeK3sInstaller struct {
	running       bool
	installedWith *k3sInstallCommand
	waited        bool
}

func (f *fakeK3sInstaller) Running(context.Context) (bool, error) { return f.running, nil }

func (f *fakeK3sInstaller) Install(_ context.Context, cmd k3sInstallCommand) error {
	f.installedWith = &cmd
	return nil
}

func (f *fakeK3sInstaller) WaitForAPI(context.Context) error {
	f.waited = true
	return nil
}

// TestEnsureK3sInstalledSkipsWhenRunning asserts an already-running k3s is not reinstalled.
func TestEnsureK3sInstalledSkipsWhenRunning(t *testing.T) {
	f := &fakeK3sInstaller{running: true}
	var out discardWriter
	if err := ensureK3sInstalled(context.Background(), f, buildK3sInstallCommand("203.0.113.10"), out); err != nil {
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
// waits for the API.
func TestEnsureK3sInstalledRunsInstaller(t *testing.T) {
	f := &fakeK3sInstaller{running: false}
	var out discardWriter
	cmd := buildK3sInstallCommand("203.0.113.10")
	if err := ensureK3sInstalled(context.Background(), f, cmd, out); err != nil {
		t.Fatalf("ensureK3sInstalled: %v", err)
	}
	if f.installedWith == nil {
		t.Fatal("Install was not called on a fresh box")
	}
	assertFlagValue(t, f.installedWith.Args, "--tls-san", "203.0.113.10")
	if !f.waited {
		t.Error("WaitForAPI was not called after a fresh install")
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
