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
	// LoadBalancer is whether Service type=LoadBalancer is likely supported, detected from whatever
	// services LoadBalancers — a cloud provider, k3s's servicelb, or MetalLB.
	LoadBalancer LoadBalancerCapability `json:"load_balancer"`
	// CertManager is whether cert-manager is installed, detected via its API group (its CRDs).
	CertManager CertManagerCapability `json:"cert_manager"`
	// MetricsServer is whether metrics-server is serving the Kubernetes Metrics API (metrics.k8s.io),
	// detected via API-group discovery. It powers `kubectl top`, HPA CPU/memory autoscaling, and the
	// utilization layer of capacity reporting; Burrow auto-ensures it as a baseline (ADR-0054 §1).
	MetricsServer MetricsServerCapability `json:"metrics_server"`
	// CloudNativePG is the CloudNativePG operator's situation: whether its CRDs are served, whether
	// a controller is actually running, and which release it is (ADR-0066 §1). It is the cluster
	// prerequisite the Postgres add-on runs on, installed by an operator-CLI setup
	// step because CRDs need cluster-admin.
	CloudNativePG CloudNativePGCapability `json:"cloudnative_pg"`
	// PgBackRest is the CloudNativePG pgBackRest plugin's situation (ADR-0066 §3): the component that
	// archives a Postgres instance's write-ahead log and takes its base backups. It is reported
	// separately from CloudNativePG because the two fail apart — an operator with no plugin runs
	// databases that cannot be backed up off-cluster — and installed by the same operator-CLI step,
	// for the same reason: its CRDs need cluster-admin.
	PgBackRest PgBackRestCapability `json:"pgbackrest"`
	// ControlPlaneDatabase is which shape the control plane's own database runs in — a
	// CloudNativePG cluster or a plain Deployment (ADR-0086 §2) — and whether it is backed up. It is
	// reported here so the answer outlives the install output that stated it.
	ControlPlaneDatabase ControlPlaneDatabaseCapability `json:"control_plane_database"`
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

// LoadBalancerCapability reports whether Service type=LoadBalancer is likely supported, and by what
// (ADR-0043). Supported is true when any LoadBalancer provider is present: a recognized cloud
// provider (a billable cloud load balancer), k3s's built-in servicelb, or MetalLB. Provider names
// which one — a cloud id (e.g. "digitalocean"), "servicelb", or "metallb" — empty when none is
// detected; only a cloud provider is billable. Inferred is always true: this recognizes a provider,
// not a direct probe (provisioning a LoadBalancer is the real test).
type LoadBalancerCapability struct {
	Supported bool   `json:"supported"`
	Inferred  bool   `json:"inferred"`
	Provider  string `json:"provider,omitempty"`
}

// CertManagerCapability reports whether cert-manager is installed. Present is true when the
// cert-manager.io API group is served — i.e. its CRDs are installed — detected via API-group
// discovery, which needs no RBAC.
type CertManagerCapability struct {
	Present bool `json:"present"`
}

// MetricsServerCapability reports whether metrics-server is serving the Kubernetes Metrics API. It
// is two facts rather than one because they fail apart, for the same reason CloudNativePGCapability
// below is three (ADR-0096 §1):
//
//   - Present is whether the Metrics API is being SERVED — the metrics.k8s.io group advertises a
//     usable version, so `kubectl top`, an HPA, and utilization reporting will get an answer. This
//     is the only field a caller should treat as "metrics-server works".
//   - Registered is whether the group exists at all. The Metrics API is served through the
//     aggregation layer, so a metrics-server that has stopped answering leaves the group registered
//     with its version marked stale. Registered without Present is precisely that state, and it must
//     not read as installed — nor as absent, since something is already registered there.
//
// Both are detected via API-group discovery, which needs no RBAC. Discovery reports what the
// aggregation layer advertises, not the result of a metrics query: see ADR-0096 §4 for what that
// still cannot see.
type MetricsServerCapability struct {
	Present    bool `json:"present"`
	Registered bool `json:"registered,omitempty"`
}

// CloudNativePGCapability reports the CloudNativePG operator, the cluster prerequisite ADR-0066 §1
// puts the Postgres add-on on — `addon install postgres` is refused without it. It is three facts
// rather than one because they fail apart, and each failure looks like the others from the outside:
//
//   - Present is whether the postgresql.cnpg.io API group is served — the CRDs are installed, so a
//     `Cluster` object can be written. Detected via API-group discovery, which needs no RBAC.
//   - Ready is whether a controller is actually RUNNING. CRDs are cluster-scoped and outlive the
//     operator that installed them, so a cluster can accept a `Cluster` object that nothing will
//     ever reconcile. Present without Ready is precisely that state, and it must not read as
//     installed.
//   - Version is the running operator's release, read from its image tag, empty when unknown.
//     Pinned is the release Burrow targets — a constant, not a cluster read. They are reported
//     side by side because the placement translation Burrow writes into a `Cluster` is a claim
//     about a specific release's schema (ADR-0077 §3), so a cluster running an older operator is a
//     fact the operator of it should be able to see rather than discover from a pruned field.
type CloudNativePGCapability struct {
	Present bool   `json:"present"`
	Ready   bool   `json:"ready"`
	Version string `json:"version,omitempty"`
	Pinned  string `json:"pinned,omitempty"`
}

// PgBackRestCapability reports the CloudNativePG pgBackRest plugin, the component a Postgres
// instance archives through (ADR-0066 §3). Present is whether its CRDs are served; Ready is whether
// its controller is actually running, kept apart for CloudNativePGCapability's reason — a CRD
// outlives the controller that installed it, and a `Stanza` written against a served CRD with
// nothing behind it is accepted and reconciled by nothing.
//
// It reports NO running version, unlike CloudNativePGCapability. The plugin's release artifact does
// not carry its version anywhere Burrow can read back, so what Burrow targets is stated (Pinned) and
// what is running is stated as present or not — a version guessed off an image tag would be a claim
// about the component holding the backups that Burrow cannot stand behind.
type PgBackRestCapability struct {
	Present bool   `json:"present"`
	Ready   bool   `json:"ready"`
	Pinned  string `json:"pinned,omitempty"`
}

// ControlPlaneDatabaseCapability reports which of the two shapes the control plane's OWN database
// is running in (ADR-0086 §2), read live from the control-plane namespace.
//
// It exists because the answer must survive the install output scrolling away. Both shapes work,
// they are not equally protected, and from the outside they are indistinguishable: an install whose
// state has no backup looks exactly like one whose state is archived to object storage. The choice
// is made once, at install, and this is where it is readable afterwards.
//
//   - Kind is "cloudnativepg" or "plain", empty when the read was not available (an older burrowd,
//     or a build with no control-plane namespace wired).
//   - Ready is whether an instance is actually serving. It is meaningful only for "cloudnativepg";
//     a "plain" database's readiness is its Deployment's, which the control plane answering this
//     call at all already demonstrates.
//   - BackedUp is whether the database archives off-cluster. It is false for every "plain" install,
//     which cannot, and for a "cloudnativepg" one with no object-storage provider registered, which
//     is what an install is on the day it is created (ADR-0086 §4).
type ControlPlaneDatabaseCapability struct {
	Kind     string `json:"kind,omitempty"`
	Ready    bool   `json:"ready"`
	BackedUp bool   `json:"backed_up"`
}

// The values ControlPlaneDatabaseCapability.Kind takes.
const (
	ControlPlaneDatabaseCloudNativePG = "cloudnativepg"
	ControlPlaneDatabasePlain         = "plain"
)

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
// detect cert-manager, then detects
// LoadBalancer support from whatever services LoadBalancers (a cloud provider, servicelb, or
// MetalLB). It is the seam over those reads so the engine
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
