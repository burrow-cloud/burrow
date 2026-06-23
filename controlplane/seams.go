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

// DeploymentSpec is the desired state of one App's Kubernetes workload — the small,
// code-free description a deploy turns into (ADR-0004): a pullable image plus metadata.
type DeploymentSpec struct {
	App      string
	Image    string
	Env      map[string]string
	Command  []string
	Replicas int32
}

// DeploymentStatus is the observed state of an App's workload, as reported by the
// cluster.
type DeploymentStatus struct {
	App             string
	Image           string
	DesiredReplicas int32
	ReadyReplicas   int32
	UpdatedReplicas int32
	// Available reports whether the workload currently meets its availability
	// condition (enough ready replicas to serve).
	Available bool
}

// LogOptions selects which log lines to return.
type LogOptions struct {
	// TailLines bounds how many of the most recent lines to return. Zero means an
	// adapter-defined default.
	TailLines int
}

// LogLine is a single line of application log output.
type LogLine struct {
	Pod       string
	Timestamp time.Time
	Message   string
}

// Kubernetes is the seam over the target cluster: the only path from the control plane
// to the runtime. It is deliberately narrow — the v0.1 operations (deploy, status,
// logs, scale, and the delete that supports teardown) and nothing more.
type Kubernetes interface {
	// ApplyDeployment creates or updates the workload for spec.App to match spec.
	ApplyDeployment(ctx context.Context, spec DeploymentSpec) error
	// DeploymentStatus returns the observed state of app's workload, or ErrNotFound
	// if no workload exists for it.
	DeploymentStatus(ctx context.Context, app string) (DeploymentStatus, error)
	// ScaleDeployment sets the desired replica count for app's workload.
	ScaleDeployment(ctx context.Context, app string, replicas int32) error
	// Logs returns recent log lines for app's workload.
	Logs(ctx context.Context, app string, opts LogOptions) ([]LogLine, error)
	// DeleteDeployment removes app's workload. Deleting a missing workload returns
	// ErrNotFound.
	DeleteDeployment(ctx context.Context, app string) error
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
}
