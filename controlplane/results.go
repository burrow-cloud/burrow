// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

// The types below are the structured inputs and outputs of the engine's operations.
// Every operation returns a result an agent can reason over — what changed and, where
// relevant, the handle to undo it (ADR-0006) — rather than prose.

// DeployRequest is the small, code-free description of a deploy: a pullable image plus
// metadata (ADR-0004). No code travels here.
type DeployRequest struct {
	App      string            `json:"app"`
	Image    string            `json:"image"`
	Env      map[string]string `json:"env,omitempty"`
	Command  []string          `json:"command,omitempty"`
	Replicas int32             `json:"replicas"`
	// Confirm acknowledges a guardrail whose disposition is confirm, letting the operation
	// proceed past it (ADR-0020). It has no effect on a guardrail set to deny.
	Confirm bool `json:"confirm,omitempty"`
}

// DeployResult reports the outcome of a successful deploy.
type DeployResult struct {
	// Release is the new release that is now running.
	Release Release `json:"release"`
	// SupersededReleaseID is the release this deploy replaced, or "" if it was the
	// first deploy of the app. It is the handle a rollback would return to.
	SupersededReleaseID string `json:"superseded_release_id,omitempty"`
}

// StatusResult is the combined control-plane and cluster view of an app.
type StatusResult struct {
	App string `json:"app"`
	// HasRelease reports whether the control plane has any release recorded for the
	// app; Release holds the most recent one when true.
	HasRelease bool    `json:"has_release"`
	Release    Release `json:"release,omitempty"`
	// Running reports whether a workload currently exists in the cluster; Workload
	// holds its observed state when true.
	Running  bool           `json:"running"`
	Workload WorkloadStatus `json:"workload,omitempty"`
}

// ScaleResult reports the outcome of a scale.
// ExposeRequest describes making an app reachable at a hostname (ADR-0018).
type ExposeRequest struct {
	App  string `json:"app"`
	Host string `json:"host"`
	Port int32  `json:"port"`
	// TLS requests an HTTPS certificate for Host via cert-manager; Issuer names the
	// ClusterIssuer to use.
	TLS    bool   `json:"tls,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	// Confirm acknowledges the expose_public guardrail so the operation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// ExposeResult reports the outcome of exposing an app.
type ExposeResult struct {
	App  string `json:"app"`
	Host string `json:"host"`
	Port int32  `json:"port"`
	URL  string `json:"url"`
}

// ReachabilityResult reports whether an app is reachable at its hostname, link by link, for
// the reachability surface (ADR-0018, ADR-0022). Summary is a one-line, plain-English verdict
// for a non-expert; the fields are the full chain for advanced users and the agent.
type ReachabilityResult struct {
	App                string   `json:"app"`
	Deployed           bool     `json:"deployed"`
	Ready              bool     `json:"ready"`
	Exposed            bool     `json:"exposed"`
	Host               string   `json:"host,omitempty"`
	Address            string   `json:"address,omitempty"` // controller-assigned external address
	TLS                bool     `json:"tls"`               // the Ingress requests an HTTPS certificate
	DNSPointsAtCluster bool     `json:"dns_points_at_cluster"`
	DNSAddresses       []string `json:"dns_addresses,omitempty"`
	Reachable          bool     `json:"reachable"`
	Summary            string   `json:"summary"`
}

type ScaleResult struct {
	App              string `json:"app"`
	PreviousReplicas int32  `json:"previous_replicas"`
	Replicas         int32  `json:"replicas"`
}

// RollbackResult reports the outcome of a rollback. A rollback is itself a forward
// deploy of a prior reference (ADR-0007), so it produces a new Release.
type RollbackResult struct {
	// Release is the new release created by the rollback (carrying the prior
	// reference) and now running.
	Release Release `json:"release"`
	// RolledBackToReleaseID is the prior release whose reference was restored.
	RolledBackToReleaseID string `json:"rolled_back_to_release_id"`
	// SupersededReleaseID is the release that was running before the rollback.
	SupersededReleaseID string `json:"superseded_release_id"`
}
