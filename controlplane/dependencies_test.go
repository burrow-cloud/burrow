// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The deploy-time dependency check (ADR-0076 §4). Two properties carry most of these tests:
// DERIVED — the list comes from what Burrow provisioned and from nothing a user typed — and
// REPORTED, NEVER FATAL — a failure rides back on a deploy that succeeded.

// installPostgresAddon registers the Postgres add-on instance for env in the registry, which is the
// first half of what makes a database dependency derivable: an environment with no instance has
// nothing to be attached to.
func installPostgresAddon(t *testing.T, d *fake.Database, env string) {
	t.Helper()
	name, err := cp.AddonInstanceName(cp.AddonPostgres, env)
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	if err := d.SaveAddon(context.Background(), cp.AddonInfo{
		Name: name, Type: cp.AddonPostgres, Environment: env, Mode: "installed",
	}); err != nil {
		t.Fatalf("SaveAddon: %v", err)
	}
}

// probeStdout renders what a check pod prints, so a test can drive the engine's parsing without a
// cluster. It is the probe's own contract: one marked line carrying the report.
func probeStdout(results ...cp.DependencyResult) string {
	b, err := json.Marshal(cp.ProbeReport{Results: results})
	if err != nil {
		panic(err)
	}
	return "some application banner on stdout\n" + cp.ProbeResultPrefix + string(b) + "\n"
}

// TestDependenciesAreDerivedFromWhatBurrowProvisioned is the whole of §4's "derived, not configured"
// in one table. There is no input here a user could have typed: an attached database is derived from
// the app database listing on the environment's own instance, and a published port is derived from
// the recorded exposure. An app Burrow gave nothing has nothing checked, which is what keeps the
// check free for the common case.
func TestDependenciesAreDerivedFromWhatBurrowProvisioned(t *testing.T) {
	cases := []struct {
		name     string
		attached bool
		port     int32
		want     []cp.DependencyKind
	}{
		{name: "nothing provisioned: nothing checked", want: nil},
		{name: "a database Burrow attached", attached: true, want: []cp.DependencyKind{cp.DependencyPostgres}},
		{name: "a port Burrow published", port: 8080, want: []cp.DependencyKind{cp.DependencyExposure}},
		{
			name: "both", attached: true, port: 8080,
			want: []cp.DependencyKind{cp.DependencyPostgres, cp.DependencyExposure},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _, d, prov := newPostgresEngine(t)
			ctx := context.Background()
			if c.attached {
				installPostgresAddon(t, d, cp.DefaultEnvironment)
				prov.SetAttachedApps(cp.DefaultEnvironment, "web")
			}
			if c.port > 0 {
				exposeApp(t, d, "web", c.port)
			}
			rep, err := e.AppChecks(ctx, "web", "")
			if err != nil {
				t.Fatalf("AppChecks: %v", err)
			}
			var got []cp.DependencyKind
			for _, dep := range rep.Dependencies {
				got = append(got, dep.Kind)
			}
			if len(got) != len(c.want) {
				t.Fatalf("dependencies = %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("dependencies = %v, want %v", got, c.want)
				}
			}
			if !rep.Enabled {
				t.Error("Enabled = false, want true: the check is Burrow's default")
			}
		})
	}
}

// TestDependencyDerivationCarriesNoSecret pins the rule the whole feature has to hold: what Burrow
// reports about a dependency is a key NAME and an address it composed itself, never the credential
// behind them. The fake provisioner's URL carries a password, so a leak would show up here.
func TestDependencyDerivationCarriesNoSecret(t *testing.T) {
	e, _, d, prov := newPostgresEngine(t)
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")

	rep, err := e.AppChecks(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("AppChecks: %v", err)
	}
	if len(rep.Dependencies) != 1 {
		t.Fatalf("dependencies = %v, want one", rep.Dependencies)
	}
	dep := rep.Dependencies[0]
	if dep.EnvKey != "DATABASE_URL" {
		t.Errorf("EnvKey = %q, want DATABASE_URL: the KEY name is what travels, never the value", dep.EnvKey)
	}
	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "fakepw") || strings.Contains(string(blob), "postgres://") {
		t.Fatalf("the checks report carries a connection string: %s", blob)
	}
}

// TestDependencyDerivationSaysWhenItCouldNotAsk is the distinction attachedApps' second return
// exists for, applied here: an instance that will not answer must not read as "no database". A
// silent empty list would report an app with a broken database as an app with no database.
func TestDependencyDerivationSaysWhenItCouldNotAsk(t *testing.T) {
	e, _, d, prov := newPostgresEngine(t)
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	prov.SetListError(errors.New("instance unreachable"))

	rep, err := e.AppChecks(context.Background(), "web", "")
	if err != nil {
		t.Fatalf("AppChecks: %v", err)
	}
	if len(rep.Dependencies) != 0 {
		t.Fatalf("dependencies = %v, want none: nothing could be derived", rep.Dependencies)
	}
	if !strings.Contains(rep.Note, "could not ask") {
		t.Errorf("note = %q, want it to say Burrow could not ask", rep.Note)
	}
	if rep.Note == cp.NoDependenciesNote {
		t.Error("an unreachable instance reported as 'nothing to check', which is the confusion this guards")
	}
}

// TestDependencyChecksAreDisableableAndVisible is ADR-0076's consequence that a Burrow-SUPPLIED
// default on a path ADR-0072 described as user-configured must be visible and disableable rather
// than silent. Both halves: the read names what runs, and the write turns it off.
func TestDependencyChecksAreDisableableAndVisible(t *testing.T) {
	e, _, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")

	rep, err := e.AppChecks(ctx, "web", "")
	if err != nil {
		t.Fatalf("AppChecks: %v", err)
	}
	if !rep.Enabled {
		t.Fatal("a brand new app is not checked; the check is supposed to be the default")
	}
	if len(rep.Dependencies) != 1 || rep.Dependencies[0].Provisioned == "" {
		t.Fatalf("the report does not say what Burrow provisioned: %+v", rep.Dependencies)
	}

	off, err := e.SetAppChecks(ctx, "web", "", false)
	if err != nil {
		t.Fatalf("SetAppChecks(false): %v", err)
	}
	if off.Enabled {
		t.Error("Enabled = true after disabling")
	}
	if !strings.Contains(off.Note, "turned off") {
		t.Errorf("note = %q, want it to say the check is off", off.Note)
	}

	on, err := e.SetAppChecks(ctx, "web", "", true)
	if err != nil {
		t.Fatalf("SetAppChecks(true): %v", err)
	}
	if !on.Enabled {
		t.Error("Enabled = false after re-enabling")
	}
}

// TestDeployRunsTheDerivedCheckInTheAppsOwnImage covers §4's mechanism end to end through the engine:
// a deploy of an app with a database runs ONE check Job, in the app's own image, carrying the app's
// config, asking for the probe injection, and with a plan naming the key rather than the value.
func TestDeployRunsTheDerivedCheckInTheAppsOwnImage(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	if err := d.SetAppEnv(ctx, "web", "LOG_LEVEL", "debug"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}
	k.SetRunResult(cp.RunResult{Stdout: probeStdout(cp.DependencyResult{
		Kind: cp.DependencyPostgres, Outcome: cp.DependencyPassed, Detail: "connected and ran SELECT 1",
	})})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("RunJob calls = %d, want exactly one (the check)", len(runs))
	}
	run := runs[0]
	if run.Image != "repo/web:1.0.0" {
		t.Errorf("check ran image %q, want the app's own image: a check run elsewhere proves the CLUSTER can reach the database, not the app", run.Image)
	}
	if run.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("check env = %v, want the app's own config", run.Env)
	}
	if run.Probe == nil {
		t.Fatal("the check Job did not ask for the probe: the app's image may have no shell and no client tools, so without it there is nothing to execute")
	}
	plan := run.Probe.Env[cp.ProbePlanEnv]
	if plan == "" {
		t.Fatal("the probe carries no plan")
	}
	if strings.Contains(plan, "fakepw") || strings.Contains(plan, "postgres://") {
		t.Fatalf("the plan in the Job spec carries a connection string: %s", plan)
	}
	if !strings.Contains(plan, "DATABASE_URL") {
		t.Errorf("plan = %s, want it to name the DATABASE_URL key so the probe reads the value from its OWN environment", plan)
	}
	if len(res.Dependencies) != 1 || res.Dependencies[0].Outcome != cp.DependencyPassed {
		t.Fatalf("deploy dependencies = %+v, want one passed result", res.Dependencies)
	}
}

// TestFailedDependencyCheckDoesNotFailTheDeploy is the §4/§6 property that everything else is built
// around. The database refuses the credential; the deploy still succeeds, the release is still
// recorded deployed, and the failure is a report on a live release rather than a verdict on it.
func TestFailedDependencyCheckDoesNotFailTheDeploy(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: probeStdout(cp.DependencyResult{
		Kind:    cp.DependencyPostgres,
		Outcome: cp.DependencyFailed,
		Reason:  cp.ReasonAuthenticationFailed,
		Detail:  "burrow-postgres.burrow-addons.svc:5432 rejected the credential (SQLSTATE 28P01)",
	})})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy returned an error for a FAILED dependency check: %v — the check is reported, never fatal (ADR-0076 §4)", err)
	}
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want %q: a failed check must not retroactively fail the release", res.Release.Status, cp.ReleaseDeployed)
	}
	if len(res.Dependencies) != 1 || !res.Dependencies[0].Failed() {
		t.Fatalf("deploy dependencies = %+v, want one failed result", res.Dependencies)
	}
	if res.Dependencies[0].Reason != cp.ReasonAuthenticationFailed {
		t.Errorf("reason = %q, want a member of the closed set", res.Dependencies[0].Reason)
	}
	var hinted bool
	for _, h := range res.Hints {
		if h == cp.DependencyFailureHint {
			hinted = true
		}
	}
	if !hinted {
		t.Error("a failed dependency carried no hint, so an agent reading a successful deploy has nothing pointing at it")
	}
	// The workload is on the cluster and the previous release was superseded: nothing was undone.
	if st, err := k.WorkloadStatus(ctx, "web"); err != nil || st.Image != "repo/web:1.0.0" {
		t.Errorf("workload = %+v (err %v), want the deployed image still applied", st, err)
	}
}

// TestCheckThatCouldNotRunIsSkippedNotFailed keeps the two facts apart that §4 depends on being
// distinguishable. A check pod that could not be scheduled says nothing whatever about the database,
// and reporting it as a failed dependency would send someone to debug a database that is fine.
func TestCheckThatCouldNotRunIsSkippedNotFailed(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	k.SetError(fake.OpRunJob, errors.New("pod could not be scheduled"))

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want one", res.Dependencies)
	}
	got := res.Dependencies[0]
	if got.Outcome != cp.DependencySkipped {
		t.Errorf("outcome = %q, want %q: a check that never ran is not a dependency that failed", got.Outcome, cp.DependencySkipped)
	}
	if got.Reason != cp.ReasonCheckNotRun {
		t.Errorf("reason = %q, want %q", got.Reason, cp.ReasonCheckNotRun)
	}
	if got.Failed() {
		t.Error("a skipped check reported as failed")
	}
}

// TestSkippedCheckCarriesThePodsOwnReason is issue #478's reporting half. A check whose pod could
// not start reported "the check did not run to completion" — the status name in a sentence — while
// the pod sat in a terminal error state naming the exact executable it could not run. Two words hid
// a feature that had never worked on any release.
//
// The OUTCOME stays skipped and the REASON stays CheckNotRun: the check genuinely did not run, and
// an agent branching on the reason must still see that. It is the DETAIL that has to carry what the
// cluster said, and it is passed through unaltered from the blocked Job so the operator reads the
// same prose the status surface would show them for the same pod.
func TestSkippedCheckCarriesThePodsOwnReason(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	k.SetError(fake.OpRunJob, &cp.JobBlockedError{
		Job:    "burrow-run-check-1",
		Reason: cp.ReasonStartError,
		Issue:  cp.IssueEvidence{Reason: cp.ReasonStartError, Container: "burrow-probe-install", Detail: `exec: "/burrowd": stat /burrowd: no such file or directory`}.Message(),
	})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want one", res.Dependencies)
	}
	got := res.Dependencies[0]
	if got.Outcome != cp.DependencySkipped || got.Reason != cp.ReasonCheckNotRun {
		t.Errorf("outcome/reason = %q/%q, want %q/%q: a check that never ran is not a dependency that failed",
			got.Outcome, got.Reason, cp.DependencySkipped, cp.ReasonCheckNotRun)
	}
	for _, want := range []string{"burrow-probe-install", "/burrowd"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, does not name %q — the pod said exactly why and the operator was told only that it did not finish", got.Detail, want)
		}
	}
	if got.Detail == "the check did not run to completion" {
		t.Error("detail is the generic line, so the pod's own reason was thrown away")
	}
}

// TestDeployRunsNoCheckWhenThereIsNothingToCheck is what keeps this free for the common case. An
// unpublished app with no database has no derived dependency, so no pod is created, no image is
// pulled, and no latency is added to a deploy that could not have learned anything.
func TestDeployRunsNoCheckWhenThereIsNothingToCheck(t *testing.T) {
	e, k, _ := newEngine3(t)
	if _, err := e.Deploy(context.Background(), cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Fatalf("RunJob calls = %d, want none: Burrow provisioned nothing for this app", len(runs))
	}
}

// TestDisabledChecksRunNothing is the disable switch actually reaching the deploy path rather than
// only the report.
func TestDisabledChecksRunNothing(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	if _, err := e.SetAppChecks(ctx, "web", "", false); err != nil {
		t.Fatalf("SetAppChecks: %v", err)
	}
	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res.Dependencies) != 0 {
		t.Errorf("dependencies = %+v, want none when the check is off", res.Dependencies)
	}
	if runs := k.RunJobs(); len(runs) != 0 {
		t.Fatalf("RunJob calls = %d, want none when the check is off", len(runs))
	}
}

// TestDependencyCheckAuditRowCarriesNoDetail pins what reaches durable storage. The row records the
// kind, the outcome and the closed-set reason, because those are what a reviewer branches on — and
// deliberately not the detail, which is the same trade auditableHookError makes when it drops a
// hook's captured output.
func TestDependencyCheckAuditRowCarriesNoDetail(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	const detail = "a detail line that must never reach a stored row"
	k.SetRunResult(cp.RunResult{Stdout: probeStdout(cp.DependencyResult{
		Kind: cp.DependencyPostgres, Outcome: cp.DependencyFailed, Reason: cp.ReasonConnectionRefused, Detail: detail,
	})})
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	entries, err := d.Audit(ctx, cp.AuditFilter{})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var found bool
	for _, entry := range entries {
		blob, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(blob), detail) {
			t.Fatalf("an audit row carries the check's detail: %s", blob)
		}
		if entry.Operation != "dependency_check" {
			continue
		}
		found = true
		if !strings.Contains(entry.Args["results"], cp.ReasonConnectionRefused) {
			t.Errorf("audit args = %v, want the closed-set reason", entry.Args)
		}
	}
	if !found {
		t.Error("no dependency_check audit row: the outcome is not recoverable once the deploy result is gone")
	}
}

// TestParseProbeReportForcesTheClosedVocabulary is the boundary guard on output that came back from a
// pod running the USER's image. The binary is Burrow's, but the surface an agent branches on must
// hold even if the answer were not: an invented reason becomes the catch-all rather than escaping.
func TestParseProbeReportForcesTheClosedVocabulary(t *testing.T) {
	raw := `{"results":[
	  {"kind":"postgres","outcome":"failed","reason":"MadeThisUp","detail":"first line\nsecond line"},
	  {"kind":"exposure","outcome":"wat","reason":"AlsoInvented"},
	  {"kind":"postgres","outcome":"passed","reason":"ShouldBeCleared","status":9999}
	]}`
	rep, err := cp.ParseProbeReport("banner\n" + cp.ProbeResultPrefix + strings.ReplaceAll(raw, "\n", "") + "\ntrailing noise\n")
	if err != nil {
		t.Fatalf("ParseProbeReport: %v", err)
	}
	if len(rep.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(rep.Results))
	}
	if rep.Results[0].Reason != cp.ReasonUnreachable {
		t.Errorf("invented reason survived as %q, want the catch-all", rep.Results[0].Reason)
	}
	if strings.Contains(rep.Results[0].Detail, "second line") {
		t.Errorf("detail = %q, want the first line only: anything past it is the user image's own output", rep.Results[0].Detail)
	}
	if rep.Results[1].Outcome != cp.DependencySkipped || rep.Results[1].Reason != cp.ReasonCheckNotRun {
		t.Errorf("unknown outcome = %+v, want a skipped/CheckNotRun result", rep.Results[1])
	}
	if rep.Results[2].Reason != "" {
		t.Errorf("a passed result kept reason %q, want none", rep.Results[2].Reason)
	}
	if rep.Results[2].Status != 0 {
		t.Errorf("status = %d, want it normalised away", rep.Results[2].Status)
	}
	for _, r := range rep.Results {
		if r.Reason != "" && !cp.IsDependencyReason(r.Reason) {
			t.Errorf("reason %q is outside the closed vocabulary", r.Reason)
		}
	}
}

// TestParseProbeReportNeedsItsMarker: a check pod whose image printed something and then died must
// not be read as a report. No marked line is an error, which the engine turns into a SKIPPED result.
// The error is a sentinel, because the engine says WHICH way the read failed rather than one line
// for every cause (issue #474).
func TestParseProbeReportNeedsItsMarker(t *testing.T) {
	_, err := cp.ParseProbeReport("standard_init_linux.go: exec format error\n")
	if err == nil {
		t.Fatal("ParseProbeReport accepted output with no marked line")
	}
	if !errors.Is(err, cp.ErrNoProbeResult) {
		t.Errorf("err = %v, want ErrNoProbeResult so the engine can say the pod printed no result line", err)
	}
	_, err = cp.ParseProbeReport(cp.ProbeResultPrefix + "{not json\n")
	if !errors.Is(err, cp.ErrProbeResultUnreadable) {
		t.Errorf("err = %v, want ErrProbeResultUnreadable: a mangled line was reached and is a different fix", err)
	}
}

// TestSkippedCheckNamesWhatWasAttempted is issue #474's second half. "The check did not run to
// completion" is the status name in a sentence: it named nothing that was tried and left the reader
// with no next move. The detail now says Burrow ran a check pod in the app's own image, and which
// way that ended — here, the case where a user image's entrypoint wraps the command and the result
// line never reaches Burrow.
func TestSkippedCheckNamesWhatWasAttempted(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	// A check pod that ran and printed only the image's own noise: no marked line, so there is no
	// result to read and no blocked pod to blame.
	k.SetRunResult(cp.RunResult{Stdout: "starting entrypoint wrapper\n"})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want one", res.Dependencies)
	}
	got := res.Dependencies[0]
	if got.Outcome != cp.DependencySkipped || got.Reason != cp.ReasonCheckNotRun {
		t.Errorf("outcome/reason = %q/%q, want %q/%q", got.Outcome, got.Reason, cp.DependencySkipped, cp.ReasonCheckNotRun)
	}
	for _, want := range []string{"check pod", "the app's own image", "no result line"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, does not say %q — the reader is told the check did not happen and nothing about what was tried", got.Detail, want)
		}
	}
	if got.Detail == "the check did not run to completion" {
		t.Error("detail is the old generic line, which restates the status name")
	}
}

// TestSkippedCheckSaysTheAppHadNotBecomeReady is the case the issue calls out as worth getting
// right. A check that produced no result while the rollout it waited for never settled is the common
// state right after `burrow addon attach postgres <app>`, which rolls the workload: the app's new
// pods were not ready, and that is both true and actionable.
func TestSkippedCheckSaysTheAppHadNotBecomeReady(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	k.SetRolloutOutcome("web", cp.RolloutOutcome{
		Reason: cp.ReasonProgressDeadlineExceeded,
		Detail: "0 of 1 replicas updated, 0 ready",
	})
	k.SetError(fake.OpRunJob, errors.New("the API server refused the check Job"))

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want one", res.Dependencies)
	}
	got := res.Dependencies[0]
	if got.Reason != cp.ReasonCheckNotRun {
		t.Errorf("reason = %q, want %q: the rollout is context, not a new reason", got.Reason, cp.ReasonCheckNotRun)
	}
	if !strings.Contains(got.Detail, "had not become ready") {
		t.Errorf("detail = %q, does not say the app's new pods were not ready when the check ran", got.Detail)
	}
}

// TestSkippedCheckDetailStaysInsideItsBound pins the composition. A blocked pod's own message can be
// long, and the rollout clause is appended to it — so the cause is trimmed and the clause is not,
// because a message long enough to overflow is exactly where "the app never became ready" explains
// the most.
func TestSkippedCheckDetailStaysInsideItsBound(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	k.SetRolloutOutcome("web", cp.RolloutOutcome{Reason: cp.ReasonProgressDeadlineExceeded, Detail: "0 ready"})
	k.SetError(fake.OpRunJob, &cp.JobBlockedError{
		Job:    "burrow-run-check-1",
		Reason: cp.ReasonUnschedulable,
		Issue:  strings.Repeat("a very long scheduler verdict. ", 40),
	})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v, want one", res.Dependencies)
	}
	got := res.Dependencies[0].Detail
	if len(got) > cp.DependencyDetailBytesForTest() {
		t.Errorf("detail is %d bytes, over the %d-byte bound: %q", len(got), cp.DependencyDetailBytesForTest(), got)
	}
	if !strings.Contains(got, "had not become ready") {
		t.Errorf("detail = %q, lost the rollout clause to the truncation", got)
	}
}

// TestDependencyVolumeCheckAwaitsAppVolumes names the one part of ADR-0076 §4 this does not build.
// The record lists a mounted volume as a third dependency, checked by creating, reading back and
// deleting a file under it. Burrow mounts no volume on a USER's workload — WorkloadSpec has no
// volume field, and every claim in the tree belongs to an add-on or the backup path — so deriving one
// would mean inventing a dependency, which is the thing §4 exists not to do. This is the skipped test
// CLAUDE.md asks for in place of a status note in the record.
func TestDependencyVolumeCheckAwaitsAppVolumes(t *testing.T) {
	t.Skip("ADR-0076 §4's volume check is unbuildable until an app can mount a volume: the app Pod has no Volumes, so there is nothing provisioned to derive a volume dependency from")
}

// newEngine3 is newEngine without the clock, for the cases that do not arrange time.
func newEngine3(t *testing.T) (*cp.Engine, *fake.Kubernetes, *fake.Database) {
	t.Helper()
	e, k, d, _ := newEngine(t, permissive())
	return e, k, d
}

// TestDependencyReasonsAreDistinctFromTheLedgerVocabulary states, as an assertion rather than a
// comment, why this is a separate closed set. ADR-0074 §2's reasons are conditions the CLUSTER
// reported about a pod, and §6's are registry-versus-cluster verdicts; these are answers a connection
// attempt made from inside a container produced, and no Kubernetes controller can ever emit one.
// Merging them would put a value nothing in the cluster can produce into a field documented as the
// cluster's own reason.
func TestDependencyReasonsAreDistinctFromTheLedgerVocabulary(t *testing.T) {
	ledger := map[string]bool{}
	for _, r := range cp.LedgerReasons() {
		ledger[r] = true
	}
	for _, r := range cp.DependencyReasons() {
		if ledger[r] {
			t.Errorf("%q is in both the dependency and the ledger vocabulary; the two answer different questions and must not be merged", r)
		}
	}
	if len(cp.DependencyReasons()) == 0 {
		t.Fatal("the dependency vocabulary is empty")
	}
}

// TestSetAppChecksIsPerEnvironment: the same app in two environments has different dependencies —
// one Postgres instance per environment (ADR-0067 §1) — so turning the check off in one must not
// reach the other.
func TestSetAppChecksIsPerEnvironment(t *testing.T) {
	e, _, d, _ := newPostgresEngine(t)
	ctx := context.Background()
	if err := d.CreateEnvironment(ctx, "staging", "burrow-staging"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if _, err := e.SetAppChecks(ctx, "web", "staging", false); err != nil {
		t.Fatalf("SetAppChecks(staging): %v", err)
	}
	prod, err := e.AppChecks(ctx, "web", "")
	if err != nil {
		t.Fatalf("AppChecks(prod): %v", err)
	}
	if !prod.Enabled {
		t.Error("disabling the check in staging turned it off in production too")
	}
	staging, err := e.AppChecks(ctx, "web", "staging")
	if err != nil {
		t.Fatalf("AppChecks(staging): %v", err)
	}
	if staging.Enabled {
		t.Error("the staging setting did not take")
	}
}

// TestChecksSettingIsForgottenWhenTheAppIs: an app created later under the same name must start
// checked, which is Burrow's default, rather than inherit a previous occupant's opt-out.
func TestChecksSettingIsForgottenWhenTheAppIs(t *testing.T) {
	e, _, d, _ := newPostgresEngine(t)
	ctx := context.Background()
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailAppDelete, cp.DispositionAllow))
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if _, err := e.SetAppChecks(ctx, "web", "", false); err != nil {
		t.Fatalf("SetAppChecks: %v", err)
	}
	if err := e.DeleteApp(ctx, "web", "", true); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	rep, err := e.AppChecks(ctx, "web", "")
	if err != nil {
		t.Fatalf("AppChecks: %v", err)
	}
	if !rep.Enabled {
		t.Error("a new app under a deleted app's name inherited its opt-out")
	}
}

// TestProbePlanRoundTrip is the contract between the engine that writes the plan and the probe that
// reads it back inside the app's container.
func TestProbePlanRoundTrip(t *testing.T) {
	want := cp.ProbePlan{Checks: []cp.ProbeCheck{
		{Kind: cp.DependencyPostgres, EnvKey: "DATABASE_URL"},
		{Kind: cp.DependencyExposure, URL: "http://web.burrow-apps.svc"},
	}}
	encoded, err := cp.MarshalProbePlan(want)
	if err != nil {
		t.Fatalf("MarshalProbePlan: %v", err)
	}
	got, err := cp.ParseProbePlan(encoded)
	if err != nil {
		t.Fatalf("ParseProbePlan: %v", err)
	}
	if len(got.Checks) != len(want.Checks) {
		t.Fatalf("checks = %+v, want %+v", got.Checks, want.Checks)
	}
	for i := range got.Checks {
		if got.Checks[i] != want.Checks[i] {
			t.Fatalf("check %d = %+v, want %+v", i, got.Checks[i], want.Checks[i])
		}
	}
}

// TestDependencyCheckHasNoOperationalLimit states the ADR-0068 §2 judgement in a test, because the
// obvious next contribution to this file is to make the deadline configurable. §4 asks for no knob,
// and a limit is a bound somebody is enforcing rather than an implementation detail of a report-only
// step — the same reasoning that keeps the readiness probe's cadence out of the limits table.
func TestDependencyCheckHasNoOperationalLimit(t *testing.T) {
	for _, code := range cp.LimitCodes() {
		if strings.Contains(string(code), "dependency") || strings.Contains(string(code), "check") {
			t.Errorf("limit %q was added for the dependency check; ADR-0076 §4 asks for no such knob", code)
		}
	}
	// The deadline is real and must stay bounded, whatever it is: an unbounded check would hold a
	// deploy result that is already correct.
	if dl := cp.DependencyCheckDeadlineForTest(); dl <= 0 || dl > 10*time.Minute {
		t.Errorf("the check deadline is %s, which is either unbounded or long enough to hold up a landed deploy", dl)
	}
}

// TestDependencyCheckWaitsForTheRolloutToSettle is ADR-0072 §4's "after the rollout settles" applied
// to the check that rides on that phase. It is what makes the exposure check honest: the Service
// routes to READY pods, so a check run mid-rollout would reach the previous release's replicas and
// report on the version this deploy just replaced.
func TestDependencyCheckWaitsForTheRolloutToSettle(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	exposeApp(t, d, "web", 8080)
	k.SetRunResult(cp.RunResult{Stdout: probeStdout(cp.DependencyResult{
		Kind: cp.DependencyExposure, Outcome: cp.DependencyPassed, Status: 200,
	})})

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	rollouts := k.Rollouts()
	if len(rollouts) == 0 {
		t.Fatal("the deploy did not wait for the rollout to settle before checking; the exposure check would have reached the release this deploy replaced")
	}
	if rollouts[0].App != "web" {
		t.Errorf("waited on %q, want web", rollouts[0].App)
	}
}

// TestDependencyCheckRunsEvenWhenTheRolloutDidNotSettle: a stalled rollout is the case the check has
// the MOST to say about, because an app that cannot reach the database it was given is a common
// reason a rollout never becomes ready. The settle is a wait, not a gate.
func TestDependencyCheckRunsEvenWhenTheRolloutDidNotSettle(t *testing.T) {
	e, k, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")
	// A rollout that never settles: the wait returns a verdict rather than a success.
	k.SetRolloutOutcome("web", cp.RolloutOutcome{
		Reason: cp.ReasonProgressDeadlineExceeded,
		Detail: "0 of 1 replicas updated, 0 ready",
	})
	k.SetRunResult(cp.RunResult{Stdout: probeStdout(cp.DependencyResult{
		Kind: cp.DependencyPostgres, Outcome: cp.DependencyFailed, Reason: cp.ReasonCredentialUnset,
		Detail: "DATABASE_URL is not set in this app's container",
	})})

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "repo/web:1.0.0"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res.Dependencies) != 1 || !res.Dependencies[0].Failed() {
		t.Fatalf("dependencies = %+v, want the check to have run and reported on a rollout that stalled", res.Dependencies)
	}
	if res.Dependencies[0].Reason != cp.ReasonCredentialUnset {
		t.Errorf("reason = %q, want the check's own verdict rather than the rollout's", res.Dependencies[0].Reason)
	}
}
