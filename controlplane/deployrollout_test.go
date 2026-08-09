// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// A deploy reports the ROLLOUT, not the submission (ADR-0092, issue #546). Before this, `deploy`
// answered as soon as the API server accepted the Deployment: a rollout whose new pod never passed
// its readiness probe came back as a deployed release with the previous one superseded, exit 0,
// while the previous release served every request. The tests here pin the three outcomes a deploy
// can report and what each leaves in the record.

// TestDeployReportsARolloutThatSettled is the ordinary case: the new replicas came up, so the report
// that has always been printed is now a report that was checked.
func TestDeployReportsARolloutThatSettled(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Rollout == nil || !res.Rollout.Settled {
		t.Fatalf("Rollout = %+v, want a settled observation", res.Rollout)
	}
	if res.Rollout.Failed() {
		t.Error("Failed() = true on a settled rollout")
	}
	rel, _ := d.Release(ctx, res.Release.ID)
	if rel.Status != cp.ReleaseDeployed || rel.Rollout != cp.RolloutSettled || rel.RolloutReason != "" {
		t.Errorf("recorded release = (%q, %q, %q), want (deployed, settled, no reason)", rel.Status, rel.Rollout, rel.RolloutReason)
	}
}

// TestDeployReportsARolloutThatDidNotSettle is the defect. The deploy must come back saying the
// rollout did not become ready, carrying the pod's reason and the status surface's explanation of
// it, and the record must say the same thing — while the release still reads `deployed`, because
// that word is what rollback walks back from.
func TestDeployReportsARolloutThatDidNotSettle(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())

	v1, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	// The workload now exists, so a blocking condition can be injected onto it: the next release's
	// pod crash-loops and never serves, which is the shape of the live failure.
	k.SetIssue("web", cp.IssueEvidence{
		Reason:    cp.ReasonCrashLoopBackOff,
		Container: "web",
		ExitCode:  1,
		LogTail:   "listing tenants is forbidden",
	})

	v2, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	if v2.Rollout == nil || v2.Rollout.Settled {
		t.Fatalf("Rollout = %+v, want an observation that did not settle", v2.Rollout)
	}
	if v2.Rollout.Reason != cp.ReasonCrashLoopBackOff {
		t.Errorf("reason = %q, want %q from the closed set", v2.Rollout.Reason, cp.ReasonCrashLoopBackOff)
	}
	// The diagnosis, not merely the verdict: the deploy looked at the same thing `burrow app status`
	// looks at, so the reason the pod gave is in the deploy's own answer (ADR-0092 §2).
	if !strings.Contains(v2.Rollout.Issue, "listing tenants is forbidden") {
		t.Errorf("Issue = %q, want the pod's own reason, which the status surface already had", v2.Rollout.Issue)
	}

	rel, _ := d.Release(ctx, v2.Release.ID)
	if rel.Rollout != cp.RolloutUnsettled || rel.RolloutReason != cp.ReasonCrashLoopBackOff {
		t.Errorf("recorded rollout = (%q, %q), want (unsettled, %s)", rel.Rollout, rel.RolloutReason, cp.ReasonCrashLoopBackOff)
	}
	// The status stays `deployed` and the previous release stays superseded, deliberately (ADR-0092
	// §4). Rollback takes the newest deployed release and re-applies what THAT superseded; demoting
	// this one would send `burrow app rollback` to the release before the one still serving.
	if rel.Status != cp.ReleaseDeployed {
		t.Errorf("recorded status = %q, want deployed: the status records what Burrow applied, and rollback walks back from it", rel.Status)
	}
	if v2.SupersededReleaseID != v1.Release.ID {
		t.Errorf("SupersededReleaseID = %q, want %q", v2.SupersededReleaseID, v1.Release.ID)
	}
	old, _ := d.Release(ctx, v1.Release.ID)
	if old.Status != cp.ReleaseSuperseded {
		t.Errorf("previous release status = %q, want superseded: the rollback chain is unchanged", old.Status)
	}
}

// TestAWedgedDeployIsStillRollbackable is the consequence of that choice, asserted rather than
// argued: after a deploy whose rollout never came up, the recovery command still returns to the
// release that is actually serving.
func TestAWedgedDeployIsStillRollbackable(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())

	v1, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	k.SetWedgedRollout("web", cp.ReasonImagePullBackOff)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	back, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if back.RolledBackToReleaseID != v1.Release.ID {
		t.Errorf("rolled back to %q, want %q — the release that was still serving", back.RolledBackToReleaseID, v1.Release.ID)
	}
}

// TestAScaledToZeroAppSettles is the case a bounded wait must not turn into a failure. An app with
// no desired replicas has nothing to roll out, so "no replicas became ready" is the correct outcome
// and not a wedged deploy — the seam answers it as settled, and the deploy reports it as such.
func TestAScaledToZeroAppSettles(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	k.SetRolloutOutcome("web", cp.RolloutOutcome{Settled: true})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Rollout == nil || !res.Rollout.Settled {
		t.Fatalf("Rollout = %+v, want settled", res.Rollout)
	}
}

// TestAnUnsettledRolloutReachesTheAuditTrail keeps the record and the trail in step: a reviewer
// reading the audit log afterwards is told whether the image the row is about ever served
// (ADR-0027), not only that a deploy was executed.
func TestAnUnsettledRolloutReachesTheAuditTrail(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	k.SetWedgedRollout("web", cp.ReasonImagePullBackOff)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	entries := d.AuditRows()
	var found bool
	for _, en := range entries {
		if en.Args["rollout"] == string(cp.RolloutUnsettled) && en.Args["rollout_reason"] == cp.ReasonImagePullBackOff {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit row records the rollout; rows = %+v", entries)
	}
}

// TestAPostDeployHookIsNeverToldTheApplicationsOutput holds the line RolloutOutcome.Output draws.
// The deploy captures what a pod that would not become ready was printing, because that is the one
// thing that explains it — and a hook's environment is a Kubernetes object anyone with namespace
// read access can see, so that capture must not travel there (ADR-0074 §9, ADR-0092 §2).
func TestAPostDeployHookIsNeverToldTheApplicationsOutput(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	const secret = "connection refused to postgres://user:hunter2@db"
	k.SetRolloutOutcome("web", cp.RolloutOutcome{
		Reason: cp.ReasonDeadlineExceeded,
		Detail: "waited 5m0s; 1 of 1 replicas updated, 0 ready",
		Output: secret,
	})
	if err := d.SetAppHook(ctx, "web", cp.DefaultEnvironment, cp.HookPostDeploy, []string{"./notify"}); err != nil {
		t.Fatalf("SetAppHook: %v", err)
	}

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Rollout == nil || !strings.Contains(res.Rollout.Issue, secret) {
		t.Fatalf("the caller was not shown the output that explains the failure: %+v", res.Rollout)
	}
	runs := k.RunJobs()
	if len(runs) == 0 {
		t.Fatal("the post-deploy hook did not run")
	}
	// The hook IS told the verdict — that is the whole point of ADR-0072 §4 — so an environment with
	// the reason in it is what makes the absence of the output below meaningful rather than vacuous.
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
