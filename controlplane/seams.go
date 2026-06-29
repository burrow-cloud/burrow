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

// LogEntry is one record returned by a logs query.
type LogEntry struct {
	Time    string `json:"time,omitempty"`
	Message string `json:"message"`
	Pod     string `json:"pod,omitempty"`
}

// LogsQuerier queries a logs backing service (an installed or connected add-on) for records
// matching a query, so the agent can answer "what happened? / why is it slow?" (ADR-0026). It is
// an optional seam — present only when logs querying is wired; the engine errors cleanly if not.
type LogsQuerier interface {
	// QueryLogs runs query against the logs store reachable at endpoint (an in-cluster
	// host:port) and returns up to limit matching records, most recent first. token is a bearer
	// credential for an authenticated backend; an empty token means unauthenticated and no
	// Authorization header is sent.
	QueryLogs(ctx context.Context, endpoint, query string, limit int, token string) ([]LogEntry, error)
}

// MetricSample is one sample returned by a metrics query. Value is the metric's value as a string so
// PromQL's exact numeric formatting (precision, NaN/Inf) is preserved rather than lost to a float
// round-trip.
type MetricSample struct {
	Labels map[string]string `json:"labels,omitempty"`
	Value  string            `json:"value"`
	Time   string            `json:"time,omitempty"`
}

// MetricsQuerier runs an instant PromQL query against a Prometheus-API-compatible metrics store (an
// installed or connected add-on) so the agent can answer "how is my app performing? / what's the CPU,
// memory, or error rate?" (ADR-0026). It is an optional seam — present only when metrics querying is
// wired; the engine errors cleanly if not.
type MetricsQuerier interface {
	// QueryMetrics runs an instant PromQL query against the Prometheus-API-compatible store reachable
	// at endpoint (an in-cluster host:port) and returns the matching samples. token is a bearer
	// credential for an authenticated backend; an empty token means unauthenticated and no
	// Authorization header is sent.
	QueryMetrics(ctx context.Context, endpoint, query string, token string) ([]MetricSample, error)
}

// DatabaseProvisioner is the seam over the installed Postgres add-on's admin surface (ADR-0031).
// burrowd connects to the shared instance as the superuser and gives each app its own database and
// login role inside it; the engine calls this on attach/detach. It is an optional seam — present
// only when the Postgres add-on path is wired; the engine errors cleanly (ErrNotImplemented) on an
// attach when it is nil. The connection string it returns is a secret VALUE: it is handed only to
// SetSecretValue and never logged, audited, returned, or carried over MCP (ADR-0029/0031).
type DatabaseProvisioner interface {
	// EnsureAppDatabase idempotently provisions an isolated database and login role for app on the
	// shared instance and returns the app's DATABASE_URL (a postgres:// connection string carrying
	// a freshly generated role password). It rotates the role password on every call, so a re-attach
	// returns a fresh, working URL with no orphaned state. The app name is validated against a strict
	// identifier pattern and every SQL identifier is quoted BEFORE any SQL runs, so a name can never
	// carry SQL. The returned string is a secret value — the caller writes it straight into the app's
	// Secret and never logs, audits, or returns it.
	EnsureAppDatabase(ctx context.Context, app string) (databaseURL string, err error)
	// DropAppDatabase removes app's database and login role from the shared instance — the
	// destructive side of detach. Dropping a database/role that is already absent is a no-op, not an
	// error. The app name is validated before any SQL, exactly as in EnsureAppDatabase.
	DropAppDatabase(ctx context.Context, app string) error
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

	// DeployAddon installs a building-block backing service per spec — a workload, a
	// ClusterIP Service, and a persistent volume when the spec asks for one — and returns
	// the instance's connection info (ADR-0025). Installing an already-installed add-on is
	// idempotent.
	DeployAddon(ctx context.Context, spec AddonSpec) (AddonInfo, error)
	// AddonReady reports whether the named add-on's backing Deployment is available. It is a
	// cheap single-Deployment readiness probe — readiness is a live property, not stored in the
	// registry. A missing Deployment is reported as not ready (false, nil), not an error.
	AddonReady(ctx context.Context, name string) (bool, error)
	// DeleteAddon removes the named add-on instance and its resources. Removing an add-on
	// that is not installed returns ErrNotFound.
	DeleteAddon(ctx context.Context, name string) error
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

	// SecretKeys returns the env-var names held in app's per-app Secret
	// (burrow-app-<app>-secrets), sorted, never the values (ADR-0028/0004). A missing
	// Secret yields an empty slice and no error — an app with no secrets set.
	SecretKeys(ctx context.Context, app string) ([]string, error)
	// SetSecretValue upserts one key=value into app's per-app Secret, creating the
	// Secret if absent (ADR-0029). The value arrives over burrowd's authenticated
	// control-plane API and is written here to the Kubernetes Secret — it is NEVER
	// logged, never audited (the audit log records the key name only), never stored in
	// Postgres, and never carried over MCP. Any error this returns must name the app and
	// key only, never the value.
	SetSecretValue(ctx context.Context, app, key, value string) error
	// UnsetSecretKey removes one key from app's per-app Secret. A missing Secret or a
	// missing key is a no-op, not an error — unsetting what is already absent succeeds.
	// The value never crosses this seam: only the key name does.
	UnsetSecretKey(ctx context.Context, app, key string) error
	// RestartWorkload triggers a rolling update of app's Deployment by bumping the
	// pod-template annotation burrow.cloud/restarted-at to at. It is how a secret change
	// (read only at pod start via envFrom) forces the running app to pick it up. A missing
	// Deployment returns ErrNotFound; the caller treats that as "nothing running to roll".
	RestartWorkload(ctx context.Context, app string, at time.Time) error
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
	// DeleteReleases removes all release records for app — the durable side of an app
	// teardown. Deleting the releases of an app that has none is a no-op, not an error.
	DeleteReleases(ctx context.Context, app string) error

	// AppEnv returns the non-secret environment store for app: the app-global current
	// config rendered into the workload at apply time (ADR-0028). An app with no env yields
	// an empty map and no error.
	AppEnv(ctx context.Context, app string) (map[string]string, error)
	// SetAppEnv upserts one env key for app in the store.
	SetAppEnv(ctx context.Context, app, key, value string) error
	// UnsetAppEnv removes one env key for app from the store. Removing a key that is not set
	// is a no-op, not an error.
	UnsetAppEnv(ctx context.Context, app, key string) error

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

	// SaveAddon upserts an add-on in the registry by name (ADR-0025). It stores the non-secret
	// registry entry — type, mode, backend, endpoint, and capabilities — never the live
	// readiness, which is probed from the cluster.
	SaveAddon(ctx context.Context, a AddonInfo) error
	// Addon returns the add-on with the given name, or ErrNotFound.
	Addon(ctx context.Context, name string) (AddonInfo, error)
	// Addons returns all registered add-ons, name order. None yields an empty slice and no
	// error. The returned entries carry no live readiness.
	Addons(ctx context.Context) ([]AddonInfo, error)
	// DeleteAddon removes the named add-on from the registry, or ErrNotFound if absent.
	DeleteAddon(ctx context.Context, name string) error

	// AppendAudit appends one audit row (ADR-0027). The log is append-only: there is no
	// update or delete path. The store assigns the row identity and orders rows by it.
	AppendAudit(ctx context.Context, entry AuditEntry) error
	// Audit returns audit rows matching filter, newest first, capped by filter.Limit (a
	// store default when unset). No matches yields an empty slice and no error.
	Audit(ctx context.Context, filter AuditFilter) ([]AuditEntry, error)
}

// Credentials is the seam over the one burrow-credentials Secret that holds every vendor
// token (ADR-0023, ADR-0030). The control plane reads a provider's token through it at call
// time, so a rotation is picked up with no restart, and writes a token value it received over
// its authenticated control-plane API. It is the only path by which the control plane reads or
// writes a Secret's contents, and the production adapter is scoped to that single object —
// burrowd's least-privilege Role grants `get` and `update` on exactly burrow-credentials and
// nothing else.
type Credentials interface {
	// Token returns the token stored under key in burrow-credentials, or ErrNotFound when
	// the Secret or the key is absent.
	Token(ctx context.Context, key string) (string, error)
	// SetToken upserts key=value into burrow-credentials, creating the Secret if absent
	// (ADR-0030). The value arrives over burrowd's authenticated, TLS-protected control-plane
	// API — never over MCP — and is written straight to the Secret: it is NEVER logged, never
	// stored in Postgres, and never returned in a response. Any error names the key only, never
	// the value.
	SetToken(ctx context.Context, key, value string) error
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
