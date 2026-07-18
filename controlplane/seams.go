// SPDX-License-Identifier: Apache-2.0
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

// RegistryAuth carries optional basic-auth credentials for listing a private repo's tags
// (ADR-0052 §7). The zero value lists anonymously — the case for public GHCR/Docker Hub.
type RegistryAuth struct {
	Username string
	Password string
}

// RegistryClient lists the tags of a container image repository so burrowd can see which
// versions exist and compute what auto-deploy would take (ADR-0052). It is OUTBOUND-only and
// used only for the optional auto-deploy read/watch — never on the core deploy path, which
// stays independent of registry reachability (ADR-0040). It is an OPTIONAL seam: when it is
// not wired the auto-deploy show degrades to reporting the level alone.
type RegistryClient interface {
	// ListTags returns the tags available in the repository named by imageRef (a reference
	// like "ghcr.io/user/app:1.2.3" or the bare repo). auth carries optional basic-auth
	// credentials for a private repo; the zero value lists anonymously. It follows the Docker
	// Registry HTTP API v2 tag-list pagination and the standard Bearer-token auth flow.
	ListTags(ctx context.Context, imageRef string, auth RegistryAuth) ([]string, error)
}

// Builder builds a container image from a git source reference inside the user's own cluster and
// pushes it to a target registry (ADR-0053). It is a seam — a real adapter (a Kubernetes build Job)
// and a fake — like every other Burrow dependency that touches the cluster, the registry, the clock,
// or the database. The interface is deliberately MINIMAL (ADR-0053 §6): it takes a source reference
// and a target image reference and returns the resulting image digest or an error — nothing more.
// Isolation and sandboxing are expressed INSIDE an implementation, never as interface knobs, so the
// separate commercial multi-tenant product can supply a hardened, sandboxed executor behind this same
// seam without the OSS interface having to anticipate its needs (ADR-0053 §6/§7). Building is code
// execution; in the single-tenant OSS path the user owns both the cluster and the source, so no
// sandbox is required (ADR-0053 §7). It is an OPTIONAL seam: nil is allowed, and the build path errors
// cleanly (ErrNotImplemented) when it is not wired.
type Builder interface {
	// Build clones source inside the cluster, builds an image, pushes it to targetImage (a pullable
	// repo:tag reference), and returns the resulting image content digest (e.g. "sha256:..."). Only
	// the git reference and the target reference cross into the builder — never source bytes; the
	// builder clones the actual code from git inside the cluster (ADR-0004/0053 §3). It returns an
	// error on any clone, build, or push failure; the caller surfaces that structurally and does NOT
	// touch the deploy path. On success the returned digest is the immutable identity the resulting
	// guarded deploy pins (ADR-0053 §4).
	//
	// insecure marks the push target as a plain-HTTP registry the push must not verify TLS against —
	// set only for the in-cluster registry, which serves plain HTTP in-cluster (ADR-0054 §5). The
	// engine is the single place that knows this: it sets insecure when it defaults the target to the
	// in-cluster registry, and leaves it false for a caller-supplied external target, which is pushed
	// over TLS. The base-image pull during the build always uses TLS regardless — insecure applies
	// only to the push to targetImage.
	//
	// cred is the resolved source-provider credential (ADR-0057). When it carries a token the builder
	// authenticates the clone to a PRIVATE git source and the buildah push/pull to the provider's
	// registry with it, by mounting it into the build Job — never as a Job env var or command-line
	// argument. The zero value (IsZero) is the public-source, credential-free path. The token is a
	// secret: an implementation must not log it, echo it, or place it in an error.
	Build(ctx context.Context, source SourceRef, targetImage string, insecure bool, cred SourceCredential) (digest string, err error)
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
	// WithNamespace returns a view of this seam whose per-app resource operations (deploy,
	// status, logs, scale, delete, expose/unexpose, and the per-app Secret) act in ns instead
	// of the configured app namespace — the mechanism that routes an operation to a named
	// environment's namespace (ADR-0035 phase 2). Add-on operations are unaffected: add-ons live
	// in their own namespace. An empty ns, or ns equal to the configured app namespace, returns a
	// view equivalent to the receiver, so default-environment behavior is identical to before
	// environments existed.
	WithNamespace(ns string) Kubernetes

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

	// ApplyAutoscaler creates or updates an autoscaling/v2 HorizontalPodAutoscaler named after app,
	// targeting app's Deployment, per spec — the replica band and the CPU (and optional memory)
	// utilization targets (ADR-0006). It is create-or-update: re-applying adjusts the existing HPA.
	// Creating the HPA does not require metrics-server; only its scaling does.
	ApplyAutoscaler(ctx context.Context, app string, spec AutoscaleSpec) error
	// DeleteAutoscaler removes app's HorizontalPodAutoscaler. Deleting an absent HPA is a no-op, not
	// an error, so turning autoscaling off is idempotent.
	DeleteAutoscaler(ctx context.Context, app string) error
	// AutoscalerActive reports whether app has an active HorizontalPodAutoscaler owning its replica
	// count. A workload apply consults it so a deploy (or rollback, or config/secret reapply) leaves
	// the HPA-managed count untouched rather than resetting it. A missing HPA is reported as inactive
	// (false, nil), not an error.
	AutoscalerActive(ctx context.Context, app string) (bool, error)
	// MetricsAPIAvailable reports whether the metrics.k8s.io API group is served (metrics-server is
	// installed), so the engine can warn that an applied HPA will not scale until it is. It is
	// best-effort by contract: the engine treats an error as "absent" and warns rather than failing,
	// so a discovery hiccup never blocks applying the HPA.
	MetricsAPIAvailable(ctx context.Context) (bool, error)
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

	// RunBackupJob runs a one-shot Job in the add-on namespace that pg_dumps app's database on the
	// installed Postgres instance to /<backup-pvc>/<app>/<backupID>.dump (custom format), ensuring
	// the backup PVC first (ADR-0032). The Job connects as the superuser, reading the password from
	// the burrow-postgres Secret via secretKeyRef env — never a CLI argument, never logged. It
	// blocks until the Job completes, returns an error if the Job fails or times out, and reaps the
	// Job on success. It returns the dump's size in bytes when the dump container reported it (the
	// pod's terminated-state message), or 0 when unknown. app is validated before any Job is built.
	RunBackupJob(ctx context.Context, app, backupID string) (sizeBytes int64, err error)
	// RunRestoreJob runs a one-shot Job in the add-on namespace that pg_restores
	// /<backup-pvc>/<app>/<backupID>.dump into app's database (--clean --if-exists, so it replaces
	// current contents). Like RunBackupJob it reads the superuser password only via secretKeyRef,
	// blocks until the Job completes, errors on failure or timeout, and reaps the Job on success.
	// app is validated before any Job is built.
	RunRestoreJob(ctx context.Context, app, backupID string) error

	// RunJob runs spec.Command as a one-shot Job in the app namespace (this seam view's namespace),
	// built from the app's own current image (spec.Image) and its config env plus per-app Secret via
	// envFrom, so DATABASE_URL and every secret resolve as the app sees them (ADR-0048 §2). It blocks
	// until the Job finishes, then captures the pod's output and the container's exit code into a
	// RunResult and returns it. A non-zero exit is a NORMAL structured outcome, not an error: the
	// error return is reserved for a launch, poll, or timeout failure (ADR-0048 §3). The finished Job
	// is garbage-collected by Kubernetes' native ttlSecondsAfterFinished, set from spec.TTLSeconds, so
	// there is no imperative reap (ADR-0048 §7). spec.App is validated before any Job is built.
	RunJob(ctx context.Context, spec RunSpec) (RunResult, error)
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
	// LatestRelease returns the most recently saved release for app in env, or ErrNotFound if
	// the app has no releases there. Releases are keyed per (app, environment) (ADR-0052 Phase 4a):
	// env is the canonical environment name (the reserved "default" for the implicit default
	// environment).
	LatestRelease(ctx context.Context, app, env string) (Release, error)
	// Releases returns all releases for app in env, oldest first, keyed per (app, environment).
	// An app with no releases there yields an empty slice and no error.
	Releases(ctx context.Context, app, env string) ([]Release, error)
	// ListReleases returns all releases for app in env, NEWEST first — the deploy timeline the
	// history surface reads (the same rows deploys already write, read the other way round from
	// Releases). Releases are keyed per (app, environment) (ADR-0052 Phase 4a). An app with no
	// releases there yields an empty slice and no error.
	ListReleases(ctx context.Context, app, env string) ([]Release, error)
	// DeleteReleases removes all release records for app across every environment — the durable
	// side of an app teardown, which removes the whole app. Deleting the releases of an app that
	// has none is a no-op, not an error.
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

	// AutoDeployLevel returns the auto-deploy level configured for app in the named environment
	// (ADR-0052 §2). A missing configuration resolves to DefaultAutoDeployLevel (off): auto-deploy is
	// opt-in, so an app with no stored row is off and is never polled (ADR-0058). env is the canonical
	// environment name (the reserved "default" for the implicit default environment).
	AutoDeployLevel(ctx context.Context, app, env string) (AutoDeployLevel, error)
	// SetAutoDeployLevel upserts the auto-deploy level for app in the named environment — the write
	// behind `burrow app auto-deploy <app> <level>`. It rejects an invalid level. It CLEARS any
	// stored disable reason: a human setting the level is the deliberate re-enable action that
	// removes a rollback or downgrade note (ADR-0052 §5).
	SetAutoDeployLevel(ctx context.Context, app, env string, level AutoDeployLevel) error
	// DisableAutoDeploy sets app's level to off in the named environment AND records why (e.g.
	// "disabled by rollback") — the safety stop of ADR-0052 §5, so the watcher does not fight a
	// deliberate downgrade. It upserts, overwriting any prior level and reason.
	DisableAutoDeploy(ctx context.Context, app, env, reason string) error
	// AutoDeployCandidates returns the distinct (app, environment) pairs the pull-based watcher may
	// reconcile: every app that has a recorded release, paired with the environment it was released
	// into (ADR-0052 Phase 4b). Candidacy is "has a running release" — the set the poller can compare
	// a registry tag against — not "has a stored level row"; the poller reads each pair's level and
	// skips those that are off, which is the default (ADR-0058), so an app that never opted in is read
	// and skipped before any registry call. None yields an empty slice and no error.
	AutoDeployCandidates(ctx context.Context) ([]AppEnvRef, error)
	// AutoDeployReason returns the stored disable reason for app in the named environment, or ""
	// when the level was human-set or is the default (no stored override) — the reason surfaced
	// next to an off level (ADR-0052 §5).
	AutoDeployReason(ctx context.Context, app, env string) (string, error)

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

	// RecordBackup persists a new backup row (ADR-0032). burrowd records it pending before
	// starting the backup Job. The row names the app, the on-PVC path, and the status — never a
	// credential. An existing row with the same ID is overwritten.
	RecordBackup(ctx context.Context, b Backup) error
	// SetBackupStatus updates a recorded backup's status (and, when known, its size) — the
	// completed/failed transition burrowd writes when the Job finishes. Setting the status of an
	// unknown backup id returns ErrNotFound.
	SetBackupStatus(ctx context.Context, id string, status BackupStatus, sizeBytes int64) error
	// ListBackups returns recorded backups, newest first. An empty app lists every app's backups;
	// a non-empty app restricts to that app. No matches yields an empty slice and no error.
	ListBackups(ctx context.Context, app string) ([]Backup, error)
	// GetBackup returns the backup with the given id, or ErrNotFound.
	GetBackup(ctx context.Context, id string) (Backup, error)

	// CreateEnvironment registers a named environment mapping name to namespace (ADR-0035 phase 2).
	// It rejects a duplicate name (the name is the primary key) with an ErrInvalid-wrapped error.
	// The reserved `default` environment is never stored here — it is synthesized by the engine.
	CreateEnvironment(ctx context.Context, name, namespace string) error
	// ListEnvironments returns the registered environments ordered by name. None yields an empty
	// slice and no error. The synthesized `default` environment is not included; the engine
	// prepends it.
	ListEnvironments(ctx context.Context) ([]Environment, error)
	// GetEnvironment returns the registered environment with the given name, or ErrNotFound.
	GetEnvironment(ctx context.Context, name string) (Environment, error)
	// DeleteEnvironment removes the registered environment with the given name, or ErrNotFound when
	// no such environment is registered. The synthesized `default` environment is never stored here,
	// so it is never removed (the engine rejects it before this call).
	DeleteEnvironment(ctx context.Context, name string) error
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
