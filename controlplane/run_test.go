// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// runAllowed is a policy that lets a run proceed without confirmation, for tests not about the
// app.run guardrail itself.
func runAllowed() cp.Policy {
	return permissive().With(cp.GuardrailAppRun, cp.DispositionAllow)
}

// mustDeploy deploys a release so the app has a current image to run a command in.
func mustDeploy(ctx context.Context, t *testing.T, e *cp.Engine, app, image string) {
	t.Helper()
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: app, Image: image, Replicas: 1}); err != nil {
		t.Fatalf("deploy %s: %v", app, err)
	}
}

// TestRunCapturesOutputAndExitCode asserts a successful run returns the captured output and exit
// code as a structured result, and drove the Job from the app's own current image with the default
// TTL (ADR-0048 §2, §3, §7).
func TestRunCapturesOutputAndExitCode(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, runAllowed())
	mustDeploy(ctx, t, e, "web", "img:1")
	k.SetRunResult(cp.RunResult{ExitCode: 0, Stdout: "hello\n"})

	res, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"echo", "hello"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.App != "web" || res.ExitCode != 0 || res.Stdout != "hello\n" {
		t.Errorf("result = %+v, want web/0/hello", res)
	}

	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("RunJobs = %d, want 1", len(runs))
	}
	if runs[0].App != "web" || runs[0].Image != "img:1" {
		t.Errorf("run recorded app/image = %s/%s, want web/img:1", runs[0].App, runs[0].Image)
	}
	if got := runs[0].Command; len(got) != 2 || got[0] != "echo" || got[1] != "hello" {
		t.Errorf("run command = %v, want [echo hello]", got)
	}
	if runs[0].TTLSeconds != 3600 {
		t.Errorf("ttl = %d, want default 3600", runs[0].TTLSeconds)
	}
}

// TestRunNonZeroExitIsStructuredNotError asserts a non-zero exit code surfaces as a structured
// RunResult, not a transport error — the agent reasons over it (ADR-0048 §3).
func TestRunNonZeroExitIsStructuredNotError(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, runAllowed())
	mustDeploy(ctx, t, e, "web", "img:1")
	k.SetRunResult(cp.RunResult{ExitCode: 3, Stdout: "boom\n"})

	res, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"false"}})
	if err != nil {
		t.Fatalf("Run returned a transport error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
}

// TestRunHeldForConfirmationThenProceeds asserts the default app.run guardrail (confirm) holds a run
// for confirmation and launches no Job, and that a confirmed run proceeds (ADR-0048 §4, ADR-0020).
func TestRunHeldForConfirmationThenProceeds(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive()) // app.run defaults to confirm
	mustDeploy(ctx, t, e, "web", "img:1")

	_, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"echo", "hi"}})
	mustGuardrail(t, err, cp.GuardrailAppRun)
	g, _ := cp.AsGuardrail(err)
	if !g.NeedsConfirmation {
		t.Errorf("held run should need confirmation, got %+v", g)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Errorf("held run must not launch a Job, got %d", len(runs))
	}

	if _, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"echo", "hi"}, Confirm: true}); err != nil {
		t.Fatalf("confirmed run: %v", err)
	}
	if runs := k.RunJobs(); len(runs) != 1 {
		t.Errorf("confirmed run should launch one Job, got %d", len(runs))
	}
}

// TestRunRefusesAmbiguousEnvironment asserts a run naming no environment while more than one is
// registered is refused before any Job launches (ADR-0047).
func TestRunRefusesAmbiguousEnvironment(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, runAllowed())
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	_, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"echo", "hi"}})
	if _, ok := cp.AsAmbiguousEnvironment(err); !ok {
		t.Fatalf("Run err = %v, want AmbiguousEnvironmentError", err)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Errorf("ambiguous run must not launch a Job, got %d", len(runs))
	}
}

// TestRunWithoutDeployedReleaseNotFound asserts a run against an app with nothing deployed is
// ErrNotFound — there is no image to run a command in (ADR-0048 §2).
func TestRunWithoutDeployedReleaseNotFound(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, runAllowed())
	_, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"echo", "hi"}})
	if !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestRunRejectsBadInput rejects an empty command, a negative TTL, and a bad app name before any Job.
func TestRunRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, runAllowed())
	mustDeploy(ctx, t, e, "web", "img:1")

	if _, err := e.Run(ctx, cp.RunRequest{App: "web", Command: nil}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("empty command err = %v, want ErrInvalid", err)
	}
	neg := int32(-1)
	if _, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"echo"}, TTLSeconds: &neg}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("negative ttl err = %v, want ErrInvalid", err)
	}
	if _, err := e.Run(ctx, cp.RunRequest{App: "Bad_Name", Command: []string{"echo"}}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("bad app name err = %v, want ErrInvalid", err)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Errorf("no Job should launch on a rejected request, got %d", len(runs))
	}
}

// TestRunTTLOverride asserts a per-call TTL (including 0, delete immediately) is passed through to
// the Job (ADR-0048 §7).
func TestRunTTLOverride(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, runAllowed())
	mustDeploy(ctx, t, e, "web", "img:1")
	zero := int32(0)
	if _, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"echo"}, TTLSeconds: &zero}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 || runs[0].TTLSeconds != 0 {
		t.Errorf("recorded runs = %+v, want one with ttl 0", runs)
	}
}

// TestRunRecordsAudit asserts a run records its command in the audit trail (ADR-0027, ADR-0048 §4).
func TestRunRecordsAudit(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, runAllowed())
	mustDeploy(ctx, t, e, "web", "img:1")
	if _, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"echo", "hi"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := d.Audit(ctx, cp.AuditFilter{Operation: "run"})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("run recorded no audit rows")
	}
	found := false
	for _, en := range entries {
		if en.Target == "web" && en.Args["command"] == "echo hi" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit row named the command %q for web (rows: %+v)", "echo hi", entries)
	}
}
