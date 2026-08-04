// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

// A deploy does real work between the call and the answer — it runs a `pre-deploy` hook, writes the
// Deployment, waits for the rollout to settle, checks the dependencies Burrow provisioned, and runs
// a `post-deploy` hook — and every one of those steps happens INSIDE the control plane. A client
// cannot narrate what it cannot see, so the engine reports its own stages and the client renders
// them (issue #480). Without this a deploy is ten to twenty seconds of silence in which a slow image
// pull and a hung command look identical.
//
// THE VOCABULARY IS CLOSED, for the same reason DependencyReasons is: the consumer that matters is a
// program branching on the value, not a person reading prose. A stage nobody decided on must not
// escape into it, and a stage a newer control plane adds must be renderable by an older client
// without looking like a bug.
//
// REPORTING NEVER CHANGES WHAT HAPPENS. Every emit point sits beside work the engine already did, in
// the order it already did it, and an event that nobody asked for is not produced: a deploy with no
// hook configured and no dependency to check reports the one stage it actually ran. Nothing here
// waits, retries, or polls to have something to say.

// The stages of a deploy, in the order they occur. Each names work the engine actually performs, so
// a stage is reported only when that work happens: an app with no `pre-deploy` hook never reports
// pre-deploy-hook, and an app with nothing to settle for never reports settle.
const (
	// StageApply is writing the Deployment to the cluster — the ApplyWorkload call. It is the one
	// stage every deploy runs.
	StageApply = "apply"
	// StagePreDeployHook is the app's `pre-deploy` hook Job (ADR-0072 §2), reported only when a hook
	// is configured. Its failure aborts the deploy (§3), so this stage can fail and end the deploy.
	StagePreDeployHook = "pre-deploy-hook"
	// StageSettle is waiting for the rollout to settle, bounded by `deploy.settle_timeout`
	// (ADR-0072 §5). It is reported only when something asks for the observation — a `post-deploy`
	// hook to tell or a derived dependency to check — because only then does the deploy wait at all.
	StageSettle = "settle"
	// StageDependencyCheck is the deploy-time check of what Burrow provisioned for the app
	// (ADR-0076 §4), reported only when there is a derived dependency and the check is enabled.
	StageDependencyCheck = "dependency-check"
	// StagePostDeployHook is the app's `post-deploy` hook Job (ADR-0072 §4), reported only when a
	// hook is configured. It cannot fail the deploy, which has already landed.
	StagePostDeployHook = "post-deploy-hook"
)

// The status a stage event carries. A stage that starts always reaches done or failed, so a client
// rendering a line on started always has something to close it with.
const (
	// DeployStarted is the stage beginning.
	DeployStarted = "started"
	// DeployDone is the stage finishing without an error.
	DeployDone = "done"
	// DeployFailed is the stage finishing badly. On a stage that aborts the deploy — a `pre-deploy`
	// hook, the apply — the deploy's error follows. On a stage that cannot abort it — the settle,
	// whose RolloutOutcome did not settle — the deploy still succeeds and the failure is a report.
	DeployFailed = "failed"
)

// deployStages is the closed set of stages, in the order a deploy runs them.
var deployStages = []string{
	StagePreDeployHook,
	StageApply,
	StageSettle,
	StageDependencyCheck,
	StagePostDeployHook,
}

// deployStatuses is the closed set of statuses a stage event may carry.
var deployStatuses = []string{DeployStarted, DeployDone, DeployFailed}

// DeployStages returns every stage a deploy may report, in the order they occur, so a caller can
// name the set without reaching into the vocabulary.
func DeployStages() []string {
	return append([]string(nil), deployStages...)
}

// DeployStatuses returns every status a stage event may carry.
func DeployStatuses() []string {
	return append([]string(nil), deployStatuses...)
}

// IsDeployStage reports whether stage is a member of the closed vocabulary.
func IsDeployStage(stage string) bool {
	for _, s := range deployStages {
		if s == stage {
			return true
		}
	}
	return false
}

// IsDeployStatus reports whether status is a member of the closed vocabulary.
func IsDeployStatus(status string) bool {
	for _, s := range deployStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// DeployEvent is one stage transition: which stage, and what happened to it. It is deliberately two
// closed-vocabulary strings and nothing else — a progress report is not a place to grow a second
// result type, and everything a caller needs to say about elapsed time it can measure itself.
type DeployEvent struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
}

// deployProgress is where a deploy's stage events go. It is threaded as an explicit parameter rather
// than carried on the context: this repo passes dependencies explicitly so they can be faked, and a
// reporter hidden in a context is one a reader of the deploy path cannot see.
//
// It is NEVER nil below the top of deploy, which normalises a caller that asked for nothing into a
// no-op once, so no emit site carries a nil check.
type deployProgress func(DeployEvent)

// noProgress is the reporter of a caller that asked for no progress — every path but a deploy that
// requested it, including rollback, the auto-deploy watcher, and an in-cluster build.
func noProgress(DeployEvent) {}

// started, done and failed are the three things any emit site says, named so the call sites read as
// the work they bracket.
func (p deployProgress) started(stage string) { p(DeployEvent{Stage: stage, Status: DeployStarted}) }
func (p deployProgress) done(stage string)    { p(DeployEvent{Stage: stage, Status: DeployDone}) }
func (p deployProgress) failed(stage string)  { p(DeployEvent{Stage: stage, Status: DeployFailed}) }

// finish closes a stage with done or failed, for a site whose outcome is a boolean.
func (p deployProgress) finish(stage string, ok bool) {
	if ok {
		p.done(stage)
		return
	}
	p.failed(stage)
}
