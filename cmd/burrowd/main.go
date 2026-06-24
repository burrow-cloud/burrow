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
	"syscall"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/api"
	"github.com/burrow-cloud/burrow/controlplane/kube"
	"github.com/burrow-cloud/burrow/controlplane/postgres"
	"github.com/burrow-cloud/burrow/controlplane/registry"
	"github.com/burrow-cloud/burrow/controlplane/sys"
)

// version is the Burrow version this binary reports and stamps into the database for
// the upgrade gate (ADR-0013). A release build may override it via -ldflags.
var version = "v0.1.0"

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
	store, err := openWithRetry(ctx, dsn, 2*time.Minute)
	if err != nil {
		return err
	}
	defer store.Close()
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

	var regOpts []registry.Option
	if os.Getenv("BURROW_REGISTRY_INSECURE") == "true" {
		regOpts = append(regOpts, registry.WithInsecure())
	}

	engine, err := controlplane.New(controlplane.Deps{
		Kubernetes: k8s,
		Registry:   registry.New(regOpts...),
		Database:   store,
		Clock:      sys.Clock{},
		IDs:        sys.IDs{},
		Policy:     controlplane.DefaultPolicy(),
	})
	if err != nil {
		return err
	}

	handler, err := api.New(api.Config{Engine: engine, Token: token})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return serve(srv)
}

// serve runs the HTTP server and shuts it down gracefully on SIGINT/SIGTERM.
func serve(srv *http.Server) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Printf("burrowd v%s listening on %s", version, srv.Addr)
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
