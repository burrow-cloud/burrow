// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "time"

// The types below are the structured inputs and outputs of the engine's operations.
// Every operation returns a result an agent can reason over — what changed and, where
// relevant, the handle to undo it (ADR-0006) — rather than prose.

// DeployRequest is the small, code-free description of a deploy: a pullable image plus
// metadata (ADR-0004). No code travels here. Env is deliberately absent: an app's
// non-secret config is an independently-managed, app-global store, sourced at apply time
// rather than passed per deploy (ADR-0028) — set it with SetEnv before deploying.
type DeployRequest struct {
	App     string   `json:"app"`
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	// MetricsPort, when positive, annotates the deployed pod so the metrics add-on scrapes
	// /metrics on this container port (ADR-0026). Zero adds no annotations.
	MetricsPort int32 `json:"metrics_port,omitempty"`
	Replicas    int32 `json:"replicas"`
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
	// Confirm acknowledges the app.expose_public guardrail so the operation proceeds past it.
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
	CertReady          bool     `json:"cert_ready"`        // the requested TLS certificate has been issued
	DNSPointsAtCluster bool     `json:"dns_points_at_cluster"`
	DNSAddresses       []string `json:"dns_addresses,omitempty"`
	// Reachable is the converged verdict: every link in the chain is green and the app is live.
	Reachable bool `json:"reachable"`
	// URL is where the app is live when Reachable (https when TLS was requested, else http); it
	// is empty until Reachable.
	URL string `json:"url,omitempty"`
	// BlockedOn names the first unready link when not Reachable (e.g. "ingress controller",
	// "tls certificate", "dns"); it is empty when Reachable. It is the one link to fix next.
	BlockedOn string `json:"blocked_on,omitempty"`
	Summary   string `json:"summary"`
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

// AddProviderRequest registers a vendor credential (ADR-0023, ADR-0030). The token VALUE travels
// in this request over burrowd's authenticated, TLS-protected control-plane API; burrowd validates
// it and then writes it into the burrow-credentials Secret. The value is never logged, never stored
// in Postgres, never returned in a response, and still never carried over MCP — provider add is a
// human/CLI operation, not an agent one.
type AddProviderRequest struct {
	// Name identifies the provider; empty defaults to the type.
	Name string `json:"name,omitempty"`
	// Type is the vendor this provider talks to.
	Type ProviderType `json:"type"`
	// SecretKey is the key in burrow-credentials the token is written under; empty defaults to Name.
	SecretKey string `json:"secret_key,omitempty"`
	// Token is the vendor API token VALUE. It is written to burrow-credentials after validation and
	// is never logged, stored in Postgres, or echoed back (ADR-0030).
	Token string `json:"token,omitempty"`
}

// AddDomainRequest points a host at an address through a configured DNS provider (ADR-0018).
// The address is the cluster's external entry point — the ingress controller's IP or hostname.
// Supply it explicitly with Address, or name an exposed App and the control plane reads the
// controller-assigned address from that app's Ingress (the value `burrow app reachability` reports).
type AddDomainRequest struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	// Address is the external IP or hostname to point Host at. Optional when App is set.
	Address string `json:"address,omitempty"`
	// App is an exposed application whose ingress external address Host should point at, used
	// when Address is empty so the agent need not look the address up itself.
	App string `json:"app,omitempty"`
	// Confirm acknowledges the dns.write guardrail so the operation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// RemoveDomainRequest removes the DNS record a provider holds for a host (ADR-0018).
type RemoveDomainRequest struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	// Confirm acknowledges the dns.delete guardrail so the operation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// DomainResult reports the DNS record a domain operation created, updated, or removed.
type DomainResult struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	Type     string `json:"type,omitempty"`
	Address  string `json:"address,omitempty"`
}

// BackupStatus is the lifecycle state of a recorded Postgres backup (ADR-0032). A backup is
// recorded pending when its Job is started and moves to completed on the Job's success or failed
// on its failure.
type BackupStatus string

const (
	// BackupPending is a backup whose Job has been started but has not yet completed.
	BackupPending BackupStatus = "pending"
	// BackupCompleted is a backup whose Job finished successfully; the dump is on the PVC.
	BackupCompleted BackupStatus = "completed"
	// BackupFailed is a backup whose Job did not complete successfully.
	BackupFailed BackupStatus = "failed"
)

// Valid reports whether s is a known BackupStatus.
func (s BackupStatus) Valid() bool {
	switch s {
	case BackupPending, BackupCompleted, BackupFailed:
		return true
	default:
		return false
	}
}

// Backup is one recorded per-app database backup (ADR-0032): the control-plane index row for a
// logical dump written to the backup PVC. It names the app, the on-PVC path, the byte size (0 when
// unknown), and the lifecycle status — never a credential or a connection string. burrowd is not
// mounted to the backup PVC, so this row, not the volume, is what `addon backups` lists.
type Backup struct {
	// ID is the backup identifier, minted from the IDs seam — also the dump filename stem.
	ID string `json:"id"`
	// App is the application whose database was dumped.
	App string `json:"app"`
	// CreatedAt is when the backup was recorded, read from the injected clock.
	CreatedAt time.Time `json:"created_at"`
	// Path is the on-PVC location of the dump (e.g. /backups/<app>/<id>.dump). It is a path on a
	// volume burrowd does not mount — never a credential.
	Path string `json:"path,omitempty"`
	// SizeBytes is the dump's size in bytes, or 0 when unknown.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Status is the lifecycle state of this backup.
	Status BackupStatus `json:"status"`
}

// BackupResult reports the outcome of an on-demand backup (ADR-0032): the recorded backup row. It
// carries the backup id, the app, the path, the size, and the status — never a secret value.
type BackupResult struct {
	Backup Backup `json:"backup"`
}

// BackupPath is the on-PVC path of app's dump for id, recorded in the backup row and used by the
// kube Job builder so both sides agree on the layout without the engine importing the kube package
// (ADR-0032). It is a path on a volume burrowd does not mount — never a credential.
func BackupPath(app, id string) string {
	return backupMountPath + "/" + app + "/" + id + ".dump"
}

// backupMountPath is where the backup PVC is mounted inside the backup/restore Job container; it
// prefixes every recorded backup path so the engine and the kube Job builder agree on the layout.
const backupMountPath = "/backups"
