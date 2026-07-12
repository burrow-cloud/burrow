// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// latestAuditArgs returns the args recorded on the newest audit row for op, so a test can assert the
// redacted metadata a guarded operation carried (ADR-0027).
func latestAuditArgs(t *testing.T, d *fake.Database, op string) map[string]string {
	t.Helper()
	rows := d.AuditRows()
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Operation == op {
			return rows[i].Args
		}
	}
	t.Fatalf("no audit row for operation %q", op)
	return nil
}

// TestDeployRecordsManualProvenance proves an explicit deploy stamps the release and the audit trail
// with a manual trigger — the default provenance for every deploy today (ADR-0052 §5).
func TestDeployRecordsManualProvenance(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Release.Trigger != cp.TriggerManual {
		t.Errorf("release trigger = %q, want manual", res.Release.Trigger)
	}
	if res.Release.Environment != "default" {
		t.Errorf("release environment = %q, want default", res.Release.Environment)
	}
	if res.Release.AutoLevel != "" || res.Release.AutoTag != "" {
		t.Errorf("manual release carries auto fields level=%q tag=%q, want none", res.Release.AutoLevel, res.Release.AutoTag)
	}
	if args := latestAuditArgs(t, d, "deploy"); args["trigger"] != "manual" {
		t.Errorf("audit trigger = %q, want manual", args["trigger"])
	}
}

// TestDeployAutoRecordsProvenance proves the shared deploy path stamps an AUTO deploy with the level
// and tag the watcher took, on the release and in the audit args (ADR-0052 §5). It drives the path
// the Phase 4b poller will use.
func TestDeployAutoRecordsProvenance(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())

	res, err := e.DeployAutoForTest(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.3.0", Replicas: 1}, cp.AutoDeployMinor, "1.3.0")
	if err != nil {
		t.Fatalf("DeployAutoForTest: %v", err)
	}
	if res.Release.Trigger != cp.TriggerAuto {
		t.Errorf("release trigger = %q, want auto", res.Release.Trigger)
	}
	if res.Release.AutoLevel != cp.AutoDeployMinor || res.Release.AutoTag != "1.3.0" {
		t.Errorf("release auto provenance = level %q tag %q, want minor/1.3.0", res.Release.AutoLevel, res.Release.AutoTag)
	}
	args := latestAuditArgs(t, d, "deploy")
	if args["trigger"] != "auto" || args["auto_level"] != "minor" || args["auto_tag"] != "1.3.0" {
		t.Errorf("audit args = %+v, want trigger=auto auto_level=minor auto_tag=1.3.0", args)
	}
}

// TestRollbackDisablesAutoDeploy proves a rollback turns auto-deploy off with the reason "disabled by
// rollback", so the watcher does not fight the deliberate downgrade (ADR-0052 §5). The reason surfaces
// through both AutoDeployReason and AutoDeployStatus.
func TestRollbackDisablesAutoDeploy(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy 1.0.0: %v", err)
	}
	// A forward manual deploy must NOT disable auto-deploy.
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.1.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy 1.1.0: %v", err)
	}
	if lvl, _ := e.AutoDeploy(ctx, "web", ""); lvl != cp.DefaultAutoDeployLevel {
		t.Fatalf("level after forward deploy = %q, want the default (unchanged)", lvl)
	}

	if _, err := e.Rollback(ctx, "web", "", true); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if lvl, _ := e.AutoDeploy(ctx, "web", ""); lvl != cp.AutoDeployOff {
		t.Errorf("level after rollback = %q, want off", lvl)
	}
	if reason, err := d.AutoDeployReason(ctx, "web", "default"); err != nil || reason != "disabled by rollback" {
		t.Errorf("AutoDeployReason = %q, err=%v, want disabled by rollback", reason, err)
	}
	st, err := e.AutoDeployStatus(ctx, "web", "")
	if err != nil {
		t.Fatalf("AutoDeployStatus: %v", err)
	}
	if st.Level != cp.AutoDeployOff || st.DisabledReason != "disabled by rollback" {
		t.Errorf("status = level %q reason %q, want off / disabled by rollback", st.Level, st.DisabledReason)
	}
}

// TestManualDowngradeDisablesAutoDeploy proves a manual deploy to a strictly lower semver disables
// auto-deploy with the reason "disabled by downgrade" (ADR-0052 §5).
func TestManualDowngradeDisablesAutoDeploy(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.2.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy 1.2.0: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.1.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy 1.1.0 (downgrade): %v", err)
	}
	if lvl, _ := e.AutoDeploy(ctx, "web", ""); lvl != cp.AutoDeployOff {
		t.Errorf("level after downgrade = %q, want off", lvl)
	}
	if reason, _ := d.AutoDeployReason(ctx, "web", "default"); reason != "disabled by downgrade" {
		t.Errorf("AutoDeployReason = %q, want disabled by downgrade", reason)
	}
}

// TestForwardManualDeployKeepsAutoDeploy proves a normal forward manual deploy leaves the level
// untouched — only a downgrade or rollback disables (ADR-0052 §5).
func TestForwardManualDeployKeepsAutoDeploy(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())

	// Pin the level to patch first, so the assertion proves the level is preserved, not just default.
	if err := e.SetAutoDeploy(ctx, "web", "", cp.AutoDeployPatch); err != nil {
		t.Fatalf("SetAutoDeploy: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.2.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy 1.2.0: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.3.0", Replicas: 1}); err != nil {
		t.Fatalf("Deploy 1.3.0 (forward): %v", err)
	}
	if lvl, _ := e.AutoDeploy(ctx, "web", ""); lvl != cp.AutoDeployPatch {
		t.Errorf("level after forward deploy = %q, want patch (unchanged)", lvl)
	}
	if reason, _ := d.AutoDeployReason(ctx, "web", "default"); reason != "" {
		t.Errorf("AutoDeployReason = %q, want empty (forward deploy does not disable)", reason)
	}
}

// TestRollbackDisableIsPerEnvironment proves disabling auto-deploy in one environment leaves another
// untouched: the level and reason are keyed per (app, environment) (ADR-0052 §5).
func TestRollbackDisableIsPerEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	// Two deploys in the default environment, then a rollback there. With prod also registered the
	// default must be named explicitly (ADR-0047).
	for _, img := range []string{"ghcr.io/u/web:1.0.0", "ghcr.io/u/web:1.1.0"} {
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: cp.DefaultEnvironment, Image: img, Replicas: 1}); err != nil {
			t.Fatalf("Deploy %s: %v", img, err)
		}
	}
	if _, err := e.Rollback(ctx, "web", cp.DefaultEnvironment, true); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if lvl, _ := e.AutoDeploy(ctx, "web", cp.DefaultEnvironment); lvl != cp.AutoDeployOff {
		t.Errorf("default level after rollback = %q, want off", lvl)
	}
	// prod was never touched, so it stays at the built-in default.
	if lvl, _ := e.AutoDeploy(ctx, "web", "prod"); lvl != cp.DefaultAutoDeployLevel {
		t.Errorf("prod level = %q, want the default (isolated from the default-env rollback)", lvl)
	}
}
