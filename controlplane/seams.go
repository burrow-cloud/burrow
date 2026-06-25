// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by seams when a requested record or resource does not exist.
// Callers compare with errors.Is; fakes and real adapters both return it (possibly
// wrapped) so engine logic can branch on absence without depending on an adapter.
var ErrNotFound = errors.New("not found")

// The interfaces below are the seams between the control plane's core logic and the
// outside world (ADR-0010). Core logic receives them; it never touches Kubernetes, a
// registry, a database, or the wall clock directly. Tests substitute the in-memory
// fakes in controlplane/internal/fake; production wires real adapters. No method reads
// ambient time or randomness — determinism comes from these injected dependencies.

// Clock is the control plane's only source of time. Injecting it keeps core logic
// deterministic: a release's CreatedAt and any timeouts come from here, never from
// time.Now.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// Resolver is the control plane's DNS lookups, injected so reachability checks stay
// deterministic in tests (ADR-0018). It reports the addresses a hostname resolves to.
type Resolver interface {
	// LookupHost returns the IP addresses host resolves to, or an error (e.g. NXDOMAIN).
	LookupHost(ctx context.Context, host string) ([]string, error)
}

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
	App      string
	Kind     WorkloadKind
	Image    string
	Env      map[string]string
	Command  []string
	Replicas int32
}

// ExposeSpec describes how to make an app reachable at a hostname (ADR-0018). v0.2 routes
// HTTP to the app's Service via an Ingress.
type ExposeSpec struct {
	// App is the application to expose; its workload provides the Service's backends.
	App string
	// Host is the external hostname to route, e.g. app.example.com.
	Host string
	// Port is the app's container port the Service forwards to. Must be positive.
	Port int32
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

// Kubernetes is the seam over the target cluster: the only path from the control plane
// to the runtime. It is deliberately narrow — the v0.1 operations (deploy, status,
// logs, scale, and the delete that supports teardown) and nothing more.
type Kubernetes interface {
	// ApplyWorkload creates or updates the workload for spec.App to match spec.
	ApplyWorkload(ctx context.Context, spec WorkloadSpec) error
	// WorkloadStatus returns the observed state of app's workload, or ErrNotFound if
	// no workload exists for it.
	WorkloadStatus(ctx context.Context, app string) (WorkloadStatus, error)
	// ScaleWorkload sets the desired replica count for app's workload.
	ScaleWorkload(ctx context.Context, app string, replicas int32) error
	// Logs returns recent log lines for app's workload.
	Logs(ctx context.Context, app string, opts LogOptions) ([]LogLine, error)
	// DeleteWorkload removes app's workload. Deleting a missing workload returns
	// ErrNotFound.
	DeleteWorkload(ctx context.Context, app string) error

	// Expose makes app reachable at a hostname by creating (or updating) a Service and an
	// Ingress that routes the host to it (ADR-0018). It does not create the workload —
	// Deploy does — and whether the host is actually reachable also depends on an ingress
	// controller and DNS, which the reachability surface reports on.
	Expose(ctx context.Context, spec ExposeSpec) error
	// Unexpose removes the Service and Ingress created by Expose. Unexposing an app that
	// was never exposed returns ErrNotFound.
	Unexpose(ctx context.Context, app string) error
	// ExposureStatus reports whether app is exposed, at what host, and the external address
	// the ingress controller assigned its Ingress (read from the Ingress's
	// status.loadBalancer — empty until a controller processes it). A never-exposed app
	// returns a zero ExposureStatus and no error.
	ExposureStatus(ctx context.Context, app string) (ExposureStatus, error)
}

// ExposureStatus is the observed state of an app's exposure, for the reachability surface
// (ADR-0018). Address is the controller-assigned external IP or hostname, read from the
// Ingress's status; it is empty until an ingress controller assigns one.
type ExposureStatus struct {
	Exposed bool
	Host    string
	Address string
}

// ImageInfo is what a registry knows about an image reference.
type ImageInfo struct {
	// Reference is the reference that was resolved.
	Reference string
	// Digest is the content digest the reference resolves to (e.g. "sha256:...").
	Digest string
}

// Registry is the seam over the container registry — the conveyor belt that carries
// image bytes the control plane never touches (ADR-0004). The control plane uses it
// only to confirm a referenced image is pullable and to resolve it to a digest for the
// deploy record.
type Registry interface {
	// Resolve returns the registry's view of reference, or ErrNotFound if the image
	// is not present/pullable.
	Resolve(ctx context.Context, reference string) (ImageInfo, error)
}

// Database is the seam over the control plane's own durable state (Postgres in
// production): the deploy records that form the history and the rollback handles
// (ADR-0007). It stores domain Releases; it is independent of cluster state.
type Database interface {
	// SaveRelease persists r. An existing release with the same ID is overwritten
	// (releases are immutable in meaning; this supports status transitions during a
	// single rollout).
	SaveRelease(ctx context.Context, r Release) error
	// Release returns the release with the given ID, or ErrNotFound.
	Release(ctx context.Context, id string) (Release, error)
	// LatestRelease returns the most recently saved release for app, or ErrNotFound
	// if the app has no releases.
	LatestRelease(ctx context.Context, app string) (Release, error)
	// Releases returns all releases for app, oldest first. An app with no releases
	// yields an empty slice and no error.
	Releases(ctx context.Context, app string) ([]Release, error)

	// Policy returns the current guardrail policy: the stored guardrail dispositions
	// overlaid on the built-in defaults (DefaultPolicy), so a store with nothing set
	// returns DefaultPolicy and newly-added guardrails get a sensible default (ADR-0020).
	Policy(ctx context.Context) (Policy, error)
	// SetGuardrail persists the disposition for one guardrail — the write behind
	// `guard set`. It rejects an invalid disposition.
	SetGuardrail(ctx context.Context, code GuardrailCode, d Disposition) error
}
