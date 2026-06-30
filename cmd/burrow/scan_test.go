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
