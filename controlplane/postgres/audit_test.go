// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
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
