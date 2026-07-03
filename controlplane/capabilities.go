// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
)

// ClusterCapabilities is a neutral, read-only report of what a cluster can do (ADR-0034): an
// ingress controller and its IngressClass, a default StorageClass, LoadBalancer support, whether
// cert-manager is installed, the cloud provider, and whether a DNS provider is configured. It is
// the low-trust agent entry point — the agent can survey a cluster and explain its state before
// anything is changed. It replaces the cluster-type picker: Burrow observes the cluster rather
// than asking the user to classify it. Each capability reports present / absent / inferred, never
// an opinion about whether the cluster "should" have it.
type ClusterCapabilities struct {
	// Ingress is the ingress-controller situation: whether a controller is actually running, and
	// which IngressClass(es) exist.
	Ingress IngressCapability `json:"ingress"`
	// Storage is the default-StorageClass situation: whether a default exists and its name.
	Storage StorageCapability `json:"storage"`
	// LoadBalancer is whether Service type=LoadBalancer is likely supported, inferred from the
	// detected cloud provider.
	LoadBalancer LoadBalancerCapability `json:"load_balancer"`
	// CertManager is whether cert-manager is installed, detected via its API group (its CRDs).
	CertManager CertManagerCapability `json:"cert_manager"`
	// Provider is the detected cloud provider, inferred from node labels / providerID.
	Provider ProviderCapability `json:"provider"`
	// DNS is whether a DNS provider is configured in the registry (ADR-0023) — a control-plane
	// fact, not a cluster read. It is filled by the engine, not the cluster probe.
	DNS DNSCapability `json:"dns"`
}

// IngressCapability reports the cluster's ingress-controller situation. Present — the "you can
// expose" signal — is true only when an ingress controller is actually running (a ready
// ingress-nginx controller Deployment), NOT merely when an IngressClass exists: an IngressClass is
// cluster-scoped and can outlive the controller that created it (deleting the ingress-nginx release
// and its namespace leaves the "nginx" class orphaned), and an orphan class routes nothing. Classes
// are the IngressClass names found, sorted; they are reported independently of Present because
// binding an Ingress still needs the class name (e.g. while the controller is being reinstalled).
type IngressCapability struct {
	// Present is true only when a ready ingress controller is running — the signal that an expose
	// will actually get an external address and admission webhook. It is not implied by Classes.
	Present bool `json:"present"`
	// Classes are the IngressClass names that exist, sorted. A class may be present while Present is
	// false (an orphan class whose controller was removed).
	Classes []string `json:"classes,omitempty"`
}

// StorageCapability reports the cluster's persistent-storage situation. DefaultPresent is true
// when a StorageClass carries the default-class annotation; DefaultClass is its name; Classes are
// all StorageClass names found, sorted.
type StorageCapability struct {
	DefaultPresent bool     `json:"default_present"`
	DefaultClass   string   `json:"default_class,omitempty"`
	Classes        []string `json:"classes,omitempty"`
}

// LoadBalancerCapability reports whether Service type=LoadBalancer is likely supported. Supported
// is inferred from the detected provider — a known cloud likely provisions a load balancer, while
// bare-metal / single-node clusters are NodePort-only. Inferred is always true: this is an
// inference from the provider, not a direct probe (provisioning a LoadBalancer is the real test).
type LoadBalancerCapability struct {
	Supported bool `json:"supported"`
	Inferred  bool `json:"inferred"`
}

// CertManagerCapability reports whether cert-manager is installed. Present is true when the
// cert-manager.io API group is served — i.e. its CRDs are installed — detected via API-group
// discovery, which needs no RBAC.
type CertManagerCapability struct {
	Present bool `json:"present"`
}

// ProviderCapability reports the detected cloud provider. Cloud is the provider id (e.g.
// "digitalocean", "aws"), empty when unknown or bare-metal; Name is a human label (e.g.
// "DigitalOcean").
type ProviderCapability struct {
	Cloud string `json:"cloud,omitempty"`
	Name  string `json:"name,omitempty"`
}

// DNSCapability reports whether a DNS provider is configured in the registry (ADR-0023).
// Configured is true when at least one provider serves the DNS capability; Providers names them.
type DNSCapability struct {
	Configured bool     `json:"configured"`
	Providers  []string `json:"providers,omitempty"`
}

// ClusterProber detects a cluster's capabilities read-only (ADR-0034): it reads IngressClasses,
// ingress-nginx controller Deployments, StorageClasses, and Nodes, and uses API-group discovery to
// detect cert-manager, then infers
// LoadBalancer support from the detected provider. It is the seam over those reads so the engine
// stays unit-testable against a fake; the production adapter (controlplane/kube) wraps a client-go
// clientset, and the same detection runs whether driven by the kubeconfig client (install) or
// burrowd's in-cluster client. It returns only the cluster-derived capabilities; the DNS field is
// filled by the engine from the providers registry. It is an optional seam — present only when
// wired; ClusterCapabilities errors cleanly (ErrNotImplemented) when it is nil.
type ClusterProber interface {
	// DetectCapabilities reads the cluster's capabilities read-only. It never writes.
	DetectCapabilities(ctx context.Context) (ClusterCapabilities, error)
}

// ClusterCapabilities reports what the cluster can do, read live so out-of-band changes are always
// reflected (ADR-0034). It runs the cluster probe through the ClusterProber seam and fills the DNS
// capability from the providers registry (ADR-0023) — a control-plane fact, not a cluster read. It
// is read-only: it changes nothing in the cluster or the registry.
func (e *Engine) ClusterCapabilities(ctx context.Context) (ClusterCapabilities, error) {
	if e.prober == nil {
		return ClusterCapabilities{}, fmt.Errorf("cluster capabilities: detection is not configured: %w", ErrNotImplemented)
	}
	caps, err := e.prober.DetectCapabilities(ctx)
	if err != nil {
		return ClusterCapabilities{}, fmt.Errorf("cluster capabilities: %w", err)
	}
	providers, err := e.db.Providers(ctx)
	if err != nil {
		return ClusterCapabilities{}, fmt.Errorf("cluster capabilities: reading providers: %w", err)
	}
	var dnsNames []string
	for _, p := range providers {
		if p.Serves(CapabilityDNS) {
			dnsNames = append(dnsNames, p.Name)
		}
	}
	caps.DNS = DNSCapability{Configured: len(dnsNames) > 0, Providers: dnsNames}
	return caps, nil
}
