// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"io"
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

// MetricPoint is one sample in a time series: a timestamp and the metric's value at it. Value is a
// string for the same reason MetricSample.Value is — PromQL's exact numeric formatting (precision,
// NaN/Inf) is preserved rather than lost to a float round-trip. Time is the timestamp as the
// Prometheus API returns it, formatted the same way MetricSample.Time is.
type MetricPoint struct {
	Time  string `json:"time,omitempty"`
	Value string `json:"value"`
}

// MetricSeries is one labelled time series returned by a range query: a label set and its ordered
// points over the queried window. It is the time-series sibling of MetricSample, which carries a
// single instant sample.
type MetricSeries struct {
	Labels map[string]string `json:"labels,omitempty"`
	Points []MetricPoint     `json:"points"`
}

// MetricsRangeQuerier runs a PromQL range query against a Prometheus-API-compatible metrics store and
// returns time series over a window — the sibling of MetricsQuerier's instant query, for sparklines
// and area charts rather than a point-in-time read (ADR-0026). It is kept as a SEPARATE optional
// interface rather than folded into MetricsQuerier so an existing instant-only implementer stays
// valid unchanged: the engine type-asserts a selected metrics querier to it and errors cleanly
// (ErrNotImplemented) when a backend does not provide the range surface.
type MetricsRangeQuerier interface {
	// QueryMetricsRange runs a PromQL range query against the Prometheus-API-compatible store
	// reachable at endpoint (an in-cluster host:port) over [start, end] sampled every step, and
	// returns one MetricSeries per matching series. token is a bearer credential for an
	// authenticated backend; an empty token means unauthenticated and no Authorization header is
	// sent.
	QueryMetricsRange(ctx context.Context, endpoint, query, token string, start, end time.Time, step time.Duration) ([]MetricSeries, error)
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

// DatabaseProvisioner is the seam over an installed Postgres add-on instance's admin surface
// (ADR-0031). burrowd connects to the environment's instance as the superuser and gives each app its
// own database and login role inside it; the engine calls this on attach/detach. It is an optional
// seam — present only when the Postgres add-on path is wired; the engine errors cleanly
// (ErrNotImplemented) on an attach when it is nil. The connection string it returns is a secret
// VALUE: it is handed only to SetSecretValue and never logged, audited, returned, or carried over
// the agent control channel (ADR-0029/0031).
//
// EVERY METHOD TAKES THE ENVIRONMENT, AND IT IS NOT OPTIONAL (ADR-0067 §1). Databases keep their
// simple names — an app called web has a database called web — so the environment is the only thing
// that distinguishes web-in-staging from web-in-production. Without it, provisioning web a second
// time found the first one, and because provisioning is IDEMPOTENT it did not fail: it rotated the
// role password and handed back a URL pointing at the other environment's data (issue #339). The
// environment selects the INSTANCE, so an implementation cannot reach another environment's server
// at all; an empty environment is rejected before any SQL, because "a signature that can omit it is
// a signature that will omit it".
type DatabaseProvisioner interface {
	// EnsureAppDatabase idempotently provisions an isolated database and login role for app on
	// environment env's own instance and returns the app's DATABASE_URL (a postgres:// connection
	// string carrying a freshly generated role password). It rotates the role password on every call,
	// so a re-attach returns a fresh, working URL with no orphaned state. env and app are both
	// validated against a strict identifier pattern and every SQL identifier is quoted BEFORE any SQL
	// runs, so neither can carry SQL; an empty env is ErrInvalid, never the default environment. The
	// returned string is a secret value — the caller writes it straight into the app's Secret and
	// never logs, audits, or returns it.
	EnsureAppDatabase(ctx context.Context, app, env string) (databaseURL string, err error)
	// DropAppDatabase removes app's database and login role from environment env's instance — the
	// destructive side of detach. Dropping a database/role that is already absent is a no-op, not an
	// error. env and app are validated before any SQL, exactly as in EnsureAppDatabase, so a detach
	// can no more reach another environment's server than an attach can.
	DropAppDatabase(ctx context.Context, app, env string) error
}

// AppDatabaseLister enumerates the apps that hold a Burrow-provisioned database on one environment's
// Postgres instance — the concrete answer to "who is attached?", and therefore to "whose data does
// removing this add-on destroy?". The engine reads it so a removal that would destroy the volume can
// name the affected apps in its confirmation message rather than warning generically (ADR-0006:
// a gate is only as good as the reason it hands back).
//
// It is a SEPARATE optional interface rather than a method on DatabaseProvisioner, for the same
// reason MetricsRangeQuerier is separate from MetricsQuerier: an existing provisioner implementation
// stays valid unchanged. The engine type-asserts its provisioner to it and degrades — reporting no
// attached apps rather than failing — when it is absent or the instance cannot be reached. That
// degradation is deliberate: an add-on is often removed precisely because it is wedged, and a
// removal must not become impossible when the thing being removed will not answer.
type AppDatabaseLister interface {
	// ListAppDatabases returns the app names with a Burrow-provisioned database on environment env's
	// instance, sorted. None yields an empty slice and no error. env is required for the same reason
	// it is on DatabaseProvisioner: the answer to "who is attached?" is only true of one instance,
	// and a removal names the apps it is about to affect (ADR-0067 §1).
	ListAppDatabases(ctx context.Context, env string) ([]string, error)
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

	// AwaitRollout blocks until app's newest revision has SETTLED — the completion test
	// `kubectl rollout status` uses, not merely that the write was accepted — or until it can say why
	// it did not, bounded by timeout (ADR-0072 §4-§5). It is what makes a `post-deploy` hook possible:
	// a hook cannot be told how a deploy went until something waits for it to go.
	//
	// EVERY OBSERVABLE CONDITION IS AN OUTCOME, NOT AN ERROR, exactly as a non-zero exit is a result
	// rather than an error on RunJob (ADR-0048 §3). A crash loop, an unschedulable pod, a missing
	// workload and an expired deadline all come back as a RolloutOutcome with a reason from the closed
	// vocabulary, because the caller's next move is the same in every case: tell the hook. The error
	// return is reserved for a call that could not be made at all (an invalid app identifier).
	//
	// It returns as soon as it has a verdict rather than sleeping out its bound: a pod reporting a
	// blocking condition ends the wait immediately, which is the difference between a hook told
	// "CrashLoopBackOff" in fifteen seconds and one told "timed out" in five minutes (issue #352).
	// An expired bound is the BACKSTOP, and its outcome carries what was observed rather than the
	// elapsed time (ADR-0072 §5).
	//
	// It mutates nothing. A rollout it reports as failed is left exactly as it is, for the hook to
	// decide about — Burrow does not roll back by itself (ADR-0072 §6).
	AwaitRollout(ctx context.Context, app string, timeout time.Duration) (RolloutOutcome, error)

	// DeployAddon installs a building-block backing service per spec for environment env — a
	// workload, a ClusterIP Service, and a persistent volume when the spec asks for one — and returns
	// the instance's connection info (ADR-0025). Installing an already-installed add-on is
	// idempotent.
	//
	// env is required and names which environment's instance this is (ADR-0067 §1): the resources are
	// named by AddonInstanceName, so the default environment lands on exactly the names an existing
	// install already has and any other environment gets its own instance beside it — its own pod,
	// its own volume, and for Postgres its own superuser credential.
	//
	// The add-on's TYPE decides what is written. Postgres is a CloudNativePG `Cluster` — one custom
	// resource, from which the operator composes the workload, the volume and the services (ADR-0066
	// §1) — and every other add-on is a Deployment Burrow authors. Which one it is follows from the
	// spec; there is no mechanism to select.
	//
	// archive, when non-nil, is the pgBackRest repository this instance archives its write-ahead log
	// and takes its base backups to (ADR-0066 §3). It applies to Postgres and is ignored by every
	// other add-on. Passing it is what wires the plugin into the instance, so an instance created
	// with a nil archive has no archiving at all and physical backups of it are refused by name; a
	// later install with one wires it, because a destination registered after the instance was
	// created is a normal sequence and not a reason to rebuild the database. Like BackupDestination
	// it carries a credential PAIR and therefore never crosses an API boundary — the adapter puts it
	// in a Secret the plugin's sidecar reads by reference and nowhere else.
	DeployAddon(ctx context.Context, spec AddonSpec, env string, archive *ArchiveDestination) (AddonInfo, error)
	// AddonReady reports whether the named add-on's backing workload is available. It is a cheap
	// single-object readiness probe — readiness is a live property, not stored in the registry — and
	// a missing workload is reported as not ready (false, nil), not an error.
	//
	// WHICH object it reads is resolved from the cluster rather than from the name, because an add-on
	// instance is not always a Deployment: a Postgres instance has none at all, and its readiness is
	// its CloudNativePG `Cluster`'s ready instance count (ADR-0066 §1). Reading only the Deployment
	// would report every Postgres instance as not running, and the failure observer would then open
	// an ADR-0074 §6 discrepancy row against a database that is serving perfectly well — a false
	// absence, which is precisely the diagnosis §6 exists to make correctly.
	AddonReady(ctx context.Context, name string) (bool, error)
	// DeleteAddon tears down the named add-on's WORKLOAD — its Deployment, Service, collector, and
	// generated config — and, only when deleteData is true, destroys its data volume as well.
	// Keeping the volume by default is the load-bearing part: for the Postgres add-on that volume
	// holds every attached app's database (ADR-0031), and a removal meant as "stop this and put it
	// back" must not destroy it. A retained volume is left with whatever the add-on needs to be
	// usable again after a reinstall. It returns what was torn down and what was deliberately kept,
	// so the caller can report it. Removing an add-on that is not installed returns ErrNotFound.
	//
	// t is the add-on's TYPE, taken from the registry row rather than probed for, and it decides
	// which teardown runs: Postgres is a CloudNativePG `Cluster` and everything else is a Deployment
	// (ADR-0066 §1). A removal is the operation that must never guess — a Postgres instance has no
	// Deployment, and probing for one and finding nothing is indistinguishable from a probe that was
	// refused. Under CloudNativePG the claims belong to the OPERATOR and carry the `Cluster` as their
	// owner, so keeping the data means disowning them before the `Cluster` is deleted rather than
	// simply not deleting them — the same promise, a different act.
	DeleteAddon(ctx context.Context, name string, t AddonType, deleteData bool) (AddonRemoval, error)
	// AddonVolumes returns every PersistentVolumeClaim in the add-on namespace that Burrow created
	// for an add-on, whether or not the add-on that owns it still exists. It reads the CLUSTER, not
	// the registry, because that is the only place a volume outliving its add-on can be seen at all
	// (ADR-0064 §6) — a removed add-on is no longer a registry row, but its claim is still there and
	// still costing money. Deciding which of them are RETAINED is the engine's job. No claims is an
	// empty slice, not an error.
	AddonVolumes(ctx context.Context) ([]AddonVolume, error)
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
	// Postgres, and never carried over the agent control channel. Any error this returns
	// must name the app and key only, never the value.
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

	// BackupJobPresent reports whether the one-shot Job for the given backup id still exists in the
	// add-on namespace. It is a READ, and it exists for one question the registry cannot answer on
	// its own (ADR-0074 §6): a backup row left `pending` — by a burrowd that restarted mid-backup, or
	// by a Job the cluster garbage-collected — is indistinguishable from a backup still running, and
	// a backup that never completed is the sort of thing discovered at restore time unless something
	// looks. A missing Job is reported as absent (false, nil), not an error.
	BackupJobPresent(ctx context.Context, backupID string) (bool, error)

	// RunBackupJob runs a one-shot Job in the add-on namespace that pg_dumps app's database on
	// environment env's Postgres instance to /<backup-pvc>/<app>/<backupID>.dump (custom format),
	// ensuring the backup PVC first (ADR-0032). The Job connects as the superuser, reading the
	// password from that instance's Secret via secretKeyRef env — never a CLI argument, never logged.
	// It blocks until the Job completes, returns an error if the Job fails or times out, and reaps
	// the Job on success. env and app are validated before any Job is built: a dump names the server
	// it came from, so it is as environment-scoped as the database it reads (ADR-0067 §1).
	//
	// dest, when non-nil, adds the SECOND half of the Job: after the dump lands on the PVC, the same
	// pod writes it to the object store and reads it back, retrying a write that did not complete
	// (ADR-0063 §7). The Job then succeeds only if the object is there, which is what lets the
	// caller record a completed backup without lying. dest carries a credential PAIR and therefore
	// never crosses an API boundary — the adapter puts it in a Job-owned Secret and nowhere else.
	//
	// The returned BackupJobOutcome carries the dump's size in bytes (0 when unknown) and, on the
	// error path, the closed reason the Job reported for its failure, so the caller can record WHY a
	// backup failed rather than that it did. A non-nil error always accompanies a failure reason;
	// the outcome is still returned so the caller does not have to parse the error to record it.
	RunBackupJob(ctx context.Context, app, env, backupID string, dest *BackupDestination) (BackupJobOutcome, error)
	// RunRestoreJob runs a one-shot Job in the add-on namespace that pg_restores
	// /<backup-pvc>/<app>/<backupID>.dump into app's database on environment env's instance (--clean
	// --if-exists, so it replaces current contents). Like RunBackupJob it reads the superuser password
	// only via secretKeyRef, blocks until the Job completes, errors on failure or timeout, and reaps
	// the Job on success. env and app are validated before any Job is built — restoring is the one
	// operation where reaching the wrong environment's server overwrites live data with another
	// environment's.
	RunRestoreJob(ctx context.Context, app, env, backupID string) error

	// RunPhysicalBackup asks CloudNativePG for a base backup of environment env's whole instance and
	// waits for the answer (ADR-0066 §2). It CREATES A CUSTOM RESOURCE and reads `.status`: a
	// `postgresql.cnpg.io/v1 Backup` with method `plugin`, handled by the pgBackRest plugin. Burrow
	// runs no backup tool, handles no superuser credential on this path, and constructs no Job — the
	// operator does the work and this seam reports what it said.
	//
	// It is refused, before any object is written, on an instance whose `Cluster` carries no
	// pgBackRest plugin: there is nowhere for a base backup to go, and a `Backup` object created
	// against it would sit in `pending` until the timeout rather than saying so.
	//
	// The returned outcome carries pgBackRest's own backup LABEL on success — the name the repository
	// knows this backup by, and the only handle a later restore has — and on failure the closed
	// reason. A `Backup` object that failed and an archive that is not reaching the store are
	// different reasons, because they are different problems (ADR-0063 §7). A non-nil error always
	// accompanies a failure reason; the outcome is returned either way so the caller records WHY
	// without parsing the error.
	RunPhysicalBackup(ctx context.Context, env, backupID string) (PhysicalBackupOutcome, error)
	// PhysicalBackupPresent reports whether the `Backup` object for the given backup id still exists
	// in the add-on namespace — BackupJobPresent's question for a physical backup, and it exists for
	// the same reason (ADR-0074 §6): a row left `pending` by a burrowd that restarted mid-backup is
	// otherwise indistinguishable from a backup still running. A missing object is absent (false,
	// nil), not an error.
	PhysicalBackupPresent(ctx context.Context, backupID string) (bool, error)

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
	// env is the canonical environment name ("prod" for the default environment, ADR-0067 §2).
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

	// AppHook returns the command app runs at phase in env — the lifecycle hook configured beside the
	// app's config (ADR-0072 §1). A phase with no hook yields a nil command and no error: unset means
	// no hook and today's behaviour exactly, so absence is the ordinary answer and not ErrNotFound.
	// The command is an argv, stored so an argument boundary survives rather than depending on a
	// shell to re-split it. env is the canonical environment name ("prod" for the default one).
	AppHook(ctx context.Context, app, env string, phase HookPhase) ([]string, error)
	// AppHooks returns every hook configured for app in env. None yields an empty slice and no error.
	AppHooks(ctx context.Context, app, env string) ([]Hook, error)
	// SetAppHook upserts the command app runs at phase in env, replacing any command already set
	// there — the write behind `burrow app hook set`. The command arrives validated (non-empty argv).
	SetAppHook(ctx context.Context, app, env string, phase HookPhase, command []string) error
	// UnsetAppHook removes app's hook at phase in env. Removing a hook that is not set is a no-op,
	// not an error: afterwards that phase runs nothing, which is what the caller asked for.
	UnsetAppHook(ctx context.Context, app, env string, phase HookPhase) error
	// DeleteAppHooks removes every hook for app across every environment — the durable side of an app
	// teardown, beside DeleteReleases. Deleting the hooks of an app that has none is a no-op.
	DeleteAppHooks(ctx context.Context, app string) error

	// Policy returns the current guardrail policy: the stored guardrail dispositions
	// overlaid on the built-in defaults (DefaultPolicy), so a store with nothing set
	// returns DefaultPolicy and newly-added guardrails get a sensible default (ADR-0020).
	Policy(ctx context.Context) (Policy, error)
	// SetGuardrail persists the disposition for one guardrail — the write behind
	// `guard set`. It rejects an invalid disposition.
	SetGuardrail(ctx context.Context, code GuardrailCode, d Disposition) error

	// OperationalConfig returns the operator-set operational configuration: every stored limit
	// value, keyed by the code it was set under (ADR-0068 §1). A limit with no stored value
	// resolves to its built-in default, so a store with nothing set yields the defaults.
	OperationalConfig(ctx context.Context) (OperationalConfig, error)
	// SetLimit persists the value of one operational limit — the write behind `cluster config
	// set`. code is the key the value is stored under, which is the bare limit code for a cluster
	// value and `<env>.<code>` for an environment one; the engine composes it, exactly as it does
	// for SetGuardrail. The value arrives already validated and in its canonical text form.
	SetLimit(ctx context.Context, code LimitCode, value string) error

	// AutoDeployLevel returns the auto-deploy level configured for app in the named environment
	// (ADR-0052 §2). A missing configuration resolves to DefaultAutoDeployLevel (off): auto-deploy is
	// opt-in, so an app with no stored row is off and is never polled (ADR-0058). env is the canonical
	// environment name ("prod" for the default environment, ADR-0067 §2).
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
	// starting the backup Job. The row names the app, the on-PVC path, the destination it is
	// being written to, and the status — never a credential. An existing row with the same ID is
	// overwritten.
	RecordBackup(ctx context.Context, b Backup) error
	// SetBackupStatus updates a recorded backup's status (and, when known, its size) — the
	// completed transition burrowd writes when the Job finishes. Setting the status of an
	// unknown backup id returns ErrNotFound.
	SetBackupStatus(ctx context.Context, id string, status BackupStatus, sizeBytes int64) error
	// FailBackup marks a recorded backup failed AND records why: reason is a member of the closed
	// BackupFailureReason set, or of ADR-0074 §2's IssueReason set when the Job never started, and
	// detail is one Burrow-authored line. It is a separate method from SetBackupStatus because a
	// failure without a reason is the thing ADR-0063 §7 exists to stop — "it did not work" sends the
	// reader to the logs, which is where the destination's silence was hiding in the first place.
	//
	// Neither argument may carry a secret value or a vendor response body. Failing an unknown backup
	// id returns ErrNotFound.
	FailBackup(ctx context.Context, id, reason, detail string) error
	// ListBackups returns recorded backups, newest first. An empty app lists every app's backups and
	// an empty env every environment's; a non-empty value restricts to that app or that environment.
	// A listing is a read, not a target, so an unfiltered call spans environments deliberately —
	// unlike backup and restore, which act on exactly one instance and take the environment as a
	// required argument (ADR-0067 §1). No matches yields an empty slice and no error.
	ListBackups(ctx context.Context, app, env string) ([]Backup, error)
	// GetBackup returns the backup with the given id, or ErrNotFound.
	GetBackup(ctx context.Context, id string) (Backup, error)
	// PendingBackups returns the recorded backups still in `pending` that were created before
	// `before`, oldest first — the candidates for ADR-0074 §6's "a backup row still pending whose Job
	// no longer exists". The cutoff is the caller's, not the store's: it is what separates a backup
	// that is simply still running from one nothing is going to finish, and the observer sets it from
	// the injected clock. None yields an empty slice and no error.
	PendingBackups(ctx context.Context, before time.Time) ([]Backup, error)

	// ManagedApps returns the distinct (app, environment) pairs the registry says Burrow OWNS A
	// WORKLOAD FOR: an app whose release history shows a rollout that actually reached the cluster.
	// It is the intent side of ADR-0074 §6's first comparison, so it is deliberately not "every app
	// with a release row" — an app whose only deploy failed before a workload was ever applied has no
	// Deployment to be missing, and reporting one would be a false positive on the day someone is
	// already debugging a failed deploy. None yields an empty slice and no error.
	ManagedApps(ctx context.Context) ([]AppEnvRef, error)

	// RecordExposure upserts the intent behind a successful expose, keyed by (app, environment)
	// (ADR-0074 §6). It records what was ASKED FOR — the host, the port, whether TLS was requested —
	// and never whether it currently works, which stays a live read.
	RecordExposure(ctx context.Context, ex Exposure) error
	// DeleteExposure removes the recorded exposure for app in env. Removing one that is not recorded
	// is a no-op, not an error: an unexpose must succeed whether or not the intent row survived.
	DeleteExposure(ctx context.Context, app, env string) error
	// Exposures returns every recorded exposure, ordered by app then environment. None yields an
	// empty slice and no error.
	Exposures(ctx context.Context) ([]Exposure, error)
	// Exposure returns the recorded exposure for app in env, or ErrNotFound when the app is not
	// published there. It is the read behind ADR-0076 §3's conservative default: the container port
	// an exposure routes to is the ONLY port Burrow knows for an app, so it is what a readiness
	// probe can honestly check. Not being published is the ordinary case, not a failure — the caller
	// treats ErrNotFound as "no port known" and authors no probe.
	Exposure(ctx context.Context, app, env string) (Exposure, error)

	// HealthEndpoint returns the health endpoint declared for app in env (ADR-0076 §5), or the zero
	// value when none was declared — which is where every app starts, since the endpoint is opt-in.
	// A missing row is not an error.
	HealthEndpoint(ctx context.Context, app, env string) (HealthEndpoint, error)
	// SetHealthEndpoint upserts the declared health endpoint for app in env — the write behind
	// `burrow app health set`. The path and port arrive already validated.
	SetHealthEndpoint(ctx context.Context, ep HealthEndpoint) error
	// UnsetHealthEndpoint removes the declared endpoint for app in env, returning the app to
	// ADR-0076 §3's default. Unsetting one that was never declared is a no-op, not an error.
	UnsetHealthEndpoint(ctx context.Context, app, env string) error
	// DeleteHealthEndpoints removes every declared endpoint for app across all environments — the
	// durable side of an app teardown, alongside DeleteReleases. Deleting for an app that declared
	// none is a no-op.
	DeleteHealthEndpoints(ctx context.Context, app string) error

	// DependencyChecksEnabled reports whether the deploy-time dependency check runs for app in env
	// (ADR-0076 §4). It answers TRUE when nothing was recorded: the check is a Burrow-supplied
	// default, so a row exists only where someone made a decision about it, and a missing row is the
	// default rather than an error.
	DependencyChecksEnabled(ctx context.Context, app, env string) (bool, error)
	// SetDependencyChecks records whether the deploy-time dependency check runs for app in env — the
	// write behind `burrow app checks enable|disable`, and the "disableable rather than silent" half
	// of adding a Burrow-supplied default to a path ADR-0072 described as user-configured.
	SetDependencyChecks(ctx context.Context, app, env string, enabled bool, at time.Time) error
	// DeleteDependencyCheckSettings removes app's recorded setting across all environments — the
	// durable side of an app teardown, alongside DeleteHealthEndpoints. Deleting for an app that
	// never recorded one is a no-op.
	DeleteDependencyCheckSettings(ctx context.Context, app string) error

	// RecordFailure opens or extends the ledger row for one observed failure (ADR-0074 §4). The row
	// is keyed by (object, reason) so concurrent reasons on one object coexist as separate rows with
	// independent lifetimes (§5). A first sighting writes first_seen, last_seen and a count of one; a
	// later sighting of the same still-active failure advances last_seen and increments the count,
	// leaving first_seen — the answer to "when did it start" — alone. A sighting of a failure that
	// had been RESOLVED opens a new row rather than reviving the old one, so a thing that broke,
	// recovered and broke again reads as two episodes instead of one long outage.
	//
	// It is the ledger's only write path for failures, and it is not the audit log: they are separate
	// tables deliberately (§7).
	RecordFailure(ctx context.Context, obs FailureObservation) error
	// ResolveFailures closes, at time at, every active ledger row that is not in keep and whose
	// object is not in skip — the "did it recover on its own" half of §4, decided by absence.
	//
	// The two lists are what keep that decision honest. keep is every failure the sweep observed
	// still happening. skip is every object the sweep could NOT read: those rows are left exactly as
	// they are, because a failure Burrow could not check is not a failure that recovered. A row for
	// an object that is in neither — one the sweep read and found healthy, or one that has left the
	// registry entirely — is resolved.
	ResolveFailures(ctx context.Context, at time.Time, keep []FailureKey, skip []ObjectRef) error
	// Failures returns ledger rows matching filter, oldest first. Oldest-first is the order ADR-0074
	// §5 asks for: when one cause has produced many rows, the earliest first_seen in the cascade is
	// the likeliest thing to actually fix. No matches yields an empty slice and no error.
	Failures(ctx context.Context, filter FailureFilter) ([]Failure, error)

	// StartObservationWindow opens a new observation window beginning at `at` and returns its id.
	// One is opened per RUN of the observer, never reused across a restart: a gap between windows is
	// time nobody was watching, and it is the only thing that stops an empty ledger from reading as
	// "nothing broke" (ADR-0074's consequences).
	StartObservationWindow(ctx context.Context, at time.Time) (int64, error)
	// ExtendObservationWindow advances window id's coverage to `until` and counts one more sweep. A
	// non-empty degraded note counts the sweep as incomplete and is stored as the window's most
	// recent degradation, so a stretch during which some objects could not be read is visible as
	// partial coverage rather than as clean coverage. Extending a window that no longer exists
	// returns ErrNotFound, which tells the caller to open a fresh one rather than stop recording.
	ExtendObservationWindow(ctx context.Context, id int64, until time.Time, degraded string) error
	// ObservationWindows returns the windows whose coverage ends at or after since, oldest first,
	// capped by limit (a store default when unset). The gaps BETWEEN the returned windows are the
	// answer the caller is really after. None yields an empty slice and no error.
	ObservationWindows(ctx context.Context, since time.Time, limit int) ([]ObservationWindow, error)

	// PruneLedger enforces ADR-0074 §4's retention bound: it removes failure rows RESOLVED before
	// `before` and observation windows whose coverage ended before it. An ACTIVE failure is never
	// removed, however old — a thing that is still broken is not history. Unbounded growth here is
	// not untidiness; it is an outage in the control plane's own database, which is why the bound is
	// a requirement rather than a setting.
	PruneLedger(ctx context.Context, before time.Time) (LedgerPruneResult, error)

	// CreateEnvironment registers a named environment mapping name to namespace (ADR-0035 phase 2).
	// It rejects a duplicate name (the name is the primary key) with an ErrInvalid-wrapped error.
	// The default environment `prod` IS stored here, written once by Engine.EnsureDefaultEnvironment
	// at burrowd startup (ADR-0067 §2) rather than synthesized on every read.
	CreateEnvironment(ctx context.Context, name, namespace string) error
	// ListEnvironments returns the registered environments ordered by name, INCLUDING the default
	// environment `prod`; the engine hoists that one first and marks it. None yields an empty slice
	// and no error.
	ListEnvironments(ctx context.Context) ([]Environment, error)
	// GetEnvironment returns the registered environment with the given name, or ErrNotFound.
	GetEnvironment(ctx context.Context, name string) (Environment, error)
	// DeleteEnvironment removes the registered environment with the given name, or ErrNotFound when
	// no such environment is registered. The default environment `prod` is stored here but is never
	// removed: the engine rejects it before this call (ADR-0067 §2).
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
	// API — never over the agent control channel — and is written straight to the Secret: it is
	// NEVER logged, never stored in Postgres, and never returned in a response. Any error names
	// the key only, never the value.
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

// ObjectStoreCredential is one S3-compatible credential: the pair an S3 API call is signed with
// (ADR-0063 §1). Both halves are SECRET VALUES — they are read from the burrow-credentials Secret
// at call time, handed straight to an adapter, and never logged, audited, returned in a response,
// or placed in an error.
type ObjectStoreCredential struct {
	AccessKeyID     string
	SecretAccessKey string
}

// IsZero reports whether the credential carries neither half, so a caller can tell "not configured"
// from "configured with an empty value".
func (c ObjectStoreCredential) IsZero() bool {
	return c.AccessKeyID == "" && c.SecretAccessKey == ""
}

// LifecycleRule is one rule read from a bucket's lifecycle configuration — the vendor-side policy
// that decides when objects in the bucket are deleted (ADR-0063 §3). It carries only what Burrow
// reconciles against backup retention: whether the rule is in force, what it applies to, and how
// long an object survives under it.
type LifecycleRule struct {
	// ID is the rule's identifier at the vendor, quoted back in a refusal so the operator can find
	// the rule they have to change.
	ID string `json:"id,omitempty"`
	// Prefix is the key prefix the rule applies to; empty means the whole bucket.
	Prefix string `json:"prefix,omitempty"`
	// Enabled reports whether the rule is in force. A disabled rule deletes nothing and so cannot
	// conflict with retention.
	Enabled bool `json:"enabled"`
	// ExpireAfterDays is how many days after creation the rule deletes an object, or 0 when the
	// rule expires nothing by age (e.g. it only aborts incomplete multipart uploads).
	ExpireAfterDays int `json:"expire_after_days,omitempty"`
}

// ObjectStore is the seam over ONE bucket's vendor, addressed by S3-compatible endpoint rather than
// by vendor name (ADR-0063 §1). Its surface is deliberately the whole of what ADR-0063 §2 permits
// Burrow to do with object storage and no more: create the bucket it will own, verify the
// destination works by writing and deleting a probe object, and read the lifecycle configuration it
// must reconcile against backup retention. There is no cp, no sync, no listing of arbitrary
// prefixes, no presigned URL, and no policy/IAM/replication surface — a capability enters here only
// when a Burrow feature requires it, never because the S3 API offers it.
//
// It is an OPTIONAL seam: nil is allowed, and registering an object-storage provider errors cleanly
// (ErrNotImplemented) when it is not wired.
type ObjectStore interface {
	// BucketExists reports whether the bucket is present and reachable with this credential. A
	// bucket that exists but belongs to someone else is reported as absent-or-unreachable rather
	// than present, since Burrow can do nothing with it either way.
	BucketExists(ctx context.Context, bucket string) (bool, error)
	// CreateBucket creates the bucket. The NAME is the engine's to choose (ADR-0063 §4: a readable
	// prefix plus a random component, recorded in the providers row) — an implementation creates
	// exactly the name it is given and never derives or adopts one.
	CreateBucket(ctx context.Context, bucket string) error
	// PutObject writes body at key. It exists for the configuration-time probe of ADR-0063 §2 and
	// for the backup path that follows; it is not a general upload surface.
	PutObject(ctx context.Context, bucket, key string, body []byte) error
	// PutObjectStream writes size bytes read from body at key, without holding them in memory. It
	// is PutObject's shape for the one payload that is not small — a pg_dump, which is routinely
	// gigabytes and would OOM the container that buffered it.
	//
	// sha256Hex is the hex SHA-256 of exactly those bytes, computed by the caller before the read
	// begins. It is not an optimisation: an S3-compatible endpoint validates the body against the
	// hash it was signed with and REFUSES a transfer that does not match, so passing it is what
	// makes "the store accepted the write" mean "the store accepted these bytes" rather than "the
	// store accepted some bytes". A caller that cannot compute it cannot use this method.
	//
	// It reports whether the write was REFUSED (the endpoint answered, and said no) as opposed to
	// failing to complete, because ADR-0063 §7 retries the second and must not retry the first: a
	// revoked credential does not become a valid one by being asked again, and spending the retry
	// budget on it delays the loud failure that is the whole point.
	PutObjectStream(ctx context.Context, bucket, key string, body io.Reader, size int64, sha256Hex string) (refused bool, err error)
	// StatObject returns the size of the object at key, or ErrNotFound when it is not there.
	//
	// It exists so a backup can be VERIFIED rather than assumed (ADR-0063 §7). A 200 on a write is
	// the endpoint's word that it accepted the request; reading the object back afterwards is the
	// separate fact that it can serve it, at the key Burrow recorded, at the length Burrow sent.
	// Those come apart in practice — an eventually-consistent listing, a bucket policy that permits
	// writes and not reads, a multipart upload that completed into nothing — and the difference is
	// only ever discovered at restore time unless something looks.
	StatObject(ctx context.Context, bucket, key string) (int64, error)
	// DeleteObject removes key. Deleting an object that is already absent is a no-op, not an error,
	// so the probe's cleanup is idempotent.
	DeleteObject(ctx context.Context, bucket, key string) error
	// LifecycleRules returns the bucket's lifecycle rules. NO rules is an empty slice and no error.
	//
	// A store that cannot ANSWER — the vendor does not implement the lifecycle API, or this
	// credential is not permitted to read it — returns an error wrapping ErrLifecycleUnknown, and
	// the engine reports the reconciliation as unknown rather than as verified (ADR-0063 §3: an
	// unverifiable invariant reported as verified is worse than one reported as unknown). Any other
	// error is a genuine failure.
	LifecycleRules(ctx context.Context, bucket string) ([]LifecycleRule, error)
}

// ObjectStoreFactory builds an ObjectStore for an S3-compatible endpoint and credential pair
// (ADR-0063 §1) — the object-storage sibling of DNSFactory, and for the same reason: the engine
// reaches a vendor without importing its adapter, so production wires controlplane/objectstore and
// a test substitutes a fake. region is the S3 region the request is signed for; endpoint is the
// API endpoint that answers, and the vendor is whoever that is.
type ObjectStoreFactory interface {
	ObjectStore(endpoint, region string, cred ObjectStoreCredential) (ObjectStore, error)
}
