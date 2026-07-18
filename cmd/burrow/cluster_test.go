// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
)

// TestClusterHelpListsLifecycleCommands confirms install and upgrade present under the
// cluster-lifecycle surface (ADR-0060): `burrow cluster --help` lists them as subcommands.
func TestClusterHelpListsLifecycleCommands(t *testing.T) {
	configWithEnv(t)
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "--help"}, &out, &errb); err != nil {
		t.Fatalf("cluster help: %v\n%s", err, errb.String())
	}
	s := out.String() + errb.String()
	for _, want := range []string{"install", "upgrade", "ingress", "registry", "bootstrap", "capacity"} {
		if !strings.Contains(s, want) {
			t.Errorf("cluster help missing subcommand %q\n%s", want, s)
		}
	}
}

// TestDeprecatedInstallUpgradeAliasesStillRun confirms the top-level `burrow install` / `burrow
// upgrade` keep working after the move under `cluster` (ADR-0060): they execute and print the
// one-line migration hint pointing at the new spelling. The alias reaches the same run path, proven
// here through the --dry-run branch, which needs no cluster.
func TestDeprecatedInstallUpgradeAliasesStillRun(t *testing.T) {
	// install --dry-run renders the manifests without a cluster and returns nil.
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"install", "--dry-run", "--namespace", "ns1", "--burrowd-image", "img:1"}, &out, &errb); err != nil {
		t.Fatalf("deprecated install alias: %v\n%s", err, errb.String())
	}
	if s := out.String() + errb.String(); !strings.Contains(s, "burrow cluster install") {
		t.Errorf("deprecated install alias should hint at `burrow cluster install`\n%s", s)
	}

	// upgrade -h short-circuits to help but still surfaces the deprecation hint.
	out.Reset()
	errb.Reset()
	if err := run(context.Background(), []string{"upgrade", "-h"}, &out, &errb); err != nil {
		t.Fatalf("deprecated upgrade alias help: %v\n%s", err, errb.String())
	}
	if s := out.String() + errb.String(); !strings.Contains(s, "burrow cluster upgrade") {
		t.Errorf("deprecated upgrade alias should hint at `burrow cluster upgrade`\n%s", s)
	}
}

func fullCaps() client.ClusterCapabilities {
	return client.ClusterCapabilities{
		Ingress:       client.IngressCapability{Present: true, Classes: []string{"nginx"}},
		Storage:       client.StorageCapability{DefaultPresent: true, DefaultClass: "do-block-storage", Classes: []string{"do-block-storage"}},
		LoadBalancer:  client.LoadBalancerCapability{Supported: true, Inferred: true},
		CertManager:   client.CertManagerCapability{Present: false},
		MetricsServer: client.MetricsServerCapability{Present: true},
		Provider:      client.ProviderCapability{Cloud: "digitalocean", Name: "DigitalOcean"},
		DNS:           client.DNSCapability{Configured: false},
	}
}

func TestWriteClusterReport(t *testing.T) {
	var b bytes.Buffer
	writeClusterReport(&b, fullCaps())
	out := b.String()
	for _, want := range []string{"Ingress", "nginx", "default StorageClass do-block-storage", "DigitalOcean", "cert-manager", "not installed", "supported (inferred"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}
}

func TestCapabilitySummary(t *testing.T) {
	got := capabilitySummary(fullCaps())
	want := "nginx IngressClass · default StorageClass do-block-storage · provider DigitalOcean · cert-manager not installed · metrics-server present"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// TestClusterReportOrphanIngressClass asserts a lingering IngressClass whose controller was removed
// renders honestly as an orphan with no running controller, not as a usable ingress.
func TestClusterReportOrphanIngressClass(t *testing.T) {
	var b bytes.Buffer
	caps := fullCaps()
	caps.Ingress = client.IngressCapability{Present: false, Classes: []string{"nginx"}}
	writeClusterReport(&b, caps)
	out := b.String()
	for _, want := range []string{"no running ingress controller", "orphan IngressClass nginx"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}
}

func TestCapabilitySummaryBareMetal(t *testing.T) {
	caps := client.ClusterCapabilities{
		Ingress:  client.IngressCapability{Present: false},
		Storage:  client.StorageCapability{DefaultPresent: false},
		Provider: client.ProviderCapability{},
	}
	got := capabilitySummary(caps)
	for _, want := range []string{"no ingress controller", "no default StorageClass", "provider unknown", "cert-manager not installed", "metrics-server absent"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}
