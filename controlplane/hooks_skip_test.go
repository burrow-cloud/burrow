// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// These tests are ADR-0080: a rollback is the incident escape hatch, so a `pre-rollback` hook that
// cannot run must not be able to strand it — and the way past must not be the one that existed
// before, which worked by deleting the hook.
//
// The default is unchanged and is tested next door (TestFailedPreRollbackAbortsTheRollback): without
// the flag, a failed hook still aborts.

// TestSkipHooksRollsBackPastAHookThatCannotRun is the defect the record closes. The hook fails, the
// rollback still lands, and nothing about the app's configuration had to be destroyed to get there.
//
// It asserts the hook did not RUN, not merely that its failure was tolerated. The case the flag
// exists for is a hook that cannot start — the image will not pull, the Job will not schedule — and
// running it again only spends the timeout again.
func TestSkipHooksRollsBackPastAHookThatCannotRun(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:A")
	mustDeploy(ctx, t, e, "web", "img:B")
	setHook(ctx, t, e, "web", "", cp.HookPreRollback, "./migrate", "down")
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: "ImagePullBackOff\n"})

	res, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true, SkipHooks: true})
	if err != nil {
		t.Fatalf("Rollback with --skip-hooks: %v — a broken hook must not be able to strand a rollback", err)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Errorf("RunJobs = %d, want 0: --skip-hooks does not run the hook and ignore it, it does not run it", len(runs))
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:A" {
		t.Errorf("applied spec = %+v, want img:A: the rollback must have landed", spec)
	}
	if res.RolledBackToReleaseID == "" {
		t.Error("the result names no release rolled back to")
	}
}

// TestSkippingAHookLeavesItConfigured is the non-destructive half, and it is the whole point of the
// flag over `hook unset`. The escape that existed before deleted the hook, so the NEXT rollback ran
// with no schema protection at all — the failure the arrangement was built to prevent, arrived at by
// way of its own escape.
func TestSkippingAHookLeavesItConfigured(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:A")
	mustDeploy(ctx, t, e, "web", "img:B")
	setHook(ctx, t, e, "web", "", cp.HookPreRollback, "./migrate", "down")
	k.SetRunResult(cp.RunResult{ExitCode: 1})

	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true, SkipHooks: true}); err != nil {
		t.Fatalf("Rollback with --skip-hooks: %v", err)
	}

	hooks, err := e.Hooks(ctx, "web", "")
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	var found bool
	for _, h := range hooks {
		if h.Phase == cp.HookPreRollback {
			found = true
			if got := strings.Join(h.Command, " "); got != "./migrate down" {
				t.Errorf("pre-rollback command = %q, want the command that was configured", got)
			}
		}
	}
	if !found {
		t.Fatal("the pre-rollback hook is gone after a skip; a skip that deletes the hook is the lossy escape this replaces")
	}

	// And the protection is really back: the next rollback, run without the flag, aborts again.
	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true}); err == nil {
		t.Fatal("the next rollback ran past the failing hook without the flag; the skip must apply to ONE invocation")
	}
}

// TestASkippedHookIsVisibleAfterwards asserts the two places "we rolled back around a broken hook"
// has to be findable: on the result at the moment it happens, and in the audit record afterwards
// (ADR-0080 §4). It is the first thing worth knowing when somebody later asks why the schema looks
// the way it does, and exactly the thing nobody writes down at three in the morning.
func TestASkippedHookIsVisibleAfterwards(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:A")
	mustDeploy(ctx, t, e, "web", "img:B")
	setHook(ctx, t, e, "web", "", cp.HookPreRollback, "./migrate", "down")
	k.SetRunResult(cp.RunResult{ExitCode: 1})

	res, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true, SkipHooks: true})
	if err != nil {
		t.Fatalf("Rollback with --skip-hooks: %v", err)
	}

	hint := strings.Join(res.Hints, "\n")
	for _, want := range []string{"pre-rollback", "SKIPPED", "./migrate down", "still configured"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the result does not say %q; a skip an operator has to infer from silence is not reported: %q", want, hint)
		}
	}

	var rows int
	for _, row := range d.AuditRows() {
		if row.Operation != "rollback" {
			continue
		}
		rows++
		if row.Args["hooks_skipped"] != string(cp.HookPreRollback) {
			t.Errorf("rollback audit row does not name the skipped phase: %+v", row.Args)
		}
		if row.Args["skipped_command"] != "./migrate down" {
			t.Errorf("rollback audit row does not name the command that did not run: %+v", row.Args)
		}
	}
	if rows == 0 {
		t.Fatal("no rollback audit row at all")
	}
}

// TestSkipHooksWithNoHookRecordsNothing: skipping nothing is not a skip. An operator who passes the
// flag on an app with no `pre-rollback` hook has changed nothing, and a hint or an audit row saying
// otherwise would be a record of something that did not happen.
func TestSkipHooksWithNoHookRecordsNothing(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:A")
	mustDeploy(ctx, t, e, "web", "img:B")

	res, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true, SkipHooks: true})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if hint := strings.Join(res.Hints, "\n"); strings.Contains(hint, "SKIPPED") {
		t.Errorf("a rollback with no hook reported a skip: %q", hint)
	}
	for _, row := range d.AuditRows() {
		if _, ok := row.Args["hooks_skipped"]; ok {
			t.Errorf("a rollback with no hook recorded a skip: %+v", row.Args)
		}
	}
}

// TestABlockedRollbackNamesTheOneActionThatClearsIt is the legibility half of ADR-0080 §3. The
// message an operator meets mid-incident has to name the hook, what it did, and the ONE command that
// gets them moving — and say that the agent cannot run it, so an agent relays the answer to a human
// instead of retrying into the same wall.
func TestABlockedRollbackNamesTheOneActionThatClearsIt(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	mustDeploy(ctx, t, e, "web", "img:A")
	mustDeploy(ctx, t, e, "web", "img:B")
	setHook(ctx, t, e, "web", "", cp.HookPreRollback, "./migrate", "down")
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: "cannot drop column in use\n"})

	_, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true})
	if err == nil {
		t.Fatal("Rollback succeeded; a failed pre-rollback hook still aborts by default")
	}
	msg := err.Error()
	for _, want := range []string{
		"pre-rollback hook failed for web",     // which hook
		`"./migrate down"`,                     // what it was
		"exit code 1",                          // what it did
		"burrow app rollback web --skip-hooks", // the one action, ready to run
		"leaves it configured",                 // what it does not cost
		"NOT when the revert itself failed",    // when not to
		"burrow-agent cannot supply it",        // who runs it
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not contain %q; somebody mid-incident is left reading source:\n%s", want, msg)
		}
	}
	// The recovery instruction is the LAST line: the captured output sits between the summary and it,
	// and an instruction that scrolled past is one nobody reads.
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	if !strings.Contains(lines[len(lines)-1], "--skip-hooks") {
		t.Errorf("the action is not the last line of the message:\n%s", msg)
	}
}

// TestTheRecoveryCommandCarriesTheEnvironment: a rollback in a named environment needs `--env` in the
// command it hands over, or the operator runs it against the wrong one. `prod` is elided, because the
// default environment is what a bare invocation already targets (ADR-0067 §2).
func TestTheRecoveryCommandCarriesTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want string
	}{
		{env: "staging", want: "burrow app rollback web --env staging --skip-hooks"},
		{env: cp.DefaultEnvironment, want: "burrow app rollback web --skip-hooks"},
	} {
		h := &cp.HookError{App: "web", Env: tc.env, Phase: cp.HookPreRollback, Command: []string{"./migrate", "down"}, Image: "img:B", ExitCode: 1}
		if !strings.Contains(h.Error(), tc.want) {
			t.Errorf("env %q: the message does not offer %q:\n%s", tc.env, tc.want, h.Error())
		}
	}
}

// TestAFailedPreDeployHookStillBlocksADeploy is the boundary of the whole change. A deploy can wait:
// if a `pre-deploy` hook is broken the right response is to fix it, and nothing is on fire while that
// happens. So the abort stays absolute there — DeployRequest carries no skip at all, which is a
// compile-time fact — and, just as importantly, the deploy's refusal must not advertise a flag that
// would let somebody routinely skip migrations.
func TestAFailedPreDeployHookStillBlocksADeploy(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	setHook(ctx, t, e, "web", "", cp.HookPreDeploy, "./migrate", "up")
	k.SetRunResult(cp.RunResult{ExitCode: 1})

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err == nil {
		t.Fatal("Deploy succeeded past a failed pre-deploy hook; the rollback override must not have widened the rule")
	}
	mustHookError(t, err, cp.HookPreDeploy)
	if _, ok := k.Spec("web"); ok {
		t.Error("a workload was applied despite the failed pre-deploy hook")
	}
	if strings.Contains(err.Error(), "--skip-hooks") {
		t.Errorf("the deploy's refusal offers --skip-hooks; the flag exists only where the urgency does:\n%s", err.Error())
	}
}
