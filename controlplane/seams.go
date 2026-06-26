// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"time"
)

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

// Kubernetes is the seam over the target cluster: the only path from the control plane
// to the runtime. It is deliberately narrow — the v0.1 operations (deploy, status,
// logs, scale, and the delete that supports teardown) and nothing more.
type Kubernetes interface {
	// ApplyWorkload creates or updates the workload for spec.App to match spec.
	ApplyWorkload(ctx context.Context, spec WorkloadSpec) error
	// WorkloadStatus returns the observed state of app's workload, or ErrNotFound if
	// no workload exists for it.
	WorkloadStatus(ctx context.Context, app string) (WorkloadStatus, error)
	// ListWorkloads returns the observed state of every Burrow-managed workload in the
	// namespace (for an apps listing). No workloads is an empty slice, not an error.
	ListWorkloads(ctx context.Context) ([]WorkloadStatus, error)
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

	// SaveProvider upserts a provider in the registry by name (ADR-0023). It stores only
	// the non-secret registry entry — type, capabilities, and the key under which the token
	// lives in the burrow-credentials Secret — never the token itself.
	SaveProvider(ctx context.Context, p Provider) error
	// Provider returns the provider with the given name, or ErrNotFound.
	Provider(ctx context.Context, name string) (Provider, error)
	// Providers returns all configured providers, name order. None yields an empty slice
	// and no error.
	Providers(ctx context.Context) ([]Provider, error)
}

// Credentials is the seam over the one burrow-credentials Secret that holds every vendor
// token (ADR-0023). The control plane reads a provider's token through it at call time, so a
// rotation is picked up with no restart. It is the only path by which the control plane reads
// a Secret's contents, and the production adapter is scoped to that single object — burrowd's
// least-privilege Role grants `get` on exactly burrow-credentials and nothing else.
type Credentials interface {
	// Token returns the token stored under key in burrow-credentials, or ErrNotFound when
	// the Secret or the key is absent.
	Token(ctx context.Context, key string) (string, error)
}

// DNSProvider is the seam over a single vendor's DNS API, holding one provider's token
// (ADR-0018, ADR-0023). burrowd is the only thing that talks to the vendor — the agent never
// holds the token and never calls the API directly. Writes are scoped to the zones the
// provider manages: an operation on a host no managed zone covers returns ErrNotFound.
type DNSProvider interface {
	// VerifyAccess confirms the token authenticates and can manage DNS, with a cheap read
	// call against the vendor. It returns ErrInvalid when the vendor rejects the token.
	VerifyAccess(ctx context.Context) error
	// EnsureRecord creates or updates the record so r.Name resolves to r.Value (ADR-0018). It
	// is idempotent: re-applying the same record is a no-op. It returns ErrNotFound when no
	// zone the provider manages covers r.Name.
	EnsureRecord(ctx context.Context, r DNSRecord) error
	// DeleteRecord removes the A/CNAME record(s) the provider holds for host. It returns
	// ErrNotFound when no managed zone covers host or no such record exists.
	DeleteRecord(ctx context.Context, host string) error
}

// DNSFactory builds a DNSProvider for a vendor type and token (ADR-0023). It is the seam that
// lets the engine reach a vendor without importing its adapter: production maps each
// ProviderType to its adapter (controlplane/dns), and tests substitute a fake. It returns
// ErrNotImplemented for a type no adapter serves.
type DNSFactory interface {
	DNS(t ProviderType, token string) (DNSProvider, error)
}
