// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
)

func fullCaps() client.ClusterCapabilities {
	return client.ClusterCapabilities{
		Ingress:      client.IngressCapability{Present: true, Classes: []string{"nginx"}},
		Storage:      client.StorageCapability{DefaultPresent: true, DefaultClass: "do-block-storage", Classes: []string{"do-block-storage"}},
		LoadBalancer: client.LoadBalancerCapability{Supported: true, Inferred: true},
		CertManager:  client.CertManagerCapability{Present: false},
		Provider:     client.ProviderCapability{Cloud: "digitalocean", Name: "DigitalOcean"},
		DNS:          client.DNSCapability{Configured: false},
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
	want := "nginx IngressClass · default StorageClass do-block-storage · provider DigitalOcean · cert-manager not installed"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestCapabilitySummaryBareMetal(t *testing.T) {
	caps := client.ClusterCapabilities{
		Ingress:  client.IngressCapability{Present: false},
		Storage:  client.StorageCapability{DefaultPresent: false},
		Provider: client.ProviderCapability{},
	}
	got := capabilitySummary(caps)
	for _, want := range []string{"no ingress controller", "no default StorageClass", "provider unknown", "cert-manager not installed"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}
