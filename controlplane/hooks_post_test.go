// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The post phase (ADR-0072 §4-§6). Everything here is about ONE claim: an unattended deploy stops
// being silent, and what it stops being silent WITH is a machine-readable outcome rather than a
// notification saying something went wrong.

// postHookEnv returns the environment the last hook Job was launched with, failing the test if no
// hook ran at all.
func postHookEnv(t *testing.T, k *fake.Kubernetes) map[string]string {
	t.Helper()
	runs := k.RunJobs()
	if len(runs) == 0 {
		t.Fatal("no hook Job ran")
	}
	return runs[len(runs)-1].Env
}

// TestPostDeployHookIsToldASuccessfulDeploySucceeded is the ordinary path: the rollout settles, and
// the hook is told so — with the app, the environment and the image, so a notification can say what
// it is talking about (ADR-0072 §4).
func TestPostDeployHookIsToldASuccessfulDeploySucceeded(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("RunJobs = %d, want 1", len(runs))
	}
	if runs[0].Image != "img:1" {
		t.Errorf("hook image = %q, want the image that was deployed", runs[0].Image)
	}
	env := runs[0].Env
	for key, want := range map[string]string{
		"BURROW_HOOK_PHASE":     "post-deploy",
		"BURROW_APP":            "web",
		"BURROW_ENVIRONMENT":    cp.DefaultEnvironment,
		"BURROW_IMAGE":          "img:1",
		"BURROW_DEPLOY_KIND":    "deploy",
		"BURROW_DEPLOY_OUTCOME": "succeeded",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
	if env["BURROW_RELEASE"] != res.Release.ID {
		t.Errorf("BURROW_RELEASE = %q, want the release the hook is reporting on (%q)", env["BURROW_RELEASE"], res.Release.ID)
	}
	// Present and EMPTY on success: a hook running under `set -u` must not abort on an unset
	// variable before it can report anything.
	reason, ok := env["BURROW_DEPLOY_REASON"]
	if !ok {
		t.Error("BURROW_DEPLOY_REASON is absent; it must be set and empty on success")
	}
	if reason != "" {
		t.Errorf("BURROW_DEPLOY_REASON = %q on a successful deploy, want empty", reason)
	}
	// A successful, settled deploy has nothing to say about the rollout.
	for _, h := range res.Hints {
		if strings.Contains(h, "did not settle") {
			t.Errorf("hint = %q on a settled rollout", h)
		}
	}
}

// TestPostDeployHookIsToldTheClosedReasonWhenTheRolloutFailed is the case the phase exists for, and
// the dependency ADR-0072 §4 names: before ADR-0074 widened the vocabulary a hook could have been
// told THAT a deploy failed and never WHY, and a hook that knows only "something went wrong" is a
// notification, not an integration.
func TestPostDeployHookIsToldTheClosedReasonWhenTheRolloutFailed(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")
	// The new release crash-loops. The reason is read off the same WorkloadStatus `burrow app status`
	// reads, so the hook branches on exactly the string a user would see for this pod.
	k.SetIssue("web", cp.IssueEvidence{
		Reason:    cp.ReasonCrashLoopBackOff,
		Container: "app",
		ExitCode:  1,
		LogTail:   "connected as postgres://user:hunter2@db/app\npanic: boom",
	})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	env := postHookEnv(t, k)
	if env["BURROW_DEPLOY_OUTCOME"] != "failed" {
		t.Errorf("BURROW_DEPLOY_OUTCOME = %q, want failed", env["BURROW_DEPLOY_OUTCOME"])
	}
	if env["BURROW_DEPLOY_REASON"] != cp.ReasonCrashLoopBackOff {
		t.Errorf("BURROW_DEPLOY_REASON = %q, want %q from ADR-0074 §2's closed set",
			env["BURROW_DEPLOY_REASON"], cp.ReasonCrashLoopBackOff)
	}
	if !cp.IsIssueReason(env["BURROW_DEPLOY_REASON"]) {
		t.Errorf("BURROW_DEPLOY_REASON = %q is not a member of the closed set", env["BURROW_DEPLOY_REASON"])
	}
	// The deploy itself still succeeded: the image is live and the release is recorded. Burrow
	// reports; it does not roll back by itself (§6).
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want it deployed: a failed rollout is not a failed deploy call", res.Release.Status)
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:2" {
		t.Errorf("applied spec = %+v, want img:2 still running: nothing rolls back automatically", spec)
	}
	var sawHint bool
	for _, h := range res.Hints {
		if strings.Contains(h, cp.ReasonCrashLoopBackOff) && strings.Contains(h, "does not roll back by itself") {
			sawHint = true
		}
	}
	if !sawHint {
		t.Errorf("hints = %v, want one naming the reason and saying Burrow does not roll back", res.Hints)
	}
}

// TestPostDeployHookIsNeverToldTheApplicationsOwnOutput is the redaction boundary carried into the
// new surface (ADR-0074 §9). A crash-loop Issue carries a bounded tail of the APPLICATION's previous
// log, which may contain anything it printed. Read live and discarded that is an accepted trade; it
// is not one for a value copied into a Job's environment, where it would sit in a Kubernetes object.
// So the hook gets the reason, and the app's own output stays in `burrow app logs`.
func TestPostDeployHookIsNeverToldTheApplicationsOwnOutput(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")
	k.SetIssue("web", cp.IssueEvidence{
		Reason:    cp.ReasonCrashLoopBackOff,
		Container: "app",
		ExitCode:  1,
		LogTail:   "DATABASE_URL=postgres://user:hunter2@db/app",
	})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for key, v := range postHookEnv(t, k) {
		if strings.Contains(v, "hunter2") {
			t.Fatalf("hook env %s carries the application's own output: %q", key, v)
		}
	}
	for _, h := range res.Hints {
		if strings.Contains(h, "hunter2") {
			t.Fatalf("a hint carries the application's own output: %q", h)
		}
	}
	for _, row := range d.AuditRows() {
		for _, v := range row.Args {
			if strings.Contains(v, "hunter2") {
				t.Fatalf("audit args carry the application's own output: %+v", row.Args)
			}
		}
		if strings.Contains(row.Result, "hunter2") {
			t.Fatalf("audit result carries the application's own output: %q", row.Result)
		}
	}
}

// TestPostDeployDeadlineReportsWhatWasObserved is ADR-0072 §5 and issue #352's shape, which must not
// be reproduced: a waiter that burns its full deadline and reports elapsed time, when the cluster was
// saying something the whole time, has converted a diagnosis into a shrug. An expired bound is
// therefore the closed set's declared BACKSTOP reason, with a detail describing what was seen.
func TestPostDeployDeadlineReportsWhatWasObserved(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")
	k.SetRolloutOutcome("web", cp.RolloutOutcome{
		Reason: cp.ReasonDeadlineExceeded,
		Detail: "waited 5m0s; 0 of 1 replicas updated, 0 ready",
	})

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	env := postHookEnv(t, k)
	if env["BURROW_DEPLOY_OUTCOME"] != "failed" {
		t.Errorf("BURROW_DEPLOY_OUTCOME = %q, want failed", env["BURROW_DEPLOY_OUTCOME"])
	}
	if env["BURROW_DEPLOY_REASON"] != cp.ReasonDeadlineExceeded {
		t.Errorf("BURROW_DEPLOY_REASON = %q, want %q", env["BURROW_DEPLOY_REASON"], cp.ReasonDeadlineExceeded)
	}
	if !strings.Contains(env["BURROW_DEPLOY_DETAIL"], "replicas") {
		t.Errorf("BURROW_DEPLOY_DETAIL = %q, want what the wait observed and not a bare elapsed time",
			env["BURROW_DEPLOY_DETAIL"])
	}
}

// TestPostDeployWaitsWithTheConfiguredBound asserts the settle bound is the operational limit
// ADR-0072 §5 puts it on rather than a constant, resolved for the environment being deployed to.
func TestPostDeployWaitsWithTheConfiguredBound(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	waits := k.Rollouts()
	if len(waits) != 1 {
		t.Fatalf("AwaitRollout calls = %d, want 1", len(waits))
	}
	if waits[0].Timeout != 5*time.Minute {
		t.Errorf("settle bound = %s, want the built-in default of 5m", waits[0].Timeout)
	}

	d.SetLimits(cp.OperationalConfig{}.With(cp.LimitDeploySettleTimeout, "45s"))
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	waits = k.Rollouts()
	if got := waits[len(waits)-1].Timeout; got != 45*time.Second {
		t.Errorf("settle bound = %s, want the operator-set 45s", got)
	}
}

// TestNoPostDeployHookWaitsForNothing is the cost boundary: unset means no hook and today's
// behaviour exactly (ADR-0072 §1), so a deploy nobody asked to be told about must not pay for a
// settle wait it would then have nothing to do with.
func TestNoPostDeployHookWaitsForNothing(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if waits := k.Rollouts(); len(waits) != 0 {
		t.Fatalf("AwaitRollout calls = %d, want 0 with no post-deploy hook set", len(waits))
	}
	if runs := k.RunJobs(); len(runs) != 1 || runs[0].Command[0] != "./migrate" {
		t.Fatalf("RunJobs = %+v, want only the pre-deploy hook", runs)
	}
}

// TestFailedPostDeployHookDoesNotUndoTheDeploy is the asymmetry with the pre phases (ADR-0072 §6).
// A failed pre hook aborts the operation because nothing has happened yet; a failed post hook aborts
// nothing, because the thing it reports on already happened. Reporting it as the deploy's failure
// would tell the caller a deploy that landed did not.
func TestFailedPostDeployHookDoesNotUndoTheDeploy(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")
	k.SetRunResult(cp.RunResult{ExitCode: 3, Stdout: "could not reach the pager: token postgres://u:hunter2@db\n"})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy returned %v, want success: a post hook cannot fail a deploy that already happened", err)
	}
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want it deployed", res.Release.Status)
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:1" {
		t.Errorf("applied spec = %+v, want the deployed image still running", spec)
	}
	var sawHint bool
	for _, h := range res.Hints {
		if strings.Contains(h, "Nothing was rolled back") {
			sawHint = true
		}
		if strings.Contains(h, "hunter2") {
			t.Errorf("a hint carries the command's captured output: %q", h)
		}
	}
	if !sawHint {
		t.Errorf("hints = %v, want one saying the hook failed and nothing was undone", res.Hints)
	}
	// The failure is still recorded where a failure belongs: an audit row naming the phase, the
	// command and the exit code, and carrying none of the command's output (ADR-0027).
	var sawRow bool
	for _, row := range d.AuditRows() {
		if row.Operation != "hook" || row.Args["phase"] != string(cp.HookPostDeploy) {
			continue
		}
		sawRow = true
		if row.Outcome != cp.AuditFailed {
			t.Errorf("hook row outcome = %q, want failed", row.Outcome)
		}
		if row.Args["deploy_outcome"] != "succeeded" {
			t.Errorf("hook row deploy_outcome = %q, want the outcome the hook was told", row.Args["deploy_outcome"])
		}
		for _, v := range row.Args {
			if strings.Contains(v, "hunter2") {
				t.Fatalf("audit args carry the command's output: %+v", row.Args)
			}
		}
		if strings.Contains(row.Result, "hunter2") {
			t.Fatalf("audit result carries the command's output: %q", row.Result)
		}
	}
	if !sawRow {
		t.Error("no post-deploy hook audit row: a command Burrow ran on the user's behalf must leave a record")
	}
}

// TestRollbackFiresPostDeployToldItWasARollback is ADR-0072 §4's second half: a rollback fires
// `post-deploy`, not a fourth phase, and it is told which direction the image moved. The image it
// reports on is the one now SERVING — the opposite of `pre-rollback`, which runs from the image
// being left behind (§8).
func TestRollbackFiresPostDeployToldItWasARollback(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:1")
	mustDeploy(ctx, t, e, "web", "img:2")
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")

	res, err := e.Rollback(ctx, "web", "", true)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("RunJobs = %d, want 1: a rollback fires post-deploy too", len(runs))
	}
	if runs[0].Image != "img:1" {
		t.Errorf("hook image = %q, want img:1, the image now serving", runs[0].Image)
	}
	env := runs[0].Env
	if env["BURROW_DEPLOY_KIND"] != "rollback" {
		t.Errorf("BURROW_DEPLOY_KIND = %q, want rollback", env["BURROW_DEPLOY_KIND"])
	}
	if env["BURROW_HOOK_PHASE"] != string(cp.HookPostDeploy) {
		t.Errorf("BURROW_HOOK_PHASE = %q, want post-deploy: there is no post-rollback phase", env["BURROW_HOOK_PHASE"])
	}
	if env["BURROW_DEPLOY_OUTCOME"] != "succeeded" {
		t.Errorf("BURROW_DEPLOY_OUTCOME = %q, want succeeded", env["BURROW_DEPLOY_OUTCOME"])
	}
	if env["BURROW_RELEASE"] != res.Release.ID {
		t.Errorf("BURROW_RELEASE = %q, want the rollback's own release %q", env["BURROW_RELEASE"], res.Release.ID)
	}
}

// TestPostDeployHookGetsTheAppsConfigToo asserts the hook is a command in the app's own image with
// the app's own environment (ADR-0048 §2), which is what makes a smoke test the natural post-deploy
// hook (ADR-0072 §7) — and that Burrow's own variables sit BESIDE the app's config rather than
// replacing it.
func TestPostDeployHookGetsTheAppsConfigToo(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./smoke")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	env := postHookEnv(t, k)
	if env["LOG_LEVEL"] != "debug" {
		t.Errorf("LOG_LEVEL = %q, want the app's own config value", env["LOG_LEVEL"])
	}
	if env["BURROW_DEPLOY_OUTCOME"] == "" {
		t.Error("the hook lost its outcome variables when the app had config of its own")
	}
}

// TestPreHooksAreToldWhichPhaseTheyAre asserts the identity variables reach the pre phases as well:
// one script that serves both `pre-deploy` and `pre-rollback` should not have to guess which moment
// it is running at. The OUTCOME variables belong to the post phase alone — there is no outcome yet
// when a pre hook runs, and a variable that was sometimes meaningful would be worse than none.
func TestPreHooksAreToldWhichPhaseTheyAre(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	env := postHookEnv(t, k)
	if env["BURROW_HOOK_PHASE"] != string(cp.HookPreDeploy) {
		t.Errorf("BURROW_HOOK_PHASE = %q, want pre-deploy", env["BURROW_HOOK_PHASE"])
	}
	if env["BURROW_IMAGE"] != "img:1" {
		t.Errorf("BURROW_IMAGE = %q, want the image being deployed", env["BURROW_IMAGE"])
	}
	if _, ok := env["BURROW_DEPLOY_OUTCOME"]; ok {
		t.Error("a pre-deploy hook was given a deploy outcome; there is not one yet when it runs")
	}
}

// TestPostDeployHookIsToldWhenTheWorkloadIsGone asserts the wait reaches for ADR-0074 §6's existing
// reason for an absence rather than coining a new one — the exact thing ADR-0072 §4 asks it not to
// do. A registry that says a release rolled out, against a cluster with no workload for it, is
// ReasonWorkloadMissing and has been since the failure ledger shipped.
func TestPostDeployHookIsToldWhenTheWorkloadIsGone(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPostDeploy, "./notify")
	k.SetRolloutOutcome("web", cp.RolloutOutcome{
		Reason: cp.ReasonWorkloadMissing,
		Detail: "the app's Deployment is not in the cluster, so there is no rollout to wait for",
	})

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	got := postHookEnv(t, k)["BURROW_DEPLOY_REASON"]
	if got != cp.ReasonWorkloadMissing {
		t.Errorf("BURROW_DEPLOY_REASON = %q, want %q", got, cp.ReasonWorkloadMissing)
	}
	if !cp.IsLedgerReason(got) {
		t.Errorf("BURROW_DEPLOY_REASON = %q is outside ADR-0074's vocabulary", got)
	}
}

// TestPostDeployOutcomeVocabularyIsTwoValues pins the outcome strings a hook branches on. Three
// values would mean a hook could meet one it has no branch for, and "unknown" is the tempting third:
// a wait that could not observe the cluster is not a third kind of answer, it is a wait that ran out
// of time without observing a settle.
func TestPostDeployOutcomeVocabularyIsTwoValues(t *testing.T) {
	if got := (cp.RolloutOutcome{Settled: true}).Outcome(); got != "succeeded" {
		t.Errorf("settled outcome = %q, want succeeded", got)
	}
	for _, reason := range []string{cp.ReasonCrashLoopBackOff, cp.ReasonDeadlineExceeded, cp.ReasonWorkloadMissing, ""} {
		out := cp.RolloutOutcome{Reason: reason}
		if got := out.Outcome(); got != "failed" {
			t.Errorf("outcome for reason %q = %q, want failed", reason, got)
		}
		if !out.Failed() {
			t.Errorf("Failed() = false for reason %q", reason)
		}
	}
}
