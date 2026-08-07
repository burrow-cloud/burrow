// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrowd is the Burrow control plane: the component that holds the cluster
// credentials, runs the deploy/rollout/rollback/logs/scale orchestration, enforces the
// guardrails, and records who deployed what (ADR-0002). It connects to the in-cluster
// Postgres (ADR-0012) and applies migrations, drives the cluster through the client-go
// adapter (ADR-0011), applies workloads by image reference without ever contacting a
// registry (ADR-0040/0004), and serves the authenticated control-plane HTTP API (ADR-0005).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/api"
	"github.com/burrow-cloud/burrow/controlplane/dns"
	"github.com/burrow-cloud/burrow/controlplane/kube"
	"github.com/burrow-cloud/burrow/controlplane/logs"
	"github.com/burrow-cloud/burrow/controlplane/metrics"
	"github.com/burrow-cloud/burrow/controlplane/objectstore"
	"github.com/burrow-cloud/burrow/controlplane/postgres"
	"github.com/burrow-cloud/burrow/controlplane/registry"
	"github.com/burrow-cloud/burrow/controlplane/sys"
)

// version is the Burrow version this binary reports and stamps into the database for the
// upgrade gate (ADR-0013). This is the development default; the release workflow rewrites it to
// the git tag before building the published image, so a released burrowd reports its real
// version. v0.0.0 keeps the upgrade gate's version parser happy for local and e2e builds.
var version = "v0.0.0"

func main() {
	// `burrowd ship-backup` is not the control plane: it is the same binary run inside a backup Job's
	// pod to write one dump to the object store and read it back (ADR-0063 §7). It is dispatched
	// before any flag or database wiring because it connects to nothing this process normally needs —
	// no Postgres, no cluster, no API token — and a failure to reach any of those must not be able to
	// fail a backup for a reason unrelated to the backup.
	if len(os.Args) > 1 && os.Args[1] == controlplane.ShipBackupCommand {
		if err := shipBackup(context.Background(), os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "burrowd ship-backup:", err)
			os.Exit(1)
		}
		return
	}
	// `burrowd install-probe <dir>` and `burrowd check-dependencies` are the two halves of the
	// deploy-time dependency check (ADR-0076 §4), dispatched here for the same reason ship-backup is:
	// they run inside a check Job's pod and connect to nothing this process normally needs. The
	// second of the two runs in the USER's image — Burrow's binary made executable there by the
	// first — so it must not reach for a database, a cluster or a token it will not find.
	if len(os.Args) > 1 && os.Args[1] == controlplane.ProbeInstallCommand {
		dir := ""
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		if err := installProbe(dir); err != nil {
			fmt.Fprintln(os.Stderr, "burrowd "+controlplane.ProbeInstallCommand+":", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == controlplane.ProbeCheckCommand {
		if err := checkDependencies(context.Background(), os.Stdout, os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, "burrowd "+controlplane.ProbeCheckCommand+":", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "burrowd:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", envOr("BURROW_LISTEN", ":8080"), "address to serve the control-plane API on")
	flag.Parse()

	// Secrets come from the environment, never flags (which are visible in the process
	// table).
	dsn := os.Getenv("BURROW_DATABASE_URL")
	if dsn == "" {
		return errors.New("BURROW_DATABASE_URL is required (the in-cluster Postgres connection string)")
	}
	token := os.Getenv("BURROW_API_TOKEN")
	if token == "" {
		return errors.New("BURROW_API_TOKEN is required (the bearer token clients authenticate with)")
	}
	ctx := context.Background()

	// Start the HTTP server immediately and reflect startup state through readiness, rather
	// than blocking the server on the database. /healthz returns 503 until the control plane
	// has connected to Postgres, migrated, and wired its API, then 200 — so burrowd is up in
	// milliseconds, and a database that is slow or briefly unreachable shows as not-ready
	// instead of blocking startup or crash-looping.
	var (
		ready      atomic.Bool
		apiHandler atomic.Pointer[http.Handler]
		store      atomic.Pointer[postgres.Store]
	)
	go func() {
		if err := startControlPlane(ctx, dsn, token, &apiHandler, &store, &ready); err != nil {
			log.Printf("burrowd: control plane failed to start (staying not-ready): %v", err)
		}
	}()
	defer func() {
		if s := store.Load(); s != nil {
			s.Close()
		}
	}()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           logRequests(serverHandler(&ready, &apiHandler)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return serve(srv)
}

// logRequests is an access log: it logs each request as it completes — method, path, status,
// and how long it took (the standard logger prepends the timestamp). The frequent /healthz
// readiness probe is skipped so the log shows real API traffic; direct control-plane traffic is
// low even on a busy cluster, so logging every request is fine.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

// statusRecorder wraps a ResponseWriter to capture the status code for the access log, defaulting
// to 200 (the status when a handler writes a body without calling WriteHeader).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// serverHandler serves /healthz as the readiness signal (503 until the control plane has
// finished starting) and delegates everything else to the API handler once it is wired.
func serverHandler(ready *atomic.Bool, apiHandler *atomic.Pointer[http.Handler]) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "control plane starting up", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if h := apiHandler.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "control plane starting up", http.StatusServiceUnavailable)
	})
	return mux
}

// startControlPlane connects to the database, applies migrations, wires the
// Kubernetes/engine/API stack, and flips readiness. It runs in the background so the HTTP
// server is serving (and answering health checks) immediately. A database that is slow or
// briefly unreachable leaves burrowd not-ready rather than blocking startup or exiting, so
// it does not crash-loop while Postgres is coming up.
func startControlPlane(ctx context.Context, dsn, token string, apiHandler *atomic.Pointer[http.Handler], storeOut *atomic.Pointer[postgres.Store], ready *atomic.Bool) error {
	store, err := openWithRetry(ctx, dsn, dbConnectBudget)
	if err != nil {
		return err
	}
	storeOut.Store(store)
	if err := store.Migrate(ctx, version); err != nil {
		return err
	}

	namespace := envOr("BURROW_NAMESPACE", "default")
	kubeCfg, err := kube.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading kubernetes config: %w", err)
	}
	k8s, err := kube.NewFromConfig(kubeCfg, namespace)
	if err != nil {
		return err
	}
	// Add-ons live in their own namespace, set by the install manifest (ADR-0025).
	k8s.WithAddonNamespace(os.Getenv("BURROW_ADDON_NAMESPACE"))
	// The operational limits the ADAPTERS apply — the unschedulable grace, the build Job's retention,
	// the metrics add-on's sample retention (ADR-0068 §6). They read the store directly rather than
	// being handed a value, so `burrow cluster config set` takes effect on the next operation instead
	// of on the next burrowd restart. A read that fails resolves to the built-in defaults and says so
	// in the log: an unavailable database must not turn into a failed deploy or a status call that
	// errors instead of answering.
	limits := controlplane.ClusterConfigFrom(store.OperationalConfig)
	k8s.WithOperationalLimits(limits)
	// Pin the backup Job's shipping container to burrowd's own stamped version, for the same reason
	// the builder image is pinned: a released control plane ships backups with the binary published
	// under the SAME release tag, not a floating :latest a later republish could change under it. An
	// explicit BURROW_SHIPPER_IMAGE always wins, which is how a dev or e2e cluster (version "v0.0.0",
	// where no published image exists at a pseudo-version) points at a locally loaded one.
	shipperImage := os.Getenv("BURROW_SHIPPER_IMAGE")
	if shipperImage == "" {
		shipperImage = kube.ShipperImageForVersion(version)
	}
	k8s.WithShipperImage(shipperImage)

	// Vendor tokens live in the burrow-credentials Secret in burrowd's own (control-plane)
	// namespace — not the app namespace — read through a get scoped to that one object
	// (ADR-0023).
	creds, err := kube.NewCredentialsFromConfig(kubeCfg, controlPlaneNamespace(), kube.DefaultCredentialsSecret)
	if err != nil {
		return err
	}

	// The Postgres add-on provisioner connects to the installed instance as the superuser to give
	// each app its own database and role (ADR-0031). It reads the superuser password from the
	// per-instance superuser Secret in the add-on namespace, so it is scoped there; which instance a
	// call reaches is decided per operation by the environment it names (ADR-0067 §1).
	dbProvisioner, err := kube.NewPostgresProvisionerFromConfig(kubeCfg, os.Getenv("BURROW_ADDON_NAMESPACE"))
	if err != nil {
		return err
	}

	// The capability prober reads the cluster's read-only capabilities live (ADR-0034). It uses
	// burrowd's in-cluster client, so it needs only the narrow read-only ClusterRole the install
	// grants (get/list on nodes, storageclasses, ingressclasses) plus API-group discovery.
	prober, err := kube.NewProberFromConfig(kubeCfg)
	if err != nil {
		return err
	}
	// Which shape burrowd's OWN database runs in is part of that report (ADR-0086 §2), so the answer
	// outlives the install output that stated it. It is read from the control-plane namespace, which
	// is why the prober is told which one that is.
	prober.WithControlPlaneNamespace(controlPlaneNamespace())

	// The in-cluster builder runs a build as a Kubernetes Job in the dedicated burrow-builds namespace,
	// isolated from both the app and control-plane namespaces (issue #278), cloning the git ref inside
	// the cluster and pushing the built image to a registry the cluster can pull from (ADR-0053). It is
	// the optional in-cluster build path — Burrow stays client-build-first, so a build is never
	// required for deploy. BURROW_BUILD_IMAGE / BURROW_GIT_IMAGE let the install override the default
	// builder and clone images (their install wiring is Phase 3). The same capacity prober fails a
	// build fast when no node has room for it, instead of leaving it Pending (issue #274).
	builder, err := kube.NewBuilderFromConfig(kubeCfg)
	if err != nil {
		return err
	}
	// Pin the builder image to burrowd's own stamped version so a released control plane pulls the
	// builder published under the SAME release tag (reproducible), rather than the floating :latest a
	// later republish could silently change. An explicit BURROW_BUILD_IMAGE always wins; a dev/e2e
	// build (version "v0.0.0") leaves the empty value, which keeps the :latest default.
	buildImage := os.Getenv("BURROW_BUILD_IMAGE")
	if buildImage == "" {
		buildImage = kube.BuilderImageForVersion(version)
	}
	builder.WithBuildImage(buildImage).WithGitImage(os.Getenv("BURROW_GIT_IMAGE")).WithCapacityProber(prober).WithOperationalLimits(limits)

	// One HTTP client shared across the observability adapters — burrowd reaches each backend
	// in-cluster.
	obsHTTP := &http.Client{Timeout: 20 * time.Second}
	engine, err := controlplane.New(controlplane.Deps{
		Kubernetes:  k8s,
		Database:    store,
		Clock:       sys.Clock{},
		IDs:         sys.IDs{},
		Resolver:    sys.Resolver{},
		Credentials: creds,
		DNS:         dns.NewFactory(),
		// The publish pre-flight's two seams (ADR-0041 §3): the host is resolved at its zone's own
		// nameservers, and the ACME challenge path is requested over plain HTTP, before a
		// certificate is ever asked for — so a path that cannot answer the challenge never opens an
		// order against the account's rate limit.
		AuthoritativeResolver: sys.AuthoritativeResolver{},
		HTTPProbe:             sys.HTTPProbe{},
		// ObjectStore reaches an S3-compatible endpoint so a backup destination outside this cluster
		// can be registered and verified (ADR-0063). It is outbound-only and is never on the deploy
		// path, which stays independent of any third party being reachable (ADR-0040).
		ObjectStore: objectstore.NewFactory(),
		Logs: map[string]controlplane.LogsQuerier{
			"victorialogs": logs.NewVictoriaLogs(obsHTTP),
			"loki":         logs.NewLoki(obsHTTP),
		},
		Metrics: map[string]controlplane.MetricsQuerier{
			"prometheus":      metrics.NewPromQL(obsHTTP),
			"victoriametrics": metrics.NewPromQL(obsHTTP),
		},
		DatabaseProvisioner: dbProvisioner,
		ClusterProber:       prober,
		// The same Prober reads scheduling capacity/headroom (node allocatable + pod requests) for
		// the capacity surface (issue #275). It uses burrowd's in-cluster client and needs the
		// cluster-wide read on nodes and pods the capability ClusterRole grants.
		CapacityProber: prober,
		// RegistryClient lists an image repository's tags for the auto-deploy read/watch (ADR-0052).
		// It lists anonymously in this read-only phase — public GHCR (the reference registry), public
		// Docker Hub, DO, and GCR-token registries all list without credentials. Authenticated
		// private-repo listing needs a deliberate burrowd RBAC grant to read the client-side
		// burrow-registry pull secret, withheld today under the least-privilege boundary
		// (ADR-0017/ADR-0040); it lands with the Phase 4 poller, for which the adapter is already
		// ready via RegistryAuth. It reaches the registry outbound over its own bounded-timeout client.
		RegistryClient: registry.NewClient(&http.Client{Timeout: 20 * time.Second}),
		// The in-cluster builder for the optional build path (ADR-0053). Optional — a build errors
		// cleanly (ErrNotImplemented) when it is not wired; it is wired here so `burrow app build` and
		// the agent build verb (later phases) have a builder.
		Builder: builder,
		// The same adapter reads back the builds that succeeded and were never deployed (issue #504).
		// It is the same object because the build Job is both the work and the record of what the work
		// was for: the builder writes that intent, the ledger reads it.
		BuildLedger: builder,
		// The zero-config default push target for an in-cluster build with no explicit target (ADR-0053
		// §5): the in-cluster registry `burrow cluster registry install` deploys, whose in-cluster
		// Service reference it wires here via BURROW_BUILD_REGISTRY. The build pushes here in-cluster
		// over plain HTTP. Empty when no in-cluster registry is installed, in which case a build must
		// name its own target; a caller-supplied target always overrides this, so external registries
		// stay fully supported.
		BuildRegistry: os.Getenv("BURROW_BUILD_REGISTRY"),
		// The PUBLIC registry host the in-cluster build's resulting deploy references so the node pulls
		// through the ingress over TLS, distinct from the internal push endpoint above (ADR-0054 §5).
		// `burrow cluster registry install --host` wires it via BURROW_BUILD_PUBLIC_REGISTRY; empty
		// falls back to referencing the internal push endpoint.
		BuildPublicRegistry: os.Getenv("BURROW_BUILD_PUBLIC_REGISTRY"),
		// The app namespace is the namespace the default environment `prod` maps to (ADR-0067 §3).
		AppNamespace: namespace,
	})
	if err != nil {
		return err
	}

	// Register the one environment an install has: `prod`, mapped to the app namespace (ADR-0067
	// §2–§3). It runs here rather than from `burrow cluster install` so that a fresh install, a
	// re-run, a restart and an UPGRADE all take the same path — an install predating ADR-0067 gains
	// the environment on its first start under this version, pointing at the namespace its apps are
	// already in, with nothing moved and nothing renamed. It is idempotent, so a second replica or a
	// hundredth restart changes nothing.
	//
	// A failure here leaves burrowd not-ready rather than serving. That is deliberate: the only way
	// it fails is a database that is unreachable (the same condition the migration above already
	// gates on) or a `prod` registered against a different namespace, which means unqualified
	// operations would land somewhere other than where this control plane deploys apps. Serving in
	// either state is worse than not serving.
	defaultEnv, err := engine.EnsureDefaultEnvironment(ctx)
	if err != nil {
		return err
	}
	log.Printf("burrowd: environment %q serves namespace %q", defaultEnv.Name, defaultEnv.Namespace)

	// This install's own id (ADR-0084 §5), rendered into the environment by the install manifests
	// from the ConfigMap that records it. It is read here rather than beside the token because it is
	// not a secret and not required: a control plane installed before ids existed carries none, and
	// one that does not know its own id serves every caller rather than refusing on an unknown.
	installID := os.Getenv("BURROW_INSTALL_ID")

	handler, err := api.New(api.Config{Engine: engine, Token: token, Version: version, InstallID: installID})
	if err != nil {
		return err
	}
	apiHandler.Store(&handler)
	ready.Store(true)
	log.Printf("burrowd %s ready", version)

	// Start the pull-based passive-deploy watcher (ADR-0052 Phase 4b): it polls the registry for new
	// in-scope tags and drives the same guarded deploy an explicit call runs. It is outbound-only and
	// optional — with no registry seam or a non-positive interval it does nothing. A non-positive
	// BURROW_AUTODEPLOY_INTERVAL turns the watcher off entirely, leaving the explicit deploy as the
	// only path (ADR-0052 §7). It runs for the life of the process on ctx.
	interval := autoDeployInterval()
	if interval < 0 {
		log.Printf("burrowd: auto-deploy poller disabled (BURROW_AUTODEPLOY_INTERVAL <= 0)")
	} else {
		poller := engine.NewAutoDeployPoller(controlplane.AutoDeployConfig{Interval: interval})
		go poller.Run(ctx)
	}

	// Start the failure observer (ADR-0074 §3): it sweeps the workloads the registry says Burrow owns,
	// compares them against the cluster, and writes what broke to the ledger. It is READ-ONLY against
	// the cluster and never remediates (§9). A non-positive BURROW_OBSERVE_INTERVAL turns it off
	// entirely, which leaves burrowd answering questions and recording no history — the state it was
	// in before this existed. It runs for the life of the process on ctx; a restart interrupts
	// observation, which is precisely why the ledger records its own coverage.
	if observeInterval := observeInterval(); observeInterval < 0 {
		log.Printf("burrowd: failure observer disabled (BURROW_OBSERVE_INTERVAL <= 0) — no failure history will be recorded")
	} else {
		observer := engine.NewObserver(controlplane.ObserverConfig{
			Interval:  observeInterval,
			Retention: ledgerRetention(),
		})
		go observer.Run(ctx)
	}

	// Start the stranded-build reconciler (issue #504): it finds builds that SUCCEEDED and whose
	// deploy never ran — a client that dropped mid-build, a control plane restarted while a build was
	// running — and finishes them through the same guarded deploy path, so a build nobody is still
	// connected to is not silently discarded along with the minutes and the budget it cost. Its first
	// sweep runs immediately, which is the point: a control plane that went down mid-build is the case
	// nothing else covers. A non-positive BURROW_BUILD_RECONCILE_INTERVAL turns it off, leaving builds
	// exactly as fragile as they were before it existed. It runs for the life of the process on ctx.
	if buildInterval := buildReconcileInterval(); buildInterval < 0 {
		log.Printf("burrowd: build reconciler disabled (BURROW_BUILD_RECONCILE_INTERVAL <= 0) — a build whose client disconnects will be discarded")
	} else {
		reconciler := engine.NewBuildReconciler(controlplane.BuildReconcilerConfig{Interval: buildInterval})
		go reconciler.Run(ctx)
	}
	return nil
}

// buildReconcileInterval reads the stranded-build sweep cadence from BURROW_BUILD_RECONCILE_INTERVAL,
// a Go duration. It returns 0 when unset — the reconciler applies DefaultBuildReconcileInterval — and
// a negative sentinel when set to a non-positive value, which turns recovery off.
func buildReconcileInterval() time.Duration {
	return envDuration("BURROW_BUILD_RECONCILE_INTERVAL")
}

// observeInterval reads the observation cadence from BURROW_OBSERVE_INTERVAL, a Go duration. It
// returns 0 when unset — the observer applies DefaultObserveInterval — and a negative sentinel when
// set to a non-positive value, which turns observation off.
func observeInterval() time.Duration {
	return envDuration("BURROW_OBSERVE_INTERVAL")
}

// ledgerRetention reads how long resolved failures are kept from BURROW_LEDGER_RETENTION, a Go
// duration (e.g. "720h"). Unset applies DefaultLedgerRetention; a non-positive value turns pruning
// off, which an operator may deliberately want — the bound exists so unbounded growth in the control
// plane's own database cannot happen by accident, not to stop someone choosing it deliberately.
func ledgerRetention() time.Duration {
	return envDuration("BURROW_LEDGER_RETENTION")
}

// envDuration parses a Go duration from an environment variable: 0 when unset (the caller's default
// applies) and a negative sentinel when the value is non-positive, which every caller reads as an
// explicit off. An unparseable value is a non-fatal misconfiguration — it logs and falls back to the
// default, because refusing to start over a malformed duration is a worse failure than running on
// the default cadence.
func envDuration(key string) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("burrowd: ignoring invalid %s %q: %v", key, v, err)
		return 0
	}
	if d <= 0 {
		return -1 // an explicit off
	}
	return d
}

// autoDeployInterval reads the auto-deploy poll cadence from BURROW_AUTODEPLOY_INTERVAL, a Go
// duration (e.g. "5m", "30s"). It returns 0 when unset — the poller applies its conservative
// default (~5 min, ADR-0052 §7) — and a negative sentinel when set to a non-positive value, which
// turns the watcher off.
func autoDeployInterval() time.Duration {
	return envDuration("BURROW_AUTODEPLOY_INTERVAL")
}

// serve runs the HTTP server and shuts it down gracefully on SIGINT/SIGTERM.
func serve(srv *http.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Printf("burrowd %s listening on %s", version, srv.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		log.Println("burrowd shutting down")
		return srv.Shutdown(shutdownCtx)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// controlPlaneNamespace returns the namespace burrowd itself runs in — where the
// burrow-credentials Secret lives (distinct from BURROW_NAMESPACE, the app namespace). It
// prefers the POD_NAMESPACE the install injects via the downward API, falls back to the
// service-account namespace file every in-cluster pod has, and finally to "burrow".
func controlPlaneNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "burrow"
}

// Database-wait tuning. Each connect/ping attempt is bounded by dbAttemptTimeout so a hung or
// slow dial fails fast (rather than blocking on the OS default TCP dial timeout, ~2 min) and the
// loop retries on the dbWaitBackoff cadence — all within the overall budget the caller passes.
// The log is throttled to at most one line per dbWaitLogInterval so a fast retry loop stays
// readable instead of printing a line every couple of seconds for the whole budget.
const (
	dbAttemptTimeout  = 5 * time.Second
	dbWaitBackoff     = 2 * time.Second
	dbWaitLogInterval = 15 * time.Second
	// dbConnectBudget is how long burrowd waits for its database to accept connections before it
	// gives up and stays not-ready. It covers a database that is still coming up beside it, which is
	// the ordinary case on a fresh install: burrowd and the database are applied in one manifest.
	//
	// It is fifteen minutes rather than the four it used to be because the default database now
	// bootstraps under an operator (ADR-0086 §1): the operator has to notice the object, provision a
	// volume, pull the PostgreSQL operand image and run initdb before anything listens. On a small
	// node with a cold image cache that is minutes, not seconds, and the cost of a budget that is too
	// short is not a retry — the wait is what makes burrowd ready at all, so exhausting it leaves a
	// control plane serving 503 until somebody restarts the pod.
	dbConnectBudget = 15 * time.Minute
)

// pinger performs one bounded attempt to connect to (and ping) the database. The retry loop
// gives each call its own timeout via ctx; a slow dial aborts when ctx expires and the loop
// retries, so a single attempt never hangs on the OS default TCP dial timeout.
type pinger func(ctx context.Context) error

// dbWait bounds how burrowd waits for the database at startup: each attempt gets its own
// per-attempt timeout so it fails fast, the loop pauses backoff between attempts, and the whole
// wait is bounded by budget. Every field is set explicitly so a test can drive the loop with
// tiny durations, deterministically and without real network.
type dbWait struct {
	attempt     time.Duration // per-attempt timeout
	backoff     time.Duration // pause between attempts
	budget      time.Duration // overall deadline for the whole wait
	logInterval time.Duration // throttle: log the first failure, then at most this often
}

// run retries ping until it succeeds or the budget is exhausted. Each attempt runs under a
// context bounded by w.attempt, so a hung or slow attempt is cancelled and the loop retries on
// its backoff cadence rather than blocking on one stuck dial. It returns nil on the first
// success, or the last error (wrapped with the budget) once the deadline passes.
func (w dbWait) run(ctx context.Context, ping pinger) error {
	deadline := time.Now().Add(w.budget)
	var lastLogged time.Time
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, w.attempt)
		err := ping(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		now := time.Now()
		if !now.Before(deadline) {
			return fmt.Errorf("connecting to the database after %s: %w", w.budget, err)
		}
		if attempt == 1 || now.Sub(lastLogged) >= w.logInterval {
			log.Printf("waiting for the database (attempt %d): %v", attempt, err)
			lastLogged = now
		}
		// Back off before the next attempt, but honor an outer cancellation so the wait does
		// not sit blocked past a shutdown signal.
		select {
		case <-ctx.Done():
			return fmt.Errorf("connecting to the database: %w", ctx.Err())
		case <-time.After(w.backoff):
		}
	}
}

// openWithRetry waits for the database to accept connections, retrying for up to budget rather
// than crashing — so burrowd comes up gracefully alongside an in-cluster Postgres that is still
// starting, instead of crash-looping until it is ready. Each attempt is bounded (see dbWait) so a
// single connect/ping never hangs on the OS default dial timeout and the loop retries fast.
func openWithRetry(ctx context.Context, dsn string, budget time.Duration) (*postgres.Store, error) {
	dsn = withConnectTimeout(dsn, dbAttemptTimeout)
	var store *postgres.Store
	ping := func(attemptCtx context.Context) error {
		s, err := postgres.Open(attemptCtx, dsn)
		if err != nil {
			return err
		}
		store = s
		return nil
	}
	w := dbWait{attempt: dbAttemptTimeout, backoff: dbWaitBackoff, budget: budget, logInterval: dbWaitLogInterval}
	if err := w.run(ctx, ping); err != nil {
		return nil, err
	}
	return store, nil
}

// withConnectTimeout adds a libpq connect_timeout (in whole seconds) to dsn as a second bound on
// a hung dial, alongside the per-attempt context. It is a no-op if the DSN already sets one or if
// the timeout is under a second. Both the URL form (postgres://…?connect_timeout=5) and the
// keyword form (host=… connect_timeout=5) are handled; an unparseable URL is returned unchanged,
// since the per-attempt context still bounds the dial.
func withConnectTimeout(dsn string, timeout time.Duration) string {
	secs := int(timeout / time.Second)
	if secs < 1 || strings.Contains(dsn, "connect_timeout") {
		return dsn
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		q := u.Query()
		q.Set("connect_timeout", strconv.Itoa(secs))
		u.RawQuery = q.Encode()
		return u.String()
	}
	if strings.TrimSpace(dsn) == "" {
		return dsn
	}
	return strings.TrimSpace(dsn) + " connect_timeout=" + strconv.Itoa(secs)
}
