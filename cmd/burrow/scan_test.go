// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/burrow-cloud/burrow/localconfig"
)

// stubScanProbe installs a fake per-context burrowd probe so `burrow env scan` runs without any
// cluster, and restores the real probe on cleanup.
func stubScanProbe(t *testing.T, probe func(kubeContext string) (string, error)) {
	t.Helper()
	orig := scanProbeFn
	scanProbeFn = func(_ context.Context, _, kubeContext, _ string) (string, error) {
		return probe(kubeContext)
	}
	t.Cleanup(func() { scanProbeFn = orig })
}

func notFoundErr() error {
	return apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, "burrowd")
}

// stubJoinAgentCredential replaces the scoped-agent join seam so `env scan` (and install/upgrade)
// tests run without a real cluster or token Secret. join is called with the write-name (the handle
// name); the stub records each call and returns whatever the provided func yields.
func stubJoinAgentCredential(t *testing.T, join func(envName string) (string, string, error)) *[]string {
	t.Helper()
	orig := joinAgentCredentialFn
	var calls []string
	joinAgentCredentialFn = func(_ context.Context, _, _, _, envName string) (string, string, error) {
		calls = append(calls, envName)
		return join(envName)
	}
	t.Cleanup(func() { joinAgentCredentialFn = orig })
	return &calls
}

// TestEnvScanBackfillsAgentCredential asserts a newly scan-registered handle gains the scoped agent
// credential (ADR-0038 §4): scan runs the join for the installed context and records its kubeconfig
// path and context on the handle.
func TestEnvScanBackfillsAgentCredential(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev")
	stubScanProbe(t, func(string) (string, error) { return "ghcr.io/burrow-cloud/burrowd:v0.6.0", nil })
	stubJoinAgentCredential(t, func(envName string) (string, string, error) {
		return "/tmp/agents/" + envName, agentKubeContextName, nil
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env scan: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env, ok := cfg.Lookup("dev")
	if !ok {
		t.Fatalf("dev handle not registered: %+v", cfg.Environments)
	}
	if env.AgentKubeconfig != "/tmp/agents/dev" || env.AgentContext != agentKubeContextName {
		t.Errorf("scan-registered handle did not gain the scoped credential: %+v", env)
	}
}

// TestEnvScanRegistersWithoutCredentialWhenAbsent covers a pre-Phase-1 install with no agent
// credential: scan registers the handle WITHOUT a scoped cred and does not fail (ADR-0038 §4).
func TestEnvScanRegistersWithoutCredentialWhenAbsent(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev")
	stubScanProbe(t, func(string) (string, error) { return "ghcr.io/burrow-cloud/burrowd:v0.6.0", nil })
	stubJoinAgentCredential(t, func(string) (string, string, error) {
		return "", "", errAgentCredentialAbsent
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env scan should not fail when the agent credential is absent: %v\n%s", err, errb.String())
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env, ok := cfg.Lookup("dev")
	if !ok {
		t.Fatalf("dev handle should still be registered without a cred: %+v", cfg.Environments)
	}
	if env.AgentKubeconfig != "" || env.AgentContext != "" {
		t.Errorf("handle should carry no scoped cred when the credential is absent: %+v", env)
	}
}

// TestEnvScanBackfillIdempotent asserts a second scan neither re-joins (no rewrite) nor duplicates
// the handle: the already-registered context is skipped, so the join seam is not called again.
func TestEnvScanBackfillIdempotent(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev")
	stubScanProbe(t, func(string) (string, error) { return "ghcr.io/burrow-cloud/burrowd:v0.6.0", nil })
	calls := stubJoinAgentCredential(t, func(envName string) (string, string, error) {
		return "/tmp/agents/" + envName, agentKubeContextName, nil
	})

	for i := 0; i < 2; i++ {
		var out, errb bytes.Buffer
		if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
			t.Fatalf("env scan run %d: %v\n%s", i, err, errb.String())
		}
	}
	if len(*calls) != 1 {
		t.Errorf("join should run once (first scan only); got %d calls: %v", len(*calls), *calls)
	}
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Environments) != 1 {
		t.Errorf("a re-scan must not duplicate the handle, got %+v", cfg.Environments)
	}
}

// TestEnvScanListsAndRegisters confirms scan prints the probe table and registers only the
// installed contexts that have no handle yet.
func TestEnvScanListsAndRegisters(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev", "prod", "broken")

	stubScanProbe(t, func(kubeContext string) (string, error) {
		switch kubeContext {
		case "dev":
			return "ghcr.io/burrow-cloud/burrowd:v0.6.0", nil
		case "prod":
			return "", notFoundErr()
		default: // broken: an unreachable cluster
			return "", &net.DNSError{Err: "no such host", Name: "broken.invalid", IsNotFound: true}
		}
	})
	// The scoped-agent join needs a real cluster and token Secret; stub it so scan records the handle
	// with a credential without a network dial.
	stubJoinAgentCredential(t, func(envName string) (string, string, error) {
		return "/tmp/agents/" + envName, agentKubeContextName, nil
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env scan: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"CONTEXT", "CLUSTER", "BURROWD", "VERSION",
		"dev", "installed", "v0.6.0",
		"prod", "not installed",
		"broken", "unreachable", "no such host",
		"Registered 1 environment handle(s): dev",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("scan output missing %q\n%s", want, s)
		}
	}

	// Only the installed context (dev) is registered as a handle.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if h, ok := cfg.Lookup("dev"); !ok {
		t.Errorf("installed context dev was not registered\n%s", s)
	} else if h.Context != "dev" || h.ControlPlaneNamespace != "burrow" {
		t.Errorf("dev handle = %+v, want context dev, control-plane namespace burrow", h)
	}
	if _, ok := cfg.Lookup("prod"); ok {
		t.Errorf("prod (not installed) should not be registered")
	}
	if _, ok := cfg.Lookup("broken"); ok {
		t.Errorf("broken (unreachable) should not be registered")
	}
}

// TestEnvScanIdempotent confirms a context that already has a handle is not registered again.
func TestEnvScanIdempotent(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev")
	// Pre-register a handle for the dev context (under a different name) so scan sees it as covered.
	cfg := &localconfig.Config{Environments: []localconfig.Environment{{Name: "existing", Context: "dev"}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	stubScanProbe(t, func(string) (string, error) {
		return "ghcr.io/burrow-cloud/burrowd:v0.6.0", nil
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env scan: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "All installed environments are already registered.") {
		t.Errorf("expected the already-registered message when every installed context already has a handle\n%s", out.String())
	}
	got, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Environments) != 1 {
		t.Errorf("environments = %d, want the single pre-existing handle (no duplicate)", len(got.Environments))
	}
}

// TestEnvScanNoneInstalled confirms the closing message when no context has Burrow installed points
// at install and names the probed control-plane namespace, rather than the bare "nothing to
// register" non-sequitur.
func TestEnvScanNoneInstalled(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev", "prod")

	stubScanProbe(t, func(string) (string, error) {
		return "", notFoundErr()
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"env", "scan", "--kubeconfig", kc}, &out, &errb); err != nil {
		t.Fatalf("env scan: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"No Burrow control plane found in any context.",
		"burrow install <context>",
		`probed the "burrow" control-plane namespace`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("none-installed closing missing %q\n%s", want, s)
		}
	}
	// Nothing installed means nothing registered.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("no handles should be registered when nothing is installed, got %+v", cfg.Environments)
	}
}
