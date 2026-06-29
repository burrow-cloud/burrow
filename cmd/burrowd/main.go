// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Command burrowd is the Burrow control plane: the component that holds the cluster
// credentials, runs the deploy/rollout/rollback/logs/scale orchestration, enforces the
// guardrails, and records who deployed what (ADR-0002). It connects to the in-cluster
// Postgres (ADR-0012) and applies migrations, drives the cluster through the client-go
// adapter (ADR-0011), resolves images through the registry resolver (ADR-0004), and
// serves the authenticated control-plane HTTP API (ADR-0005).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	store, err := openWithRetry(ctx, dsn, 2*time.Minute)
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

	var regOpts []registry.Option
	if os.Getenv("BURROW_REGISTRY_INSECURE") == "true" {
		regOpts = append(regOpts, registry.WithInsecure())
	}

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

	// One HTTP client shared across the observability adapters — burrowd reaches each backend
	// in-cluster.
	obsHTTP := &http.Client{Timeout: 20 * time.Second}
	engine, err := controlplane.New(controlplane.Deps{
		Kubernetes:  k8s,
		Registry:    registry.New(regOpts...),
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
	})
	if err != nil {
		return err
	}

	handler, err := api.New(api.Config{Engine: engine, Token: token})
	if err != nil {
		return err
	}
	apiHandler.Store(&handler)
	ready.Store(true)
	log.Printf("burrowd %s ready", version)
	return nil
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

// openWithRetry waits for the database to accept connections, retrying for up to timeout
// rather than crashing — so burrowd comes up gracefully alongside an in-cluster Postgres
// that is still starting, instead of crash-looping until it is ready.
func openWithRetry(ctx context.Context, dsn string, timeout time.Duration) (*postgres.Store, error) {
	deadline := time.Now().Add(timeout)
	for attempt := 1; ; attempt++ {
		store, err := postgres.Open(ctx, dsn)
		if err == nil {
			return store, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connecting to the database after %s: %w", timeout, err)
		}
		log.Printf("waiting for the database (attempt %d): %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
}
