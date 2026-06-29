// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

func TestClusterCapabilitiesFillsDNSFromRegistry(t *testing.T) {
	ctx := context.Background()
	d := fake.NewDatabase()
	// A configured DNS provider in the registry — the engine fills the DNS capability from it.
	if err := d.SaveProvider(ctx, cp.Provider{
		Name: "do-dns", Type: cp.ProviderDigitalOcean, Capabilities: []cp.Capability{cp.CapabilityDNS}, SecretKey: "do-dns",
	}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	prober := fake.NewClusterProber(cp.ClusterCapabilities{
		Ingress:      cp.IngressCapability{Present: true, Classes: []string{"nginx"}},
		Storage:      cp.StorageCapability{DefaultPresent: true, DefaultClass: "do-block-storage"},
		LoadBalancer: cp.LoadBalancerCapability{Supported: true, Inferred: true},
		Provider:     cp.ProviderCapability{Cloud: "digitalocean", Name: "DigitalOcean"},
	})

	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Registry: fake.NewRegistry(), Database: d,
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(), ClusterProber: prober,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	caps, err := e.ClusterCapabilities(ctx)
	if err != nil {
		t.Fatalf("ClusterCapabilities: %v", err)
	}
	// The cluster-derived fields pass through from the prober.
	if !caps.Ingress.Present || caps.Storage.DefaultClass != "do-block-storage" {
		t.Errorf("cluster fields not passed through: %+v", caps)
	}
	// DNS is filled from the registry.
	if !caps.DNS.Configured || len(caps.DNS.Providers) != 1 || caps.DNS.Providers[0] != "do-dns" {
		t.Errorf("DNS = %+v, want configured with provider do-dns", caps.DNS)
	}
}

func TestClusterCapabilitiesNoDNSProvider(t *testing.T) {
	ctx := context.Background()
	prober := fake.NewClusterProber(cp.ClusterCapabilities{
		Ingress: cp.IngressCapability{Present: true, Classes: []string{"traefik"}},
	})
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Registry: fake.NewRegistry(), Database: fake.NewDatabase(),
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(), ClusterProber: prober,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps, err := e.ClusterCapabilities(ctx)
	if err != nil {
		t.Fatalf("ClusterCapabilities: %v", err)
	}
	if caps.DNS.Configured {
		t.Errorf("DNS should be unconfigured with no provider, got %+v", caps.DNS)
	}
}

func TestClusterCapabilitiesNotConfigured(t *testing.T) {
	ctx := context.Background()
	// No ClusterProber wired — the read errors cleanly with ErrNotImplemented.
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Registry: fake.NewRegistry(), Database: fake.NewDatabase(),
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.ClusterCapabilities(ctx); !errors.Is(err, cp.ErrNotImplemented) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
}
