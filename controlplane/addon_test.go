// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// stubLogs is an inline LogsQuerier that records the endpoint it was queried at and returns a
// fixed record, so a QueryLogs test can assert the engine resolved the add-on from the registry.
type stubLogs struct {
	endpoint string
}

func (s *stubLogs) QueryLogs(_ context.Context, endpoint, _ string, _ int) ([]cp.LogEntry, error) {
	s.endpoint = endpoint
	return []cp.LogEntry{{Message: "hello"}}, nil
}

// newAddonEngine builds an engine with a logs querier wired, returning the seams a test needs to
// arrange and inspect the add-on registry.
func newAddonEngine(t *testing.T) (*cp.Engine, *fake.Database, *fake.Clock, *stubLogs) {
	t.Helper()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	c := fake.NewClock(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC))
	logs := &stubLogs{}
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Registry: fake.NewRegistry(), Database: d,
		Clock: c, IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		Logs: map[string]cp.LogsQuerier{"victorialogs": logs, "loki": logs},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, d, c, logs
}

// TestInstallListQueryRemoveAddon exercises the full registry-backed lifecycle: install records
// the add-on in the database (with CreatedAt from the clock), list reads it back from the
// database with live readiness probed, query resolves the logs endpoint from the database, and
// remove tears down the cluster resources and deletes the registry row.
func TestInstallListQueryRemoveAddon(t *testing.T) {
	ctx := context.Background()
	e, d, c, logs := newAddonEngine(t)

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

	// Query resolves the logs endpoint from the registry, not the cluster.
	if _, err := e.QueryLogs(ctx, "*", 10); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if logs.endpoint == "" {
		t.Errorf("QueryLogs did not resolve an endpoint from the registry")
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
	e, d, c, logs := newAddonEngine(t)

	info, err := e.ConnectAddon(ctx, "loki", "loki.observability.svc:3100")
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
	if _, err := e.QueryLogs(ctx, "{app=\"web\"}", 10); err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if logs.endpoint != "loki.observability.svc:3100" {
		t.Errorf("QueryLogs resolved endpoint %q, want the connected Loki endpoint", logs.endpoint)
	}

	// Connecting again upserts the endpoint.
	if _, err := e.ConnectAddon(ctx, "loki", "loki.new.svc:3100"); err != nil {
		t.Fatalf("re-ConnectAddon: %v", err)
	}
	if got, _ := d.Addon(ctx, "loki"); got.Endpoint != "loki.new.svc:3100" {
		t.Errorf("after re-connect, endpoint = %q, want updated", got.Endpoint)
	}
}

// TestConnectAddonInvalid rejects an unknown backend and an empty endpoint as ErrInvalid.
func TestConnectAddonInvalid(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newAddonEngine(t)
	if _, err := e.ConnectAddon(ctx, "nope", "x:1"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("ConnectAddon unknown backend err = %v, want ErrInvalid", err)
	}
	if _, err := e.ConnectAddon(ctx, "loki", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("ConnectAddon empty endpoint err = %v, want ErrInvalid", err)
	}
}

// TestRemoveAddonUnknown reports ErrNotFound when the add-on is not in the registry.
func TestRemoveAddonUnknown(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newAddonEngine(t)
	if err := e.RemoveAddon(ctx, "burrow-logs", true); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("RemoveAddon unknown err = %v, want ErrNotFound", err)
	}
}

// TestQueryLogsNoAddon reports ErrNotFound when no logs add-on is registered.
func TestQueryLogsNoAddon(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newAddonEngine(t)
	if _, err := e.QueryLogs(ctx, "*", 10); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("QueryLogs with no add-on err = %v, want ErrNotFound", err)
	}
}
