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
	// Readiness is the readiness probe to author on the container (ADR-0076 §1-§3). The zero value
	// means NO PROBE, which is what an app whose port Burrow does not know gets — behaviour
	// identical to before probes existed. The engine resolves it with ResolveReadiness on every
	// apply, so a deploy, a rollback, and a config reapply all author the same probe.
	//
	// There is no liveness field here and there is not going to be one by default (§1): readiness
	// takes a pod out of service reversibly, liveness restarts the container and manufactures the
	// crash loop it was meant to detect.
	Readiness ReadinessCheck
	Replicas  int32
	// SecretFiles is the set of the app's secret KEYS that are projected into files, and the one
	// directory they land in (ADR-0089 §1-§2). It is this type's first field that is not about the
	// container's code, and it is what the app PodSpec's first Volumes entry is built from.
	//
	// It carries key names and filenames and NEVER a value: the value stays in the per-app Secret,
	// and the pod template gains a KeyToPath reference to it (ADR-0029 holds unchanged). The zero
	// value adds no volume, no volume mount and no BURROW_SECRETS_DIR, so an app that mounts nothing
	// gets the pod template it had before mounts existed.
	//
	// Like Env and Readiness it is CURRENT STATE rather than a snapshot on the release: the engine
	// reads it on every apply, so a rollback keeps the files the running code needs rather than
	// taking them back to whatever was mounted when the older release was cut (ADR-0089 §5).
	SecretFiles SecretMounts
	// SecretEnvKeys is the app's secret keys that still reach the container as ENVIRONMENT VARIABLES,
	// for an app that marked at least one key file-only (ADR-0089 §4). It is read only when
	// SecretFiles says so, and it is nil for every other app — which is what keeps their pod template
	// bit-for-bit what it was, sourcing the whole Secret through envFrom.
	//
	// It exists because envFrom cannot exclude a key. The only way to keep one out of the environment
	// is to stop sourcing the Secret wholesale and name the rest, and the price of that is that a NEW
	// key becomes a pod-template change rather than something a restart picks up.
	SecretEnvKeys []string
	// ReleaseID is the release this workload is applying. It is stamped on the pod template (under
	// ReleaseAnnotation) so a new release always rolls the workload, even when the image reference
	// is unchanged; re-applying the same release reuses the same ID, so the apply stays idempotent.
	// Empty adds no annotation.
	ReleaseID string
}

// RunSpec is the desired one-off command Job the kube seam builds for RunJob (ADR-0048): the app's
// own current image and config env, plus the caller's command and the finished-Job TTL. The per-app
// Secret is injected by the adapter via envFrom, not carried here, so no secret value crosses this
// seam. No code travels here (ADR-0004) — the command names an entrypoint already in the image.
type RunSpec struct {
	App string
	// ID is the unique run identifier the Job name derives from (burrow-run-<ID>), minted by the
	// engine's ID seam so the Job name is deterministic in tests.
	ID string
	// Image is the app's currently-deployed image the command runs in (ADR-0048 §2).
	Image string
	// Command is the command and its arguments run as the container's command.
	Command []string
	// Env is the app's non-secret config, sourced as container env alongside the per-app Secret.
	Env map[string]string
	// TTLSeconds is the Job's ttlSecondsAfterFinished — how long the finished Job lingers before
	// Kubernetes garbage-collects it (ADR-0048 §7). Zero deletes it as soon as it finishes.
	TTLSeconds int32
	// Probe, when set, asks the adapter to make Burrow's own binary executable inside this Job's
	// container before Command runs: an init container from Burrow's image copies its own executable
	// into an emptyDir that the app's container mounts at ProbeMountPath (ADR-0076 §4).
	//
	// It exists because the deploy-time dependency check has to run in the APP's image — the app's
	// filesystem, service account, network policy and credential are where misconfiguration lives —
	// and that image may contain no shell, no psql and no curl, which is exactly the minimal image
	// users are told to build. nil (the default) is an ordinary run and authors nothing extra.
	//
	// No code travels here either (ADR-0004): the init container's image is Burrow's own published
	// image, named by the adapter, and the only thing that moves is a reference to it.
	Probe *ProbeSpec
	// SecretFiles and SecretEnvKeys are the app's secret projection, carried here for the same
	// reason Image and Env are: a Job runs the app's own image with the app's own environment
	// (ADR-0048 §2), and the app's environment is now two doors rather than one.
	//
	// A run is where this matters MOST. An environment variable is inherited by every child process,
	// and a run is what starts a shell; a Job that sourced the Secret wholesale would put a key the
	// app marked file-only (ADR-0089 §4) back exactly where the app took it out of. Carrying the
	// projection means the Job reads it as the file it is.
	SecretFiles   SecretMounts
	SecretEnvKeys []string
}

// ProbeSpec is the extra a Job needs to run Burrow's probe inside the app's image (ADR-0076 §4).
type ProbeSpec struct {
	// Env is the probe's OWN configuration, applied to the check container after the app's config so
	// it cannot be shadowed by an app that happens to set the same key. It is non-secret by
	// construction — the plan carries environment variable NAMES and in-cluster addresses Burrow
	// composed, never a value — which is what makes it safe in a Job spec.
	Env map[string]string
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
	// blocked, when the cluster reports a genuinely blocking pod condition — a pull failure that
	// names the image, the registry host and the `burrow config registry login` fix (ADR-0006); a
	// scheduling failure that names the taint or the resource; a crash loop that names the exit
	// code (ADR-0074 §2). It is best-effort enrichment: empty when the workload is healthy or when
	// no blocking condition was observed, so it never becomes a required field.
	//
	// It never contains a secret VALUE. A missing config or secret key is named, because the key is
	// the actionable part; a crash-loop log tail is the application's own output, bounded and
	// labelled as such (ADR-0074 §9).
	Issue string `json:"issue,omitempty"`
	// IssueReason is the machine-usable reason behind Issue — a member of the closed set
	// IssueReasons() enumerates (e.g. "ImagePullBackOff", "Unschedulable", "CrashLoopBackOff") —
	// for an agent that wants to branch on the cause rather than parse the prose (ADR-0074 §5).
	// Each value is the raw Kubernetes reason string wherever Kubernetes has one. Empty whenever
	// Issue is empty.
	IssueReason string `json:"issue_reason,omitempty"`
	// Locked reports whether this app carries a lock, so deleting it refuses until somebody unlocks
	// it (cloud ADR-0060). It is CONTROL-PLANE state filled in by the engine, not something read
	// from the cluster: the Kubernetes seam never sets it, and a workload the engine did not enrich
	// reports false. It rides here so lock state is visible wherever an app is listed rather than
	// discoverable only by attempting to destroy it.
	Locked bool `json:"locked,omitempty"`
}

// WorkloadEventKind names what a workload watch is reporting (ADR-0079 §1). Two of the four kinds
// are about the WATCH rather than about a workload, and that is the point: a consumer that cannot
// tell "nothing broke" from "I stopped looking" is the failure ADR-0074's coverage record exists to
// prevent, and a watch that reconnects silently produces exactly it.
type WorkloadEventKind string

const (
	// WorkloadChanged carries one workload's current observed state. It is the same derivation
	// ListWorkloads returns — see WatchWorkloads for the one deliberate difference — so a ledger row
	// and a `burrow app status` answer cannot disagree about one pod.
	WorkloadChanged WorkloadEventKind = "changed"
	// WorkloadGone reports that the workload is no longer in the cluster; only Status.App is set. It
	// is NOT ADR-0074 §6's absence diagnosis, which is a comparison against the registry and stays
	// the periodic pass's — it is what makes the conditions latched against that workload clear.
	WorkloadGone WorkloadEventKind = "gone"
	// WorkloadSynced reports that the watch has delivered a complete current picture of its
	// namespace: every workload in it has been reported since the watch was established, or since it
	// last re-listed, so from here an absence of events is an absence of change. It is what RESUMES
	// coverage (ADR-0079 §4).
	WorkloadSynced WorkloadEventKind = "synced"
	// WorkloadDropped reports that the watch lost its place. Between it and the next WorkloadSynced
	// the observer saw nothing, and a failure that started and ended in that stretch is invisible —
	// so it ENDS coverage, exactly as a burrowd restart does (ADR-0079 §4).
	WorkloadDropped WorkloadEventKind = "dropped"
)

// WorkloadEvent is one thing a workload watch has to say: about a workload, or about itself.
type WorkloadEvent struct {
	// Kind is what this event reports.
	Kind WorkloadEventKind
	// Namespace is the namespace the watch that produced it covers. It is on the event rather than
	// implied by the channel because several watches deliver on one channel, and the consumer has to
	// know which of them just dropped.
	Namespace string
	// Status is the workload's observed state on WorkloadChanged, and carries only App on
	// WorkloadGone. It is unset on the two events that are about the watch.
	Status WorkloadStatus
	// Detail is one line saying why a watch dropped, empty on every other kind. It is for a log line,
	// not for a ledger row.
	Detail string
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
	Pod string `json:"pod"`
	// Timestamp is the instant the cluster recorded the line, in UTC. It is zero only when no
	// time could be read for the line at all — a malformed or partial record at the start of a
	// pod's stream — so a zero value means "unknown", not "not provided".
	Timestamp time.Time `json:"timestamp"`
	// Message is the application's own output, with any timestamp the cluster added stripped off.
	Message string `json:"message"`
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
