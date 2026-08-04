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

// A deploy reports its stages so an operator waiting on it can tell a slow image pull from a hung
// command (issue #480). The property under test throughout is that IT REPORTS ONLY WORK THAT
// HAPPENED: an app with nothing to wait for reports the one stage it ran, and every extra stage in a
// sequence corresponds to a feature that app actually has.

// recordStages collects a deploy's events as "stage:status", in order.
func recordStages(got *[]string) func(cp.DeployEvent) {
	return func(ev cp.DeployEvent) { *got = append(*got, ev.Stage+":"+ev.Status) }
}

// wantStages fails unless got is exactly want, rendered so a mismatch reads as the deploy does.
func wantStages(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stages = %v, want %v", got, want)
	}
}

// TestDeployStagesAreClosedVocabulary guards the set: a stage or status added without a decision
// would reach an agent branching on the value, and a name removed would silently stop being
// reported. It is the same guard DependencyReasons carries, for the same reason.
func TestDeployStagesAreClosedVocabulary(t *testing.T) {
	wantStageNames := []string{"pre-deploy-hook", "apply", "settle", "dependency-check", "post-deploy-hook"}
	got := cp.DeployStages()
	if strings.Join(got, ",") != strings.Join(wantStageNames, ",") {
		t.Errorf("DeployStages() = %v, want %v (in the order a deploy runs them)", got, wantStageNames)
	}
	for _, s := range wantStageNames {
		if !cp.IsDeployStage(s) {
			t.Errorf("IsDeployStage(%q) = false", s)
		}
	}
	for _, s := range []string{"", "Apply", "rollout", "pre-rollback-hook"} {
		if cp.IsDeployStage(s) {
			t.Errorf("IsDeployStage(%q) = true, want false — the set is closed", s)
		}
	}
	wantStatuses := []string{"started", "done", "failed"}
	if strings.Join(cp.DeployStatuses(), ",") != strings.Join(wantStatuses, ",") {
		t.Errorf("DeployStatuses() = %v, want %v", cp.DeployStatuses(), wantStatuses)
	}
	for _, s := range wantStatuses {
		if !cp.IsDeployStatus(s) {
			t.Errorf("IsDeployStatus(%q) = false", s)
		}
	}
	if cp.IsDeployStatus("running") {
		t.Error(`IsDeployStatus("running") = true, want false`)
	}
	// The accessors must hand back copies: a caller that sorts or appends to the returned slice must
	// not be editing the catalogue.
	cp.DeployStages()[0] = "clobbered"
	if cp.DeployStages()[0] == "clobbered" {
		t.Error("DeployStages() shares its backing array with the catalogue")
	}
}

// TestBareDeployReportsOnlyTheApply is the case the feature must not spoil. An app with no hook and
// nothing Burrow provisioned waits for nothing at all — the settle stays lazy and unobserved — so
// the deploy reports the single thing it did. A settle stage here would mean the reporting had
// started forcing a wait nobody asked for.
func TestBareDeployReportsOnlyTheApply(t *testing.T) {
	e, _, _, _ := newEngine(t, permissive())
	var got []string
	if _, err := e.Deploy(context.Background(), cp.DeployRequest{
		App: "web", Image: "img:1", Replicas: 1, Progress: recordStages(&got),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	wantStages(t, got, []string{"apply:started", "apply:done"})
}

// TestDeployWithADerivedDependencyReportsTheSettleAndTheCheck is the sequence an app with a database
// or a published port sees: the apply, then the rollout wait the check forces, then the check. It is
// where most of the ten to twenty seconds went.
func TestDeployWithADerivedDependencyReportsTheSettleAndTheCheck(t *testing.T) {
	e, _, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")

	var got []string
	if _, err := e.Deploy(ctx, cp.DeployRequest{
		App: "web", Image: "img:1", Replicas: 1, Progress: recordStages(&got),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	wantStages(t, got, []string{
		"apply:started", "apply:done",
		"settle:started", "settle:done",
		"dependency-check:started", "dependency-check:done",
	})
}

// TestDeployReportsBothHooks pins the two hook stages, and pins that they are reported only when a
// hook is CONFIGURED. The no-hook rows are the point: a deploy must never print a stage for a Job
// that never ran.
func TestDeployReportsBothHooks(t *testing.T) {
	cases := []struct {
		name  string
		hooks []cp.HookPhase
		want  []string
	}{
		{
			name: "no hooks at all",
			want: []string{"apply:started", "apply:done"},
		},
		{
			name:  "a pre-deploy hook only",
			hooks: []cp.HookPhase{cp.HookPreDeploy},
			want: []string{
				"pre-deploy-hook:started", "pre-deploy-hook:done",
				"apply:started", "apply:done",
			},
		},
		{
			name:  "a post-deploy hook only, which is what forces the settle",
			hooks: []cp.HookPhase{cp.HookPostDeploy},
			want: []string{
				"apply:started", "apply:done",
				"settle:started", "settle:done",
				"post-deploy-hook:started", "post-deploy-hook:done",
			},
		},
		{
			name:  "both",
			hooks: []cp.HookPhase{cp.HookPreDeploy, cp.HookPostDeploy},
			want: []string{
				"pre-deploy-hook:started", "pre-deploy-hook:done",
				"apply:started", "apply:done",
				"settle:started", "settle:done",
				"post-deploy-hook:started", "post-deploy-hook:done",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _, d, _ := newEngine(t, permissive())
			ctx := context.Background()
			for _, phase := range c.hooks {
				if err := d.SetAppHook(ctx, "web", cp.DefaultEnvironment, phase, []string{"./migrate"}); err != nil {
					t.Fatalf("SetAppHook: %v", err)
				}
			}
			var got []string
			if _, err := e.Deploy(ctx, cp.DeployRequest{
				App: "web", Image: "img:1", Replicas: 1, Progress: recordStages(&got),
			}); err != nil {
				t.Fatalf("Deploy: %v", err)
			}
			wantStages(t, got, c.want)
		})
	}
}

// TestAFailingPreDeployHookReportsAFailedStage asserts the stage that aborts the deploy is marked
// failed rather than left open — the CLI has already printed the start of that line and needs
// something to close it with.
func TestAFailingPreDeployHookReportsAFailedStage(t *testing.T) {
	e, k, d, _ := newEngine(t, permissive())
	ctx := context.Background()
	if err := d.SetAppHook(ctx, "web", cp.DefaultEnvironment, cp.HookPreDeploy, []string{"./migrate"}); err != nil {
		t.Fatalf("SetAppHook: %v", err)
	}
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: "migration 003 failed\n"})

	var got []string
	_, err := e.Deploy(ctx, cp.DeployRequest{
		App: "web", Image: "img:1", Replicas: 1, Progress: recordStages(&got),
	})
	if _, ok := cp.AsHook(err); !ok {
		t.Fatalf("Deploy err = %v, want a HookError", err)
	}
	wantStages(t, got, []string{"pre-deploy-hook:started", "pre-deploy-hook:failed"})
}

// TestAFailingApplyReportsAFailedStage covers the other aborting stage.
func TestAFailingApplyReportsAFailedStage(t *testing.T) {
	e, k, _, _ := newEngine(t, permissive())
	k.SetError(fake.OpApply, errors.New("the API server said no"))

	var got []string
	if _, err := e.Deploy(context.Background(), cp.DeployRequest{
		App: "web", Image: "img:1", Replicas: 1, Progress: recordStages(&got),
	}); err == nil {
		t.Fatal("Deploy succeeded with a failing apply")
	}
	wantStages(t, got, []string{"apply:started", "apply:failed"})
}

// TestARolloutThatDidNotSettleIsAFailedStageOnASucceedingDeploy is the asymmetry ADR-0072 §6 sets
// out, made visible: the wait ended badly and says so, and the deploy still succeeded because the
// image is live and the release is recorded. The mark is about the stage, not the verdict.
func TestARolloutThatDidNotSettleIsAFailedStageOnASucceedingDeploy(t *testing.T) {
	e, k, d, _ := newEngine(t, permissive())
	ctx := context.Background()
	if err := d.SetAppHook(ctx, "web", cp.DefaultEnvironment, cp.HookPostDeploy, []string{"./notify"}); err != nil {
		t.Fatalf("SetAppHook: %v", err)
	}
	k.SetRolloutOutcome("web", cp.RolloutOutcome{
		Reason: cp.ReasonCrashLoopBackOff,
		Detail: "1 of 1 replicas updated, 0 ready",
	})

	var got []string
	res, err := e.Deploy(ctx, cp.DeployRequest{
		App: "web", Image: "img:1", Replicas: 1, Progress: recordStages(&got),
	})
	if err != nil {
		t.Fatalf("Deploy: %v — a rollout that did not settle must not fail the deploy", err)
	}
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want deployed", res.Release.Status)
	}
	wantStages(t, got, []string{
		"apply:started", "apply:done",
		"settle:started", "settle:failed",
		"post-deploy-hook:started", "post-deploy-hook:done",
	})
}

// TestTheSettleIsReportedOnceHoweverManyConsumersAskForIt pins the reporting to the shared
// observation (issue #407): an app with BOTH a derived dependency and a `post-deploy` hook waits
// once and therefore reports once. Two settle stages would mean the deploy had gone back to waiting
// twice, which is the defect settleOnce exists to prevent.
func TestTheSettleIsReportedOnceHoweverManyConsumersAskForIt(t *testing.T) {
	e, _, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	if err := d.SetAppHook(ctx, "web", cp.DefaultEnvironment, cp.HookPostDeploy, []string{"./notify"}); err != nil {
		t.Fatalf("SetAppHook: %v", err)
	}

	var got []string
	if _, err := e.Deploy(ctx, cp.DeployRequest{
		App: "web", Image: "img:1", Replicas: 1, Progress: recordStages(&got),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	settles := 0
	for _, s := range got {
		if s == "settle:started" {
			settles++
		}
	}
	if settles != 1 {
		t.Fatalf("stages = %v, want exactly one settle:started", got)
	}
}

// TestADeployThatAskedForNoProgressStillWorks pins the nil normalisation: every existing caller —
// the auto-deploy watcher, an in-cluster build, `burrow-agent` — sets no reporter, and the deploy
// path must not carry a nil check per emit site to survive that.
func TestADeployThatAskedForNoProgressStillWorks(t *testing.T) {
	e, _, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	if err := d.SetAppHook(ctx, "web", cp.DefaultEnvironment, cp.HookPostDeploy, []string{"./notify"}); err != nil {
		t.Fatalf("SetAppHook: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy with no Progress: %v", err)
	}
}

// TestNothingIsReportedBeforeTheGuardrailDecides is the engine half of what makes a held deploy
// deliverable as a status-coded refusal: the API writes the stream's header on the first event, so a
// hold or a denial must produce none.
func TestNothingIsReportedBeforeTheGuardrailDecides(t *testing.T) {
	for _, disposition := range []cp.Disposition{cp.DispositionConfirm, cp.DispositionDeny} {
		t.Run(string(disposition), func(t *testing.T) {
			e, _, _, _ := newEngine(t, cp.Policy{}.With(cp.GuardrailAppDeploy, disposition))
			var got []string
			_, err := e.Deploy(context.Background(), cp.DeployRequest{
				App: "web", Image: "img:1", Replicas: 1, Progress: recordStages(&got),
			})
			if _, ok := cp.AsGuardrail(err); !ok {
				t.Fatalf("Deploy err = %v, want a guardrail refusal", err)
			}
			if len(got) != 0 {
				t.Fatalf("stages = %v, want none before the guardrail decides", got)
			}
		})
	}
}

// TestRollbackReportsNoProgress pins the scope: rollback reaches settleOnce and runPostDeployHook
// too, and issue #480 is about the deploy. Widening the reported surface is a separate change with
// its own client and CLI half.
func TestRollbackReportsNoProgress(t *testing.T) {
	e, _, d, _ := newEngine(t, permissive())
	ctx := context.Background()
	if err := d.SetAppHook(ctx, "web", cp.DefaultEnvironment, cp.HookPostDeploy, []string{"./notify"}); err != nil {
		t.Fatalf("SetAppHook: %v", err)
	}
	for _, img := range []string{"img:1", "img:2"} {
		if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: img, Replicas: 1}); err != nil {
			t.Fatalf("Deploy %s: %v", img, err)
		}
	}
	// A rollback takes no reporter at all, so there is nothing to assert but that it still runs.
	if _, err := e.Rollback(ctx, "web", "", true); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}
