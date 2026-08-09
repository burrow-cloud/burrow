// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// A rollback reports the ROLLOUT, not the submission (ADR-0093, issue #548). Before this it answered
// as soon as the API server accepted the Deployment, and the sentence it answered with named the
// release being rolled back AWAY FROM as superseded — which, when the restored image never became
// ready, is the release Kubernetes is still running. The tests here pin the three outcomes and, above
// all, what a failed rollback leaves behind for the operator who is already recovering.

// wedgedRollback deploys two releases and then jams the rollout, so a rollback from the second back
// to the first is applied but never becomes ready. It returns the two releases in the order they were
// deployed: the first is what a rollback returns TO, the second what it moves AWAY FROM.
func wedgedRollback(ctx context.Context, t *testing.T, e *cp.Engine, k *fake.Kubernetes, reason string) (string, string) {
	t.Helper()
	v1, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	v2, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	k.SetWedgedRollout("web", reason)
	return v1.Release.ID, v2.Release.ID
}

// TestRollbackReportsARolloutThatSettled is the ordinary recovery: the older image came back up, so
// the line rollback has always printed is now one that was checked before it was said.
func TestRollbackReportsARolloutThatSettled(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())

	v1, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	back, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.Rollout == nil || !back.Rollout.Settled {
		t.Fatalf("Rollout = %+v, want a settled observation", back.Rollout)
	}
	if back.RolledBackToReleaseID != v1.Release.ID {
		t.Errorf("rolled back to %q, want %q", back.RolledBackToReleaseID, v1.Release.ID)
	}
	rel, _ := d.Release(ctx, back.Release.ID)
	if rel.Status != cp.ReleaseDeployed || rel.Rollout != cp.RolloutSettled || rel.RolloutReason != "" {
		t.Errorf("recorded release = (%q, %q, %q), want (deployed, settled, no reason)", rel.Status, rel.Rollout, rel.RolloutReason)
	}
}

// TestRollbackReportsARolloutThatDidNotSettle is the defect. The rollback must come back saying the
// restored image did not become ready, carrying the pod's own reason, and the record must say the
// same — while the row still reads `deployed`, because that word is what the NEXT rollback walks back
// from and there has to be one.
func TestRollbackReportsARolloutThatDidNotSettle(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	v1, v2 := wedgedRollback(ctx, t, e, k, cp.ReasonCrashLoopBackOff)
	k.SetIssue("web", cp.IssueEvidence{
		Reason:    cp.ReasonCrashLoopBackOff,
		Container: "web",
		ExitCode:  1,
		LogTail:   "migration 0041 is already applied",
	})

	back, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.Rollout == nil || back.Rollout.Settled {
		t.Fatalf("Rollout = %+v, want an observation that did not settle", back.Rollout)
	}
	if back.Rollout.Reason != cp.ReasonCrashLoopBackOff {
		t.Errorf("reason = %q, want %q from the closed set", back.Rollout.Reason, cp.ReasonCrashLoopBackOff)
	}
	// The diagnosis, not merely the verdict. A rollback that does not come up is the moment a second
	// call to find out why costs the most, so the pod's own reason travels with the answer.
	if !strings.Contains(back.Rollout.Issue, "migration 0041 is already applied") {
		t.Errorf("Issue = %q, want the pod's own reason, which the status surface already had", back.Rollout.Issue)
	}
	if back.RolledBackToReleaseID != v1 {
		t.Errorf("rolled back to %q, want %q", back.RolledBackToReleaseID, v1)
	}
	// The release being rolled back AWAY FROM is what the report has to name as still serving, so the
	// result has to carry it whatever the rollout did.
	if back.SupersededReleaseID != v2 {
		t.Errorf("SupersededReleaseID = %q, want %q — the release still serving", back.SupersededReleaseID, v2)
	}
	rel, _ := d.Release(ctx, back.Release.ID)
	if rel.Rollout != cp.RolloutUnsettled || rel.RolloutReason != cp.ReasonCrashLoopBackOff {
		t.Errorf("recorded rollout = (%q, %q), want (unsettled, %s)", rel.Rollout, rel.RolloutReason, cp.ReasonCrashLoopBackOff)
	}
	if rel.Status != cp.ReleaseDeployed {
		t.Errorf("recorded status = %q, want deployed", rel.Status)
	}
}

// TestAFailedRollbackLeavesRollbackWorking is the reason the row above stays `deployed` (ADR-0093
// §3). Marking it failed while superseding the release it replaces would leave the app with NO
// deployed release, and rollback selects by that word: the operator who just watched a rollback fail
// would find the recovery command itself broken.
//
// It also pins the trap the report exists to name: the next rollback returns to the release this one
// was moving away from, not further back. That is correct — the chain is intact — and it is not what
// an operator reaching for rollback a second time expects.
func TestAFailedRollbackLeavesRollbackWorking(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	_, v2 := wedgedRollback(ctx, t, e, k, cp.ReasonImagePullBackOff)

	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true}); err != nil {
		t.Fatalf("first Rollback: %v", err)
	}
	again, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true})
	if err != nil {
		t.Fatalf("second Rollback: %v — a failed rollback must not break the recovery command", err)
	}
	if again.RolledBackToReleaseID != v2 {
		t.Errorf("second rollback returned to %q, want %q — the release the first rollback moved away from", again.RolledBackToReleaseID, v2)
	}
}

// TestARollbackSettlesOnce holds issue #407's rule on the second operation that waits. The rollback's
// own report and the `post-deploy` hook are two consumers of ONE observation, so a rollback with a
// hook spends the settle bound once — not once per party — and the two cannot disagree.
func TestARollbackSettlesOnce(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")
	before := len(k.Rollouts())

	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := len(k.Rollouts()) - before; got != 1 {
		t.Errorf("AwaitRollout calls during the rollback = %d, want exactly 1: the report and the hook share one observation", got)
	}
}

// TestARollbackToldNotToWaitReportsNothing is ADR-0093 §2's escape hatch, and what it must NOT do: a
// rollback that declined the wait reports an unknown outcome, and records one, rather than falling
// back to the sentence that asserted a recovery nobody observed.
func TestARollbackToldNotToWaitReportsNothing(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	before := len(k.Rollouts())

	back, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true, NoWait: true})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := len(k.Rollouts()) - before; got != 0 {
		t.Errorf("AwaitRollout calls = %d, want 0 for a rollback that declined to wait", got)
	}
	if back.Rollout != nil {
		t.Errorf("Rollout = %+v, want nil: an unobserved rollout has nothing truthful to report", back.Rollout)
	}
	rel, _ := d.Release(ctx, back.Release.ID)
	if rel.Rollout != cp.RolloutUnobserved {
		t.Errorf("recorded rollout = %q, want %q", rel.Rollout, cp.RolloutUnobserved)
	}
}

// TestAnUnsettledRollbackReachesTheAuditTrail keeps the record and the trail in step (ADR-0027).
// "Was the recovery real?" is the question a reviewer asks of a rollback row, and until the rollout
// was recorded beside it the row said only that a rollback was executed.
func TestAnUnsettledRollbackReachesTheAuditTrail(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	wedgedRollback(ctx, t, e, k, cp.ReasonImagePullBackOff)

	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	var found bool
	for _, en := range d.AuditRows() {
		if en.Operation == "rollback" && en.Args["rollout"] == string(cp.RolloutUnsettled) && en.Args["rollout_reason"] == cp.ReasonImagePullBackOff {
			found = true
		}
	}
	if !found {
		t.Errorf("no rollback audit row records the rollout; rows = %+v", d.AuditRows())
	}
}

// TestARollbacksPostHookIsNeverToldTheApplicationsOutput holds ADR-0074 §9's line on the rollback
// path too. The rollback captures what the pod that would not become ready was printing, because on
// this path it is the only thing that explains why the recovery did not work — and a hook's
// environment persists in a Kubernetes object anyone with namespace read access can see.
func TestARollbacksPostHookIsNeverToldTheApplicationsOutput(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	const secret = "connection refused to postgres://user:hunter2@db"
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")
	k.SetRolloutOutcome("web", cp.RolloutOutcome{
		Reason: cp.ReasonDeadlineExceeded,
		Detail: "waited 5m0s; 1 of 1 replicas updated, 0 ready",
		Output: secret,
	})

	back, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.Rollout == nil || !strings.Contains(back.Rollout.Issue, secret) {
		t.Fatalf("the caller was not shown the output that explains the failure: %+v", back.Rollout)
	}
	runs := k.RunJobs()
	if len(runs) == 0 {
		t.Fatal("the post-deploy hook did not run after the rollback")
	}
	// The hook IS told the verdict, which is what makes the absence of the output below meaningful.
	if got := runs[len(runs)-1].Env["BURROW_DEPLOY_REASON"]; got != cp.ReasonDeadlineExceeded {
		t.Fatalf("hook BURROW_DEPLOY_REASON = %q, want %q", got, cp.ReasonDeadlineExceeded)
	}
	for _, run := range runs {
		for key, value := range run.Env {
			if strings.Contains(value, secret) {
				t.Errorf("hook env %s carries the application's output; it persists in a Kubernetes object", key)
			}
		}
	}
}
