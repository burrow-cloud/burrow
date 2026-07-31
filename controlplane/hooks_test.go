// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// setHook configures a hook and fails the test if the write does not take, so the tests below read
// as "given this hook, when a deploy happens".
func setHook(ctx context.Context, t *testing.T, e *cp.Engine, app, env string, phase cp.HookPhase, command ...string) {
	t.Helper()
	if _, err := e.SetHook(ctx, app, env, phase, command); err != nil {
		t.Fatalf("SetHook(%s, %s): %v", app, phase, err)
	}
}

// mustHookError asserts err is a HookError for the given phase and returns it.
func mustHookError(t *testing.T, err error, phase cp.HookPhase) *cp.HookError {
	t.Helper()
	h, ok := cp.AsHook(err)
	if !ok {
		t.Fatalf("err = %v, want a HookError", err)
	}
	if h.Phase != phase {
		t.Fatalf("hook phase = %q, want %q", h.Phase, phase)
	}
	return h
}

// TestNoHookLeavesDeployUnchanged asserts unset means no hook and today's behaviour exactly
// (ADR-0072 §1): a deploy with nothing configured launches no Job at all.
func TestNoHookLeavesDeployUnchanged(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Fatalf("RunJobs = %d, want 0: an app with no hook must launch nothing", len(runs))
	}
	if _, ok := k.Spec("web"); !ok {
		t.Error("no workload applied: a deploy with no hook must roll out exactly as before")
	}
}

// TestPreDeployHookRunsFromTheNewImage asserts the pre-deploy hook runs the IMAGE BEING DEPLOYED,
// not the running one, with the app's config injected (ADR-0072 §2).
func TestPreDeployHookRunsFromTheNewImage(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./manage.py", "migrate")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("RunJobs = %d, want 1", len(runs))
	}
	if runs[0].Image != "img:2" {
		t.Errorf("hook image = %q, want img:2 (the image being deployed, not the running one)", runs[0].Image)
	}
	if got := strings.Join(runs[0].Command, " "); got != "./manage.py migrate" {
		t.Errorf("hook command = %q, want the configured argv", got)
	}
	if runs[0].TTLSeconds != 3600 {
		t.Errorf("hook Job ttl = %d, want 3600 so a failed hook's Job is left for diagnosis", runs[0].TTLSeconds)
	}
	// The workload the hook preceded still rolled out.
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:2" {
		t.Errorf("applied spec = %+v, want img:2 rolled out after a successful hook", spec)
	}
}

// TestPreDeployHookRunsOnAnAutomatedDeploy is the case the record exists for: auto-deploy ships an
// image with NOBODY PRESENT, so a hook that fired only on explicit deploys would miss the only path
// that cannot sequence a migration itself (ADR-0072 §2).
func TestPreDeployHookRunsOnAnAutomatedDeploy(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1.0.0", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")

	if _, err := e.DeployAutoForTest(ctx, cp.DeployRequest{App: "web", Image: "img:1.0.1", Replicas: 1}, cp.AutoDeployPatch, "1.0.1"); err != nil {
		t.Fatalf("auto Deploy: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("RunJobs = %d, want 1: an unattended deploy must run the hook too", len(runs))
	}
	if runs[0].Image != "img:1.0.1" {
		t.Errorf("hook image = %q, want the tag the watcher took", runs[0].Image)
	}
}

// TestFailedPreDeployHookAbortsTheDeploy is §3: the new image does not roll out, the running version
// keeps serving, and the failure is reported as the deploy's failure with the command's output.
func TestFailedPreDeployHookAbortsTheDeploy(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: "ERROR: relation \"users\" does not exist\n"})

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})
	h := mustHookError(t, err, cp.HookPreDeploy)
	if h.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", h.ExitCode)
	}
	if !strings.Contains(h.Output, "does not exist") {
		t.Errorf("output = %q, want the command's captured output", h.Output)
	}
	if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "did not happen") {
		t.Errorf("error text = %q, want the output and what did not happen", err.Error())
	}
	// The running version keeps serving on the old schema: nothing was applied to the cluster.
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:1" {
		t.Errorf("applied spec = %+v, want the previous image still running", spec)
	}
	// The attempt is recorded as a FAILED release, so history shows it, and the release it would
	// have superseded is still the deployed one (a failed release is not a rollback target).
	releases, err := d.Releases(ctx, "web", cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("releases = %d, want 2 (the deploy that landed and the one that did not)", len(releases))
	}
	if releases[1].Status != cp.ReleaseFailed {
		t.Errorf("aborted release status = %q, want %q", releases[1].Status, cp.ReleaseFailed)
	}
	if releases[0].Status != cp.ReleaseDeployed {
		t.Errorf("prior release status = %q, want it still deployed", releases[0].Status)
	}
}

// TestPreDeployHookThatNeverFinishesAbortsTheDeploy asserts a hook Burrow stopped waiting on is a
// failure like any other: the deploy does not happen, and the error says the command did not finish
// rather than reporting a bare exit code it never had (ADR-0072 §3).
func TestPreDeployHookThatNeverFinishesAbortsTheDeploy(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")
	k.SetError(fake.OpRunJob, errors.New("kube: run job burrow-run-pre-deploy-r9: timed out"))

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})
	h := mustHookError(t, err, cp.HookPreDeploy)
	if h.Cause == nil {
		t.Error("Cause is nil, want the launch/timeout failure the command never got past")
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:1" {
		t.Errorf("applied spec = %+v, want the previous image still running", spec)
	}
}

// TestFailedHookKeepsItsOutputOutOfTheAuditLog is the redaction boundary (ADR-0027): the audit row
// records the phase, the command and the exit code, and never what the command printed — a hook's
// output is the user's own program's and may carry anything it read out of the app's Secret.
func TestFailedHookKeepsItsOutputOutOfTheAuditLog(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	if err := e.SetConfig(ctx, "web", "", "LOG_LEVEL", "debug", true); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")
	k.SetRunResult(cp.RunResult{ExitCode: 2, Stdout: "connected as postgres://user:hunter2@db/app\n"})

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err == nil {
		t.Fatal("Deploy succeeded, want the hook failure to abort it")
	}
	var sawHookRow bool
	for _, row := range d.AuditRows() {
		for _, v := range row.Args {
			if strings.Contains(v, "hunter2") {
				t.Fatalf("audit args carry the command's output: %+v", row.Args)
			}
		}
		if strings.Contains(row.Result, "hunter2") {
			t.Fatalf("audit result carries the command's output: %q", row.Result)
		}
		if row.Operation != "hook" {
			continue
		}
		sawHookRow = true
		if row.Outcome != cp.AuditFailed {
			t.Errorf("hook row outcome = %q, want failed", row.Outcome)
		}
		if row.Args["command"] != "./migrate" || row.Args["phase"] != string(cp.HookPreDeploy) {
			t.Errorf("hook row args = %+v, want the phase and the command", row.Args)
		}
		if row.Args["env_keys"] != "LOG_LEVEL" {
			t.Errorf("hook row env_keys = %q, want the KEY NAMES only", row.Args["env_keys"])
		}
	}
	if !sawHookRow {
		t.Error("no hook audit row: a command Burrow ran on the user's behalf must leave a record")
	}
}

// TestRollbackWithNoPreRollbackHookRunsNothing asserts §8's safe default: unset means nothing runs,
// because a team practising expand/contract migrates forward only.
func TestRollbackWithNoPreRollbackHookRunsNothing(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:1")
	mustDeploy(ctx, t, e, "web", "img:2")

	if _, err := e.Rollback(ctx, "web", "", true); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Fatalf("RunJobs = %d, want 0: pre-rollback defaults to nothing", len(runs))
	}
}

// TestRollbackRunsPreRollbackFromTheImageBeingLeft is §8's load-bearing detail: rolling back B to A,
// the code that knows how to undo B's migration is in B, so the hook runs from B.
func TestRollbackRunsPreRollbackFromTheImageBeingLeft(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:A")
	mustDeploy(ctx, t, e, "web", "img:B")
	setHook(ctx, t, e, "web", "", cp.HookPreRollback, "./migrate", "down")

	if _, err := e.Rollback(ctx, "web", "", true); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("RunJobs = %d, want 1", len(runs))
	}
	if runs[0].Image != "img:B" {
		t.Errorf("pre-rollback image = %q, want img:B — the image being rolled back FROM", runs[0].Image)
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:A" {
		t.Errorf("applied spec = %+v, want img:A restored after the hook", spec)
	}
}

// TestRollbackNeverFiresPreDeploy is the test the issue asks for by name, because this is the one
// that looks correct in a diagram and corrupts a schema in practice (ADR-0072 §8). A rollback is
// mechanically a deploy of an older image, so "pre-deploy runs on every deploy path" would reach it —
// and running A's migration tool while returning to A steps back one of A's OWN migrations, since A
// does not know B's exists.
func TestRollbackNeverFiresPreDeploy(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:A")
	mustDeploy(ctx, t, e, "web", "img:B")
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate", "up")
	setHook(ctx, t, e, "web", "", cp.HookPreRollback, "./migrate", "down")

	if _, err := e.Rollback(ctx, "web", "", true); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("RunJobs = %d, want exactly 1: a rollback fires pre-rollback and never pre-deploy", len(runs))
	}
	if got := strings.Join(runs[0].Command, " "); got != "./migrate down" {
		t.Errorf("command = %q, want the pre-rollback command; the pre-deploy hook must not fire", got)
	}
	if runs[0].Image == "img:A" {
		t.Error("the hook ran from the image being rolled back TO, which is exactly the failure §8 warns about")
	}
}

// TestFailedPreRollbackAbortsTheRollback asserts the pre phases share one rule: the hook runs before
// traffic moves back so the schema is stepped back before the older code serves, which means a
// failure must leave the older code out of service rather than serving against a half-stepped-back
// schema (ADR-0072 §3's rule applied to §8's ordering).
func TestFailedPreRollbackAbortsTheRollback(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:A")
	mustDeploy(ctx, t, e, "web", "img:B")
	setHook(ctx, t, e, "web", "", cp.HookPreRollback, "./migrate", "down")
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: "cannot drop column in use\n"})

	_, err := e.Rollback(ctx, "web", "", true)
	h := mustHookError(t, err, cp.HookPreRollback)
	if h.Image != "img:B" {
		t.Errorf("hook image = %q, want img:B", h.Image)
	}
	if !strings.Contains(err.Error(), "rollback did not happen") {
		t.Errorf("error text = %q, want it to say the rollback did not happen", err.Error())
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:B" {
		t.Errorf("applied spec = %+v, want img:B still serving", spec)
	}
}

// TestHooksAreSerializedPerAppAndEnvironment asserts §9: two pushes in quick succession must not run
// two migration Jobs concurrently against one database. The fake blocks inside RunJob until the test
// releases it, so a second deploy that overlapped would be observable.
func TestHooksAreSerializedPerAppAndEnvironment(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")

	var mu sync.Mutex
	var concurrent, peak int
	k.SetRunJobHook(func() {
		mu.Lock()
		concurrent++
		if concurrent > peak {
			peak = concurrent
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		concurrent--
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:x", Replicas: 1})
		}(i)
	}
	wg.Wait()
	if peak > 1 {
		t.Errorf("peak concurrent hook Jobs = %d, want 1: hooks are serialized per app and environment", peak)
	}
	if runs := k.RunJobs(); len(runs) != 4 {
		t.Errorf("RunJobs = %d, want 4 (serialized, not dropped)", len(runs))
	}
}

// TestHooksOfDifferentAppsDoNotBlockEachOther asserts the lock is per app and environment, not
// global: one app's slow migration must not stall another app's deploy.
func TestHooksOfDifferentAppsDoNotBlockEachOther(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")
	setHook(ctx, t, e, "api", "", cp.HookPreDeploy, "./migrate")

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	k.SetRunJobHook(func() {
		started <- struct{}{}
		<-release
	})

	var wg sync.WaitGroup
	for _, app := range []string{"web", "api"} {
		wg.Add(1)
		go func(app string) {
			defer wg.Done()
			_, _ = e.Deploy(ctx, cp.DeployRequest{App: app, Image: "img:1", Replicas: 1})
		}(app)
	}
	// Both hooks must be in flight at once; if the lock were global the second would never start.
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("only one hook started: the lock must be per app and environment, not global")
		}
	}
	close(release)
	wg.Wait()
}

// TestSetListUnsetHook exercises the configuration surface: one command with the phase named, a
// listing in the order the phases fire, and an unset that returns the app to today's behaviour.
func TestSetListUnsetHook(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	setHook(ctx, t, e, "web", "", cp.HookPreRollback, "./migrate", "down")
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate", "up")

	hooks, err := e.Hooks(ctx, "web", "")
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 2 {
		t.Fatalf("hooks = %d, want 2", len(hooks))
	}
	if hooks[0].Phase != cp.HookPreDeploy || hooks[1].Phase != cp.HookPreRollback {
		t.Errorf("listing order = %q, %q; want the order the phases fire in", hooks[0].Phase, hooks[1].Phase)
	}
	if got := strings.Join(hooks[0].Command, " "); got != "./migrate up" {
		t.Errorf("pre-deploy command = %q", got)
	}

	// Setting again replaces rather than accumulating.
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./newmigrate")
	hooks, err = e.Hooks(ctx, "web", "")
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 2 || strings.Join(hooks[0].Command, " ") != "./newmigrate" {
		t.Errorf("hooks after re-set = %+v, want the pre-deploy command replaced", hooks)
	}

	if err := e.UnsetHook(ctx, "web", "", cp.HookPreDeploy); err != nil {
		t.Fatalf("UnsetHook: %v", err)
	}
	// Unsetting what is already unset is a no-op, not an error.
	if err := e.UnsetHook(ctx, "web", "", cp.HookPreDeploy); err != nil {
		t.Fatalf("second UnsetHook: %v", err)
	}
	hooks, err = e.Hooks(ctx, "web", "")
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 1 || hooks[0].Phase != cp.HookPreRollback {
		t.Errorf("hooks after unset = %+v, want only the pre-rollback hook", hooks)
	}
}

// TestSetHookRejectsUnrunnableConfiguration asserts the phase vocabulary is closed and a command
// that could not run is refused where a human is present to be told.
func TestSetHookRejectsUnrunnableConfiguration(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	cases := []struct {
		name    string
		phase   cp.HookPhase
		command []string
		want    string
	}{
		{"unknown phase", cp.HookPhase("during-deploy"), []string{"./x"}, "unknown phase"},
		{"post-rollback is not a phase", cp.HookPhase("post-rollback"), []string{"./x"}, "unknown phase"},
		{"empty command", cp.HookPreDeploy, nil, "command is empty"},
		{"blank program", cp.HookPreDeploy, []string{"  "}, "names no program"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.SetHook(ctx, "web", "", c.phase, c.command)
			if err == nil {
				t.Fatalf("SetHook(%q, %v) succeeded, want a refusal", c.phase, c.command)
			}
			if !errors.Is(err, cp.ErrInvalid) {
				t.Errorf("err = %v, want it to wrap ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), c.want)
			}
		})
	}
}

// TestHookPhasesAreClosedAndNamedForWhenTheyRun pins the vocabulary: every phase a hook may be set
// on says WHEN it runs, the set is the three ADR-0072 names and no more, and it is listed in the
// order the phases fire around an app's life rather than alphabetically.
//
// `post-rollback` is asserted ABSENT deliberately (§4): a rollback fires `post-deploy`, told it was
// a rollback, because "did this settle and is it serving?" is the same question whichever direction
// the image moved — a fourth name would be a second spelling of one answer.
func TestHookPhasesAreClosedAndNamedForWhenTheyRun(t *testing.T) {
	phases := cp.HookPhases()
	want := []cp.HookPhase{cp.HookPreDeploy, cp.HookPostDeploy, cp.HookPreRollback}
	if len(phases) != len(want) {
		t.Fatalf("HookPhases() = %v, want %v", phases, want)
	}
	for i, p := range want {
		if phases[i] != p {
			t.Fatalf("HookPhases() = %v, want %v (in firing order)", phases, want)
		}
	}
	for _, p := range phases {
		if !strings.HasPrefix(string(p), "pre-") && !strings.HasPrefix(string(p), "post-") {
			t.Errorf("phase %q does not say when it runs", p)
		}
		if !cp.KnownHookPhase(p) {
			t.Errorf("KnownHookPhase(%q) = false", p)
		}
	}
	if cp.KnownHookPhase("post-rollback") {
		t.Error("KnownHookPhase(post-rollback) = true: a rollback fires post-deploy, told it was one (ADR-0072 §4)")
	}
}

// TestHooksAreScopedPerEnvironment asserts a hook set in one environment does not run in another —
// the reason the store is keyed by (app, environment, phase).
func TestHooksAreScopedPerEnvironment(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	setHook(ctx, t, e, "web", "staging", cp.HookPreDeploy, "./migrate")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: cp.DefaultEnvironment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy into prod: %v", err)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Fatalf("RunJobs = %d, want 0: a staging hook must not run on a prod deploy", len(runs))
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Env: "staging", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy into staging: %v", err)
	}
	if runs := k.RunJobs(); len(runs) != 1 {
		t.Fatalf("RunJobs = %d, want 1 after the staging deploy", len(runs))
	}
}

// TestDeleteAppRemovesItsHooks asserts an app's hooks do not outlive it: a new app of the same name
// must start unset, not inherit a stranger's pre-deploy command.
func TestDeleteAppRemovesItsHooks(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive().With(cp.GuardrailAppDelete, cp.DispositionAllow))
	mustDeploy(ctx, t, e, "web", "img:1")
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate")

	if err := e.DeleteApp(ctx, "web", "", true); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	hooks, err := e.Hooks(ctx, "web", "")
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(hooks) != 0 {
		t.Fatalf("hooks after delete = %+v, want none", hooks)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Errorf("RunJobs = %d, want 0: a recreated app must not inherit the deleted one's hook", len(runs))
	}
}
