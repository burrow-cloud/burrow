// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// errBoom is a generic injected failure for the audit best-effort test.
var errBoom = errors.New("boom")

// auditRows returns the audit entries the fake captured for an operation, oldest first.
func auditRows(t *testing.T, d *fake.Database, op string) []cp.AuditEntry {
	t.Helper()
	var out []cp.AuditEntry
	for _, e := range d.AuditRows() {
		if e.Operation == op {
			out = append(out, e)
		}
	}
	return out
}

// TestAuditDeployAllowedRecordsExecuted: a normal allowed deploy records an allowed decision
// row and an executed row, and never executes a denied or held variant.
func TestAuditDeployAllowedRecordsExecuted(t *testing.T) {
	e, _, r, d, _ := newEngine(t, permissive())
	ctx := context.Background()
	r.Add("img:1", "sha256:deadbeef")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	rows := auditRows(t, d, "deploy")
	if len(rows) != 2 {
		t.Fatalf("deploy audit rows = %d, want 2 (decision + execution)", len(rows))
	}
	if rows[0].Outcome != cp.AuditAllowed {
		t.Errorf("decision outcome = %q, want allowed", rows[0].Outcome)
	}
	if rows[0].Target != "web" {
		t.Errorf("target = %q, want web", rows[0].Target)
	}
	if rows[0].Args["image"] != "img:1" || rows[0].Args["replicas"] != "2" {
		t.Errorf("decision args = %v, want image=img:1 replicas=2", rows[0].Args)
	}
	if rows[1].Outcome != cp.AuditExecuted {
		t.Errorf("execution outcome = %q, want executed", rows[1].Outcome)
	}
	if rows[0].Caller == "" || rows[1].Caller == "" {
		t.Errorf("caller should be populated, got %q / %q", rows[0].Caller, rows[1].Caller)
	}
	// The timestamp comes from the injected clock (2026-06-23T12:00:00Z in the harness).
	if rows[0].Timestamp.IsZero() {
		t.Errorf("timestamp should come from the clock, got zero")
	}
}

// TestAuditScaleHeldDoesNotExecute: a confirm-disposition scale with no confirm records a held
// row and does NOT execute (no executed row, no scale on the cluster).
func TestAuditScaleHeldDoesNotExecute(t *testing.T) {
	pol := permissive().With(cp.GuardrailReplicaCeiling, cp.DispositionConfirm)
	pol.MaxReplicas = 3
	e, k, r, d, _ := newEngine(t, pol)
	ctx := context.Background()
	r.Add("img:1", "sha256:deadbeef")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// 5 exceeds the ceiling of 3, so app.replica_ceiling holds it for confirmation.
	if _, err := e.Scale(ctx, "web", "", 5, false); err == nil {
		t.Fatalf("Scale without confirm should be held")
	}

	rows := auditRows(t, d, "scale")
	if len(rows) != 1 {
		t.Fatalf("scale audit rows = %d, want 1 (held decision only)", len(rows))
	}
	if rows[0].Outcome != cp.AuditHeld {
		t.Errorf("outcome = %q, want held", rows[0].Outcome)
	}
	if rows[0].GuardrailCode != string(cp.GuardrailReplicaCeiling) {
		t.Errorf("guardrail = %q, want app.replica_ceiling", rows[0].GuardrailCode)
	}
	if rows[0].Disposition != string(cp.DispositionConfirm) {
		t.Errorf("disposition = %q, want confirm", rows[0].Disposition)
	}
	// Not executed: the workload still has 1 replica.
	st, err := k.WorkloadStatus(ctx, "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.DesiredReplicas != 1 {
		t.Errorf("replicas = %d, want 1 (held op must not execute)", st.DesiredReplicas)
	}
}

// TestAuditScaleConfirmedExecutes: the same scale with confirm records executed and applies.
func TestAuditScaleConfirmedExecutes(t *testing.T) {
	pol := permissive().With(cp.GuardrailReplicaCeiling, cp.DispositionConfirm)
	pol.MaxReplicas = 3
	e, k, r, d, _ := newEngine(t, pol)
	ctx := context.Background()
	r.Add("img:1", "sha256:deadbeef")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if _, err := e.Scale(ctx, "web", "", 5, true); err != nil {
		t.Fatalf("Scale with confirm: %v", err)
	}

	rows := auditRows(t, d, "scale")
	if len(rows) != 2 {
		t.Fatalf("scale audit rows = %d, want 2 (allowed decision + executed)", len(rows))
	}
	if rows[0].Outcome != cp.AuditAllowed {
		t.Errorf("decision outcome = %q, want allowed", rows[0].Outcome)
	}
	if rows[1].Outcome != cp.AuditExecuted {
		t.Errorf("execution outcome = %q, want executed", rows[1].Outcome)
	}
	if st, _ := k.WorkloadStatus(ctx, "web"); st.DesiredReplicas != 5 {
		t.Errorf("replicas = %d, want 5", st.DesiredReplicas)
	}
}

// TestAuditAppDeleteDeniedDoesNotExecute: a deny-disposition app delete records denied and does
// NOT tear the app down.
func TestAuditAppDeleteDeniedDoesNotExecute(t *testing.T) {
	pol := permissive().With(cp.GuardrailAppDelete, cp.DispositionDeny)
	e, k, r, d, _ := newEngine(t, pol)
	ctx := context.Background()
	r.Add("img:1", "sha256:deadbeef")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if err := e.DeleteApp(ctx, "web", "", true); err == nil {
		t.Fatalf("DeleteApp under deny should be refused")
	}

	rows := auditRows(t, d, "app_delete")
	if len(rows) != 1 {
		t.Fatalf("app_delete audit rows = %d, want 1 (denied decision only)", len(rows))
	}
	if rows[0].Outcome != cp.AuditDenied {
		t.Errorf("outcome = %q, want denied", rows[0].Outcome)
	}
	if rows[0].Disposition != string(cp.DispositionDeny) {
		t.Errorf("disposition = %q, want deny", rows[0].Disposition)
	}
	// Not executed: the workload still exists.
	if _, err := k.WorkloadStatus(ctx, "web"); err != nil {
		t.Errorf("workload should survive a denied delete, got err %v", err)
	}
}

// TestAuditRedactsEnvValues asserts the audit log records env KEY NAMES only, never values. A
// secret-shaped value set on the app must not appear anywhere in any recorded row.
func TestAuditRedactsEnvValues(t *testing.T) {
	e, _, r, d, _ := newEngine(t, permissive())
	ctx := context.Background()
	r.Add("img:1", "sha256:deadbeef")

	const secretValue = "super-secret-token-value"
	if err := d.SetAppEnv(ctx, "web", "API_KEY", secretValue); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	if err := d.SetAppEnv(ctx, "web", "DB_HOST", "db.internal"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	rows := auditRows(t, d, "deploy")
	var sawKeys bool
	for _, e := range rows {
		for k, v := range e.Args {
			if strings.Contains(v, secretValue) || strings.Contains(v, "db.internal") {
				t.Fatalf("audit args %s=%q leaked an env value", k, v)
			}
		}
		if keys := e.Args["env_keys"]; keys != "" {
			sawKeys = true
			if !strings.Contains(keys, "API_KEY") || !strings.Contains(keys, "DB_HOST") {
				t.Errorf("env_keys = %q, want the key names API_KEY and DB_HOST", keys)
			}
		}
	}
	if !sawKeys {
		t.Errorf("expected an execution row recording the env KEY NAMES")
	}
}

// TestAuditAppendFailureDoesNotFailOperation: an audit append error is swallowed — the deploy
// still succeeds (the record is best-effort relative to the action, ADR-0027).
func TestAuditAppendFailureDoesNotFailOperation(t *testing.T) {
	e, _, r, d, _ := newEngine(t, permissive())
	ctx := context.Background()
	r.Add("img:1", "sha256:deadbeef")
	d.SetError(fake.OpAppendAudit, errBoom)

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy should succeed despite an audit append failure, got %v", err)
	}
}
