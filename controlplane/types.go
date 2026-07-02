// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "time"

// The types in this file are the values the seams (seams.go) exchange with the control
// plane — the small, code-free descriptions and observed states that cross the boundary
// between core logic and the adapters. They carry no behavior beyond their fields; the
// interfaces that pass them live in seams.go.

// WorkloadKind names the Kubernetes resource a workload maps to (ADR-0011). The seam
// speaks in workloads rather than a single resource type so new kinds are additive. The
// empty value means WorkloadDeployment.
type WorkloadKind string

const (
	// WorkloadDeployment is a stateless Deployment — the only kind v0.1 uses.
	WorkloadDeployment WorkloadKind = "Deployment"
	// WorkloadStatefulSet is a stateful StatefulSet, for workloads needing stable
	// identity, persistent volumes, or ordered rollout. Not used in v0.1; reserved so
	// adding it later is additive, not a rename.
	WorkloadStatefulSet WorkloadKind = "StatefulSet"
)

// WorkloadSpec is the desired state of one App's Kubernetes workload — the small,
// code-free description a deploy turns into (ADR-0004): a kind, a pullable image, and
// metadata.
type WorkloadSpec struct {
	App     string
	Kind    WorkloadKind
	Image   string
	Env     map[string]string
	Command []string
	// MetricsPort, when positive, is the container port the app serves Prometheus metrics on.
	// buildDeployment annotates the pod template (prometheus.io/scrape, /port, /path) so the
	// metrics add-on's scraper discovers and scrapes /metrics on it. Zero adds no annotations.
	MetricsPort int32
	Replicas    int32
}

// ExposeSpec describes how to make an app reachable at a hostname (ADR-0018). v0.2 routes
// HTTP to the app's Service via an Ingress, optionally with TLS issued by cert-manager.
type ExposeSpec struct {
	// App is the application to expose; its workload provides the Service's backends.
	App string
	// Host is the external hostname to route, e.g. app.example.com.
	Host string
	// Port is the app's container port the Service forwards to. Must be positive.
	Port int32
	// TLS requests an HTTPS certificate for Host via cert-manager (the Ingress is annotated
	// for the Issuer ClusterIssuer, and a TLS Secret is named for cert-manager to fill).
	TLS bool
	// Issuer is the cert-manager ClusterIssuer to request the certificate from when TLS.
	Issuer string
}

// WorkloadStatus is the observed state of an App's workload, as reported by the cluster.
type WorkloadStatus struct {
	App             string       `json:"app"`
	Kind            WorkloadKind `json:"kind"`
	Image           string       `json:"image"`
	DesiredReplicas int32        `json:"desired_replicas"`
	ReadyReplicas   int32        `json:"ready_replicas"`
	UpdatedReplicas int32        `json:"updated_replicas"`
	// Available reports whether the workload currently meets its availability
	// condition (enough ready replicas to serve).
	Available bool `json:"available"`
	// Issue is a human- and agent-actionable explanation of why an unavailable workload is
	// blocked, when the cluster reports a genuinely blocking pod condition — e.g. a pull
	// failure that names the image, the registry host, and the `burrow registry login`
	// fix (ADR-0006). It is best-effort enrichment: empty when the workload is healthy or
	// when no blocking condition was observed, so it never becomes a required field.
	Issue string `json:"issue,omitempty"`
	// IssueReason is the raw, machine-usable Kubernetes reason behind Issue (e.g.
	// "ImagePullBackOff" or "ErrImagePull"), for an agent that wants to branch on the cause
	// rather than parse the prose. Empty whenever Issue is empty.
	IssueReason string `json:"issue_reason,omitempty"`
}

// ExposureStatus is the observed state of an app's exposure, for the reachability surface
// (ADR-0018). Address is the controller-assigned external IP or hostname, read from the
// Ingress's status; it is empty until an ingress controller assigns one.
type ExposureStatus struct {
	Exposed bool
	Host    string
	Address string
	// TLS reports whether the Ingress requests a certificate (its spec has a TLS entry).
	TLS bool
	// CertReady reports whether the requested TLS certificate has been issued (its Secret holds a
	// certificate). It is meaningful only when TLS is true.
	CertReady bool
}

// LogOptions selects which log lines to return.
type LogOptions struct {
	// TailLines bounds how many of the most recent lines to return. Zero means an
	// adapter-defined default.
	TailLines int
}

// LogLine is a single line of application log output.
type LogLine struct {
	Pod       string    `json:"pod"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

// ImageInfo is what a registry knows about an image reference.
type ImageInfo struct {
	// Reference is the reference that was resolved.
	Reference string
	// Digest is the content digest the reference resolves to (e.g. "sha256:...").
	Digest string
}

// DNSRecordType is the kind of DNS record the control plane manages (ADR-0018). A host is
// pointed at an IPv4 address with an A record or at another hostname with a CNAME; the engine
// chooses based on the address it is given.
type DNSRecordType string

const (
	RecordA     DNSRecordType = "A"
	RecordCNAME DNSRecordType = "CNAME"
)

// DNSRecord is one record the control plane manages on the user's behalf.
type DNSRecord struct {
	// Type is A or CNAME.
	Type DNSRecordType
	// Name is the fully-qualified host, e.g. app.example.com.
	Name string
	// Value is the target: an IPv4 address for an A record, a hostname for a CNAME.
	Value string
	// TTL is the record's time to live in seconds; 0 means the provider's default.
	TTL int
}
