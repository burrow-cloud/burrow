// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// stubLogs is an inline LogsQuerier that records the endpoint and bearer token it was queried with
// and returns a fixed record, so a QueryLogs test can assert the engine resolved the add-on from the
// registry and threaded the token through.
type stubLogs struct {
	endpoint string
	token    string
}

func (s *stubLogs) QueryLogs(_ context.Context, endpoint, _ string, _ int, token string) ([]cp.LogEntry, error) {
	s.endpoint = endpoint
	s.token = token
	return []cp.LogEntry{{Message: "hello"}}, nil
}

// stubMetrics is an inline MetricsQuerier that records the endpoint and bearer token it was queried
// with and returns a fixed sample, so a QueryMetrics test can assert the engine resolved the add-on
// from the registry and threaded the token through.
type stubMetrics struct {
	endpoint string
	token    string
}

func (s *stubMetrics) QueryMetrics(_ context.Context, endpoint, _ string, token string) ([]cp.MetricSample, error) {
	s.endpoint = endpoint
	s.token = token
	return []cp.MetricSample{{Value: "1"}}, nil
}

// newAddonEngine builds an engine with logs and metrics queriers wired, returning the seams a test
// needs to arrange and inspect the add-on registry.
func newAddonEngine(t *testing.T) (*cp.Engine, *fake.Database, *fake.Clock, *stubLogs, *fake.Credentials) {
	t.Helper()
	e, d, c, logs, _, creds := newAddonEngineFull(t)
	return e, d, c, logs, creds
}

// newAddonEngineFull is like newAddonEngine but also returns the metrics stub, for the metrics tests.
func newAddonEngineFull(t *testing.T) (*cp.Engine, *fake.Database, *fake.Clock, *stubLogs, *stubMetrics, *fake.Credentials) {
	t.Helper()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	c := fake.NewClock(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC))
	logs := &stubLogs{}
	mets := &stubMetrics{}
	creds := fake.NewCredentials()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Registry: fake.NewRegistry(), Database: d,
		Clock: c, IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: fake.NewDNSFactory(),
		Logs:    map[string]cp.LogsQuerier{"victorialogs": logs, "loki": logs},
		Metrics: map[string]cp.MetricsQuerier{"prometheus": mets, "victoriametrics": mets},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, d, c, logs, mets, creds
}

// TestInstallListQueryRemoveAddon exercises the full registry-backed lifecycle: install records
// the add-on in the database (with CreatedAt from the clock), list reads it back from the
// database with live readiness probed, query resolves the logs endpoint from the database, and
// remove tears down the cluster resources and deletes the registry row.
func TestInstallListQueryRemoveAddon(t *testing.T) {
	ctx := context.Background()
	e, d, c, logs, _ := newAddonEngine(t)

	info, err := e.InstallAddon(ctx, cp.AddonLogs, true)
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	if info.Name != "burrow-logs" || info.Mode != "installed" {
		t.Errorf("info = %+v, want burrow-logs installed", info)
	}
	if !info.CreatedAt.Equal(c.Now()) {
		t.Errorf("CreatedAt = %v, want %v from the clock", info.CreatedAt, c.Now())
	}

	// The add-on is recorded in the registry, independent of the cluster.
	if got, err := d.Addon(ctx, "burrow-logs"); err != nil || got.Mode != "installed" {
		t.Fatalf("registry Addon = %+v err=%v", got, err)
	}

	// List reads from the registry; the fake cluster reports the installed add-on ready.
	list, err := e.ListAddons(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "burrow-logs" || !list[0].Ready {
		t.Fatalf("ListAddons = %+v err=%v, want one ready burrow-logs", list, err)
	}

	// Query resolves the logs endpoint from the registry, not the cluster. The installed default is
	// unauthenticated (no SecretKey), so the querier is passed an empty token.
	if _, err := e.QueryLogs(ctx, "*", 10, ""); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if logs.endpoint == "" {
		t.Errorf("QueryLogs did not resolve an endpoint from the registry")
	}
	if logs.token != "" {
		t.Errorf("QueryLogs token = %q, want empty for an unauthenticated add-on", logs.token)
	}

	// Remove tears down and deletes the registry row.
	if err := e.RemoveAddon(ctx, "burrow-logs", true); err != nil {
		t.Fatalf("RemoveAddon: %v", err)
	}
	if _, err := d.Addon(ctx, "burrow-logs"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("after remove, registry Addon err = %v, want ErrNotFound", err)
	}
	if list, err := e.ListAddons(ctx); err != nil || len(list) != 0 {
		t.Errorf("after remove, ListAddons = %+v err=%v, want empty", list, err)
	}
}

// TestConnectAddon registers an existing backend (Loki) into the registry and queries it: connect
// records a connected add-on with the backend's derived capabilities and the given endpoint, and a
// logs query then dispatches to the querier keyed by that backend.
func TestConnectAddon(t *testing.T) {
	ctx := context.Background()
	e, d, c, logs, _ := newAddonEngine(t)

	info, err := e.ConnectAddon(ctx, "loki", "loki.observability.svc:3100", "", "")
	if err != nil {
		t.Fatalf("ConnectAddon: %v", err)
	}
	if info.Name != "loki" || info.Mode != "connected" || info.Backend != "loki" {
		t.Errorf("info = %+v, want connected loki backend", info)
	}
	if info.Endpoint != "loki.observability.svc:3100" || len(info.Capabilities) != 1 || info.Capabilities[0] != "logs" {
		t.Errorf("info = %+v, want endpoint + logs capability", info)
	}
	if !info.CreatedAt.Equal(c.Now()) {
		t.Errorf("CreatedAt = %v, want %v from the clock", info.CreatedAt, c.Now())
	}

	// The connected add-on is recorded in the registry; query resolves its endpoint and backend.
	if got, err := d.Addon(ctx, "loki"); err != nil || got.Mode != "connected" {
		t.Fatalf("registry Addon = %+v err=%v", got, err)
	}
	if _, err := e.QueryLogs(ctx, "{app=\"web\"}", 10, ""); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if logs.endpoint != "loki.observability.svc:3100" {
		t.Errorf("QueryLogs resolved endpoint %q, want the connected Loki endpoint", logs.endpoint)
	}

	// Connecting again upserts the endpoint.
	if _, err := e.ConnectAddon(ctx, "loki", "loki.new.svc:3100", "", ""); err != nil {
		t.Fatalf("re-ConnectAddon: %v", err)
	}
	if got, _ := d.Addon(ctx, "loki"); got.Endpoint != "loki.new.svc:3100" {
		t.Errorf("after re-connect, endpoint = %q, want updated", got.Endpoint)
	}
}

// TestConnectAddonAuthThreadsToken connects an authenticated backend, passing the bearer token
// VALUE (over what is, in production, burrowd's authenticated API). The engine writes it into the
// credential store via SetToken under the key, the registry records only the key, and a logs query
// reads the token back through the Credentials seam and threads it to the querier (ADR-0030/0023).
func TestConnectAddonAuthThreadsToken(t *testing.T) {
	ctx := context.Background()
	e, d, _, logs, creds := newAddonEngine(t)

	info, err := e.ConnectAddon(ctx, "loki", "loki.observability.svc:3100", "addon-loki", "s3cr3t")
	if err != nil {
		t.Fatalf("ConnectAddon: %v", err)
	}
	if info.SecretKey != "addon-loki" {
		t.Errorf("info.SecretKey = %q, want addon-loki", info.SecretKey)
	}
	// The engine wrote the token VALUE into the credential store under the key.
	if tok, ok := creds.Get("addon-loki"); !ok || tok != "s3cr3t" {
		t.Errorf("SetToken stored %q ok=%v, want s3cr3t true", tok, ok)
	}
	// The registry records only the key, never the token.
	if got, _ := d.Addon(ctx, "loki"); got.SecretKey != "addon-loki" {
		t.Errorf("registry SecretKey = %q, want addon-loki", got.SecretKey)
	}

	if _, err := e.QueryLogs(ctx, "*", 10, ""); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if logs.token != "s3cr3t" {
		t.Errorf("QueryLogs token = %q, want the token read from the Secret", logs.token)
	}
}

// TestConnectAddonTokenWithoutKeyInvalid rejects a token with no key to store it under, and writes
// nothing.
func TestConnectAddonTokenWithoutKeyInvalid(t *testing.T) {
	ctx := context.Background()
	e, d, _, _, _ := newAddonEngine(t)
	if _, err := e.ConnectAddon(ctx, "loki", "loki.svc:3100", "", "tok"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("token without key err = %v, want ErrInvalid", err)
	}
	if _, err := d.Addon(ctx, "loki"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("nothing should be recorded when the token has no key (got %v)", err)
	}
}

// TestConnectAddonInvalid rejects an unknown backend and an empty endpoint as ErrInvalid.
func TestConnectAddonInvalid(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newAddonEngine(t)
	if _, err := e.ConnectAddon(ctx, "nope", "x:1", "", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("ConnectAddon unknown backend err = %v, want ErrInvalid", err)
	}
	if _, err := e.ConnectAddon(ctx, "loki", "", "", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("ConnectAddon empty endpoint err = %v, want ErrInvalid", err)
	}
}

// TestRemoveAddonUnknown reports ErrNotFound when the add-on is not in the registry.
func TestRemoveAddonUnknown(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newAddonEngine(t)
	if err := e.RemoveAddon(ctx, "burrow-logs", true); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("RemoveAddon unknown err = %v, want ErrNotFound", err)
	}
}

// TestQueryLogsNoAddon reports ErrNotFound when no logs add-on is registered.
func TestQueryLogsNoAddon(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newAddonEngine(t)
	if _, err := e.QueryLogs(ctx, "*", 10, ""); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("QueryLogs with no add-on err = %v, want ErrNotFound", err)
	}
}

// TestQueryLogsBackendSelector registers two logs-capable add-ons — an installed VictoriaLogs
// ("burrow-logs") and a connected Loki ("loki") — and asserts the backend selector targets the
// right one: empty picks the first (the installed default), a concrete backend ("victorialogs") or
// a registry name ("loki") picks that add-on, and an unknown backend is ErrNotFound naming it. The
// recorded endpoint distinguishes which add-on the engine resolved.
func TestQueryLogsBackendSelector(t *testing.T) {
	ctx := context.Background()
	e, _, _, logs, _ := newAddonEngine(t)

	if _, err := e.InstallAddon(ctx, cp.AddonLogs, true); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	// Capture the installed default's resolved endpoint by querying it before a second add-on exists.
	if _, err := e.QueryLogs(ctx, "*", 10, ""); err != nil {
		t.Fatalf("QueryLogs (capture): %v", err)
	}
	installedEndpoint := logs.endpoint
	if installedEndpoint == "" {
		t.Fatalf("installed default did not resolve an endpoint")
	}
	if _, err := e.ConnectAddon(ctx, "loki", "loki.observability.svc:3100", "", ""); err != nil {
		t.Fatalf("ConnectAddon: %v", err)
	}

	// Empty backend keeps the historical first-match behavior — the installed default.
	logs.endpoint = ""
	if _, err := e.QueryLogs(ctx, "*", 10, ""); err != nil {
		t.Fatalf("QueryLogs empty backend: %v", err)
	}
	if logs.endpoint == "" || logs.endpoint == "loki.observability.svc:3100" {
		t.Errorf("empty backend resolved %q, want the installed default %q", logs.endpoint, installedEndpoint)
	}

	// A registry name selects the connected Loki.
	logs.endpoint = ""
	if _, err := e.QueryLogs(ctx, "*", 10, "loki"); err != nil {
		t.Fatalf("QueryLogs backend loki: %v", err)
	}
	if logs.endpoint != "loki.observability.svc:3100" {
		t.Errorf("backend loki resolved %q, want the connected Loki endpoint", logs.endpoint)
	}

	// A concrete backend selects the installed VictoriaLogs.
	logs.endpoint = ""
	if _, err := e.QueryLogs(ctx, "*", 10, "victorialogs"); err != nil {
		t.Fatalf("QueryLogs backend victorialogs: %v", err)
	}
	if logs.endpoint != installedEndpoint {
		t.Errorf("backend victorialogs resolved %q, want the installed default %q", logs.endpoint, installedEndpoint)
	}

	// An unknown backend is ErrNotFound naming the requested backend.
	_, err := e.QueryLogs(ctx, "*", 10, "nope")
	if !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("QueryLogs unknown backend err = %v, want ErrNotFound", err)
	}
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("QueryLogs unknown backend err = %v, want it to name the requested backend", err)
	}
}

// TestQueryMetricsBackendSelector mirrors the logs selector for metrics: with a connected Prometheus
// registered, a registry-name backend selects it and an unknown backend is ErrNotFound naming it.
func TestQueryMetricsBackendSelector(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, mets, _ := newAddonEngineFull(t)

	if _, err := e.ConnectAddon(ctx, "prometheus", "prometheus.monitoring.svc:9090", "", ""); err != nil {
		t.Fatalf("ConnectAddon: %v", err)
	}

	mets.endpoint = ""
	if _, err := e.QueryMetrics(ctx, "up", "prometheus"); err != nil {
		t.Fatalf("QueryMetrics backend prometheus: %v", err)
	}
	if mets.endpoint != "prometheus.monitoring.svc:9090" {
		t.Errorf("backend prometheus resolved %q, want the connected Prometheus endpoint", mets.endpoint)
	}

	_, err := e.QueryMetrics(ctx, "up", "nope")
	if !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("QueryMetrics unknown backend err = %v, want ErrNotFound", err)
	}
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("QueryMetrics unknown backend err = %v, want it to name the requested backend", err)
	}
}

// TestConnectMetricsAndQuery connects an existing Prometheus and queries it: connect records a
// connected add-on with the metrics capability, and a metrics query then dispatches to the querier
// keyed by that backend, resolving its endpoint from the registry.
func TestConnectMetricsAndQuery(t *testing.T) {
	ctx := context.Background()
	e, d, _, _, mets, _ := newAddonEngineFull(t)

	info, err := e.ConnectAddon(ctx, "prometheus", "prometheus.monitoring.svc:9090", "", "")
	if err != nil {
		t.Fatalf("ConnectAddon: %v", err)
	}
	if info.Mode != "connected" || info.Backend != "prometheus" ||
		len(info.Capabilities) != 1 || info.Capabilities[0] != "metrics" {
		t.Errorf("info = %+v, want connected prometheus with metrics capability", info)
	}
	if got, err := d.Addon(ctx, "prometheus"); err != nil || got.Mode != "connected" {
		t.Fatalf("registry Addon = %+v err=%v", got, err)
	}

	samples, err := e.QueryMetrics(ctx, `up{job="web"}`, "")
	if err != nil {
		t.Fatalf("QueryMetrics: %v", err)
	}
	if len(samples) != 1 || samples[0].Value != "1" {
		t.Errorf("samples = %+v, want one sample with value 1", samples)
	}
	if mets.endpoint != "prometheus.monitoring.svc:9090" {
		t.Errorf("QueryMetrics resolved endpoint %q, want the connected Prometheus endpoint", mets.endpoint)
	}
	if mets.token != "" {
		t.Errorf("QueryMetrics token = %q, want empty for an unauthenticated add-on", mets.token)
	}
}

// TestConnectMetricsAuthThreadsToken connects an authenticated Prometheus, passing the bearer token
// VALUE. The engine writes it into the credential store via SetToken under the key, and a metrics
// query reads it back through the Credentials seam and threads it to the querier (ADR-0030/0023).
func TestConnectMetricsAuthThreadsToken(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, mets, creds := newAddonEngineFull(t)

	if _, err := e.ConnectAddon(ctx, "prometheus", "prometheus.monitoring.svc:9090", "addon-prometheus", "s3cr3t"); err != nil {
		t.Fatalf("ConnectAddon: %v", err)
	}
	if tok, ok := creds.Get("addon-prometheus"); !ok || tok != "s3cr3t" {
		t.Errorf("SetToken stored %q ok=%v, want s3cr3t true", tok, ok)
	}
	if _, err := e.QueryMetrics(ctx, "up", ""); err != nil {
		t.Fatalf("QueryMetrics: %v", err)
	}
	if mets.token != "s3cr3t" {
		t.Errorf("QueryMetrics token = %q, want the token read from the Secret", mets.token)
	}
}

// TestQueryMetricsNoAddon reports ErrNotFound when no metrics add-on is registered.
func TestQueryMetricsNoAddon(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _, _ := newAddonEngineFull(t)
	if _, err := e.QueryMetrics(ctx, "up", ""); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("QueryMetrics with no add-on err = %v, want ErrNotFound", err)
	}
}
