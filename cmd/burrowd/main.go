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
	store, err := openWithRetry(ctx, dsn, 4*time.Minute)
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

	// Vendor tokens live in the burrow-credentials Secret in burrowd's own (control-plane)
	// namespace — not the app namespace — read through a get scoped to that one object
	// (ADR-0023).
	creds, err := kube.NewCredentialsFromConfig(kubeCfg, controlPlaneNamespace(), kube.DefaultCredentialsSecret)
	if err != nil {
		return err
	}

	// The Postgres add-on provisioner connects to the installed instance as the superuser to give
	// each app its own database and role (ADR-0031). It reads the superuser password from the
	// burrow-postgres Secret in the add-on namespace, so it is scoped there.
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

	// The in-cluster builder runs a build as a Kubernetes Job in the app namespace, cloning the git
	// ref inside the cluster and pushing the built image to a registry the cluster can pull from
	// (ADR-0053). It is the optional in-cluster build path — Burrow stays client-build-first, so a
	// build is never required for deploy. BURROW_BUILD_IMAGE / BURROW_GIT_IMAGE let the install
	// override the default builder and clone images (their install wiring is Phase 3).
	builder, err := kube.NewBuilderFromConfig(kubeCfg, namespace)
	if err != nil {
		return err
	}
	builder.WithBuildImage(os.Getenv("BURROW_BUILD_IMAGE")).WithGitImage(os.Getenv("BURROW_GIT_IMAGE"))

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
		// The zero-config default push target for an in-cluster build with no explicit target (ADR-0053
		// §5): the in-cluster registry `burrow cluster registry install` deploys, whose in-cluster
		// reference it wires here via BURROW_BUILD_REGISTRY. Empty when no in-cluster registry is
		// installed, in which case a build must name its own target; a caller-supplied target always
		// overrides this, so external registries stay fully supported.
		BuildRegistry: os.Getenv("BURROW_BUILD_REGISTRY"),
		// The app namespace is the implicit `default` environment (ADR-0035 phase 2).
		AppNamespace: namespace,
	})
	if err != nil {
		return err
	}

	handler, err := api.New(api.Config{Engine: engine, Token: token, Version: version})
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
	return nil
}

// autoDeployInterval reads the auto-deploy poll cadence from BURROW_AUTODEPLOY_INTERVAL, a Go
// duration (e.g. "5m", "30s"). It returns 0 when unset — the poller applies its conservative
// default (~5 min, ADR-0052 §7) — and a negative sentinel when set to a non-positive value, which
// turns the watcher off. An unparseable value is a non-fatal misconfiguration: it logs and falls
// back to the default.
func autoDeployInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("BURROW_AUTODEPLOY_INTERVAL"))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("burrowd: ignoring invalid BURROW_AUTODEPLOY_INTERVAL %q: %v", v, err)
		return 0
	}
	if d <= 0 {
		return -1 // an explicit off
	}
	return d
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
