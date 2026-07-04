// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestStoreAuditAppendAndFilter round-trips audit rows through the real schema: append a handful
// for a test-unique target, then read them back newest-first and exercise the app/operation/
// outcome filters. Rows are scoped to t.Name()-prefixed targets so the test is isolated and
// idempotent against the shared database.
func TestStoreAuditAppendAndFilter(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"
	other := t.Name() + "-api"
	base := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	rows := []cp.AuditEntry{
		{Timestamp: base, Operation: "deploy", Target: app, Args: map[string]string{"image": "img:1", "replicas": "1"}, Outcome: cp.AuditAllowed, Disposition: "allow", Caller: "control-plane"},
		{Timestamp: base.Add(time.Second), Operation: "deploy", Target: app, Args: map[string]string{"env_keys": "API_KEY,DB_HOST"}, Outcome: cp.AuditExecuted, Caller: "control-plane"},
		{Timestamp: base.Add(2 * time.Second), Operation: "app_delete", Target: app, GuardrailCode: "app_delete", Disposition: "deny", Outcome: cp.AuditDenied, Caller: "control-plane"},
		{Timestamp: base.Add(3 * time.Second), Operation: "scale", Target: other, Args: map[string]string{"replicas": "5"}, Outcome: cp.AuditExecuted, Caller: "control-plane"},
	}
	for i, e := range rows {
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatalf("AppendAudit[%d]: %v", i, err)
		}
	}

	// Filter by app: all rows for `app`, newest first (the app_delete row was appended last).
	got, err := s.Audit(ctx, cp.AuditFilter{App: app})
	if err != nil {
		t.Fatalf("Audit(app): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Audit(app) returned %d rows, want 3", len(got))
	}
	if got[0].Operation != "app_delete" || got[0].Outcome != cp.AuditDenied {
		t.Errorf("newest row = %s/%s, want app_delete/denied", got[0].Operation, got[0].Outcome)
	}
	if got[0].ID == 0 {
		t.Errorf("store should assign an id, got 0")
	}
	// args round-trips, including the key-names-only execution row.
	var sawKeys bool
	for _, e := range got {
		if e.Args["env_keys"] == "API_KEY,DB_HOST" {
			sawKeys = true
		}
	}
	if !sawKeys {
		t.Errorf("env_keys args did not round-trip")
	}

	// Filter by operation within the app.
	deploys, err := s.Audit(ctx, cp.AuditFilter{App: app, Operation: "deploy"})
	if err != nil || len(deploys) != 2 {
		t.Fatalf("Audit(app, deploy) = %d rows, err=%v, want 2", len(deploys), err)
	}

	// Filter by outcome within the app.
	denied, err := s.Audit(ctx, cp.AuditFilter{App: app, Outcome: cp.AuditDenied})
	if err != nil || len(denied) != 1 || denied[0].Operation != "app_delete" {
		t.Fatalf("Audit(app, denied) = %+v, err=%v, want one app_delete", denied, err)
	}

	// Limit caps the result.
	one, err := s.Audit(ctx, cp.AuditFilter{App: app, Limit: 1})
	if err != nil || len(one) != 1 {
		t.Fatalf("Audit(app, limit 1) = %d rows, err=%v, want 1", len(one), err)
	}
}

// TestStoreAuditPrincipal round-trips the principal column (ADR-0038): a row written with a
// principal reads it back, and a row inserted the way a pre-migration writer would (no principal
// column) reads the schema default of empty string — the seeding the migration promises so a later SSO
// change fills a value rather than migrating stored rows' meaning.
func TestStoreAuditPrincipal(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"
	base := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	// Write-then-read: the principal survives the round trip.
	if err := s.AppendAudit(ctx, cp.AuditEntry{
		Timestamp: base, Operation: "deploy", Target: app,
		Outcome: cp.AuditExecuted, Caller: "control-plane", Principal: "shared-agent",
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	got, err := s.Audit(ctx, cp.AuditFilter{App: app})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Audit returned %d rows, want 1", len(got))
	}
	if got[0].Principal != "shared-agent" {
		t.Errorf("principal = %q, want shared-agent", got[0].Principal)
	}
	if got[0].Caller != "control-plane" {
		t.Errorf("caller = %q, want control-plane (distinct from principal)", got[0].Caller)
	}

	// Pre-existing row: insert directly WITHOUT the principal column (as a writer predating the
	// migration would), then confirm the read sees the DEFAULT '' rather than erroring.
	dsn := os.Getenv("BURROW_TEST_DATABASE_URL") // openStore already skipped if unset
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	other := t.Name() + "-legacy"
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO audit_log (ts, operation, target, outcome, caller) VALUES ($1, $2, $3, $4, $5)`,
		base, "deploy", other, string(cp.AuditExecuted), "control-plane"); err != nil {
		t.Fatalf("raw insert without principal: %v", err)
	}
	legacy, err := s.Audit(ctx, cp.AuditFilter{App: other})
	if err != nil {
		t.Fatalf("Audit(legacy): %v", err)
	}
	if len(legacy) != 1 {
		t.Fatalf("Audit(legacy) returned %d rows, want 1", len(legacy))
	}
	if legacy[0].Principal != "" {
		t.Errorf("pre-existing row principal = %q, want empty (schema default)", legacy[0].Principal)
	}
}

// TestStoreAuditClientVersion round-trips the client_version column (ADR-0039): a row written with a
// client version reads it back, and a row inserted the way a pre-migration writer would (no
// client_version column) reads the schema default of empty — the seeding the migration promises so
// adding the column migrates no stored row's meaning.
func TestStoreAuditClientVersion(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := t.Name() + "-web"
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	// Write-then-read: the client version survives the round trip.
	if err := s.AppendAudit(ctx, cp.AuditEntry{
		Timestamp: base, Operation: "deploy", Target: app,
		Outcome: cp.AuditExecuted, Caller: "control-plane", Principal: "shared-agent", ClientVersion: "v0.9.0",
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	got, err := s.Audit(ctx, cp.AuditFilter{App: app})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Audit returned %d rows, want 1", len(got))
	}
	if got[0].ClientVersion != "v0.9.0" {
		t.Errorf("client version = %q, want v0.9.0", got[0].ClientVersion)
	}

	// Pre-existing row: insert directly WITHOUT the client_version column (as a writer predating the
	// migration would), then confirm the read sees the DEFAULT '' rather than erroring.
	dsn := os.Getenv("BURROW_TEST_DATABASE_URL") // openStore already skipped if unset
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	other := t.Name() + "-legacy"
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO audit_log (ts, operation, target, outcome, caller) VALUES ($1, $2, $3, $4, $5)`,
		base, "deploy", other, string(cp.AuditExecuted), "control-plane"); err != nil {
		t.Fatalf("raw insert without client_version: %v", err)
	}
	legacy, err := s.Audit(ctx, cp.AuditFilter{App: other})
	if err != nil {
		t.Fatalf("Audit(legacy): %v", err)
	}
	if len(legacy) != 1 {
		t.Fatalf("Audit(legacy) returned %d rows, want 1", len(legacy))
	}
	if legacy[0].ClientVersion != "" {
		t.Errorf("pre-existing row client version = %q, want empty (schema default)", legacy[0].ClientVersion)
	}
}
