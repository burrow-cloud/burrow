// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// A build is a long-lived, cluster-side operation that was being modelled as a synchronous function
// call (issue #504). The build Job outlives the request BY DESIGN — it is a Kubernetes object with a
// life of its own — and what did not outlive the request was anything that knew what the Job was
// for. So a client that went away mid-build (a proxy's read timeout, a lost network, a closed lid, a
// Ctrl-C) took the whole result with it: the Job cloned, ran every Dockerfile step, pushed the
// image, reached Complete, and no app existed. Minutes of build and a unit of whatever build budget
// applies, spent for nothing.
//
// THE PROPERTY THIS ESTABLISHES: a build that succeeds is never silently discarded.
//
// The mechanism is reconciliation rather than a detached goroutine, because a goroutine is a second
// thing that can go away. A completed build Job with no corresponding release is a STATE, held in
// the cluster, and a state can be noticed by whoever is running now — including a control plane that
// was restarted, redeployed, or upgraded while the build was running, which is the case a goroutine
// on any context cannot survive. The build Job carries its own intent (BuildIntent, recorded by an
// AttributedBuilder), so the reconciler needs nothing that was in the original call frame and
// nothing that was in the control plane's memory.
//
// IT IS THE SAME GUARDED DEPLOY, not a bypass. The recovered image goes through the unexported
// deploy every other path uses — the guardrail decision, the audit trail, the rollout, the deploy
// record, the rollback handle — and it goes through it with NO confirmation, because there is no
// caller present to confirm anything. A held deploy is therefore held here too: recorded as held in
// the audit trail, marked on the build, and left alone. The build is not discarded, so a person who
// then re-runs it with a confirmation reuses the image rather than paying to build it again.

// DefaultBuildReconcileInterval is how often the control plane looks for a build that succeeded and
// was never deployed. A minute is the same cadence the failure observer sweeps on and for the same
// reason: the thing being called is the cluster's own API server, and the call is one Job list in
// one namespace with a label selector that matches nothing at all in the ordinary case.
const DefaultBuildReconcileInterval = time.Minute

// DefaultBuildSettleMargin is how long a build must have been finished before the reconciler will
// treat it as stranded and finish it itself. It is the margin that keeps the reconciler from racing
// the build's ORIGINAL caller, who is still there in the ordinary case and is a few seconds from
// recording a release for the same image.
//
// The overlap it has to cover is small and bounded: between the Job reaching Complete and the
// deploy writing its Pending release there is one pod read and the deploy's prologue — the
// environment, the policy, the replica count, the guardrail decision. Everything slow in a deploy
// (the pre-deploy hook, the rollout, the settle) happens AFTER the release row exists, and the
// release row is what makes a build idempotent here. Two minutes is generous against seconds.
const DefaultBuildSettleMargin = 2 * time.Minute

// BuildReconcilerConfig configures the stranded-build reconciler. The zero value is valid: every
// field falls back to a documented default, so cmd/burrowd can start it with an empty config and a
// test can drive it deterministically by supplying Interval and After.
type BuildReconcilerConfig struct {
	// Interval is the sweep cadence. Zero or negative applies DefaultBuildReconcileInterval.
	Interval time.Duration
	// SettleMargin is how long a build must have been finished before it is treated as stranded.
	// Zero or negative applies DefaultBuildSettleMargin.
	SettleMargin time.Duration
	// After returns a channel that fires after d — the seam the run loop waits on between sweeps.
	// Nil applies time.After. It is injected rather than read off the wall clock so a test drives
	// the loop with no real sleeping and no ambient time (ADR-0010).
	After func(d time.Duration) <-chan time.Time
}

// BuildReconciler finishes builds that succeeded and whose deploy never ran (issue #504). It is
// fail-soft in the way every Burrow sweep is: one build's failure is logged and never stops the
// pass or affects another build, and a pass that cannot list at all simply waits for the next one.
// It reads no ambient time — the settle cutoff comes from the engine's injected clock, and the
// cadence from the injected After seam.
type BuildReconciler struct {
	engine   *Engine
	interval time.Duration
	settle   time.Duration
	after    func(d time.Duration) <-chan time.Time

	// mu guards done and failures, which Run touches from one goroutine but a test may drive
	// concurrently with a read.
	mu sync.Mutex
	// done is the builds this process has decided not to finish — held by a guardrail, superseded, or
	// failed too many times. It exists because the ledger's own marker can FAIL to be written (an
	// install whose Role predates the patch verb is exactly that case), and a decision that cannot be
	// recorded must still be a decision: without this, one permanently held build writes a guardrail
	// row every sweep for as long as its retention lasts. It is deliberately in-memory and lost on
	// restart, so a marker that could not be written is retried once per process rather than never.
	done map[string]bool
	// failures counts consecutive attempts that ended in something other than a decision, per build.
	failures map[string]int
}

// buildFailureCap is how many times a build whose deploy keeps failing for a reason that is not a
// guardrail is retried before this process leaves it alone. A transient failure — an unreachable
// database, a cluster that refused the apply — deserves another sweep; a permanent one (an
// environment that no longer exists) is not going to get better, and retrying it every minute for
// the Job's whole retention costs a list, a pod read, and a full deploy prologue each time.
const buildFailureCap = 5

// NewBuildReconciler builds the reconciler bound to this engine. It reads the engine's build ledger,
// database, and clock seams, and drives the SAME unexported guarded deploy every other path uses.
// Constructing it does not start it — call Run.
func (e *Engine) NewBuildReconciler(cfg BuildReconcilerConfig) *BuildReconciler {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultBuildReconcileInterval
	}
	settle := cfg.SettleMargin
	if settle <= 0 {
		settle = DefaultBuildSettleMargin
	}
	after := cfg.After
	if after == nil {
		after = time.After
	}
	return &BuildReconciler{
		engine:   e,
		interval: interval,
		settle:   settle,
		after:    after,
		done:     map[string]bool{},
		failures: map[string]int{},
	}
}

// decided records that this process will not look at a build again, and reports whether it already
// had. It is the backstop for a ledger marker that could not be written.
func (r *BuildReconciler) decided(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.done[id]
	r.done[id] = true
	return was
}

// alreadyDecided reports whether this process has decided about a build, without deciding.
func (r *BuildReconciler) alreadyDecided(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done[id]
}

// failed counts one attempt that ended in neither a deploy nor a decision, and reports whether the
// build has now failed often enough to be left alone.
func (r *BuildReconciler) failed(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[id]++
	if r.failures[id] < buildFailureCap {
		return false
	}
	r.done[id] = true
	return true
}

// Run sweeps until ctx is cancelled. It runs a pass immediately — the pass that matters most is the
// one right after a restart, because a control plane that went down mid-build is exactly the case
// nothing else covers — then waits the interval before the next, honoring cancellation promptly.
// With no build ledger wired it logs and returns rather than spinning: recovery is an optional seam,
// and an install without it behaves as it did before this existed.
func (r *BuildReconciler) Run(ctx context.Context) {
	if r.engine.buildLedger == nil {
		slog.InfoContext(ctx, "build reconciler disabled: no build ledger seam wired")
		return
	}
	slog.InfoContext(ctx, "build reconciler started", "interval", r.interval, "settle_margin", r.settle)
	for {
		r.reconcile(ctx)
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "build reconciler stopped", "reason", ctx.Err())
			return
		case <-r.after(r.interval):
		}
	}
}

// reconcile runs one sweep. A failure listing stranded builds aborts only this sweep; a per-build
// failure is contained inside finish and never stops the sweep.
func (r *BuildReconciler) reconcile(ctx context.Context) {
	if r.engine.buildLedger == nil {
		return
	}
	cutoff := r.engine.clock.Now().Add(-r.settle)
	stranded, err := r.engine.buildLedger.StrandedBuilds(ctx, cutoff)
	if err != nil {
		slog.WarnContext(ctx, "build reconcile: listing stranded builds failed", "error", err)
		return
	}
	for _, b := range stranded {
		if ctx.Err() != nil {
			return
		}
		r.finish(ctx, b)
	}
}

// finish finishes one stranded build, or decides — once — that it will not.
//
// THE IDEMPOTENCY GUARD IS THE RELEASE HISTORY, and it is the right guard rather than a convenient
// one: the question "was this build discarded?" is exactly the question "does a release exist for
// the image it produced?". It holds against the original caller finishing concurrently, against a
// second sweep, and against a control plane that crashed between deploying and reaping. A release
// recorded FAILED counts too: a deploy that ran and broke is recorded and visible, which is the
// opposite of silently discarded, and re-running it unattended is how a bad image becomes a loop.
//
// THE RECENCY GUARD IS THE OTHER HALF, and without it the fix would be worse than the bug. A build
// that is finished late must never move an app BACKWARDS: if anything has been released for this app
// since the build finished, the operator has moved on, and rolling them back to an older image
// minutes later — unattended, with no client to see it happen — is a far more damaging failure than
// losing a build. So a build that has been overtaken is set aside rather than deployed. It is the
// one case where a successful build is not deployed, and it is deliberate rather than silent: it is
// logged, and the build is left in place so it can still be re-run deliberately.
func (r *BuildReconciler) finish(ctx context.Context, b StrandedBuild) {
	if b.App == "" || b.Image == "" || b.Digest == "" {
		// A build missing any part of what it takes to deploy is not finishable — most likely one
		// started by a control plane that predates attribution. It is left ALONE rather than guessed
		// at, and its own retention reaps it. Half-recorded intent must not become a deploy pinned to
		// a reference nobody wrote.
		return
	}
	if r.alreadyDecided(b.ID) {
		return
	}
	env := envName(b.Env)
	image := pinDigest(b.Image, b.Digest)
	releases, err := r.engine.db.Releases(ctx, b.App, env)
	if err != nil {
		slog.WarnContext(ctx, "build reconcile: reading release history failed", "build", b.ID, "app", b.App, "env", env, "error", err)
		return
	}
	for _, rel := range releases {
		if rel.Image == image {
			// The build reached a deploy after all — the original caller got there, or an earlier
			// sweep did. Nothing to finish; drop the Job so the next sweep has less to look at.
			r.reap(ctx, b)
			return
		}
	}
	if rel, overtaken := releasedSince(releases, b.CompletedAt); overtaken {
		r.setAside(ctx, b, "superseded")
		slog.InfoContext(ctx, "build reconcile: a build that succeeded was overtaken and is not being deployed", "build", b.ID, "app", b.App, "env", env, "image", image, "running", rel.Image, "released_at", rel.CreatedAt)
		return
	}
	if deleted, err := r.appDeletedSince(ctx, b); err != nil {
		slog.WarnContext(ctx, "build reconcile: reading the audit trail failed", "build", b.ID, "app", b.App, "error", err)
		return
	} else if deleted {
		// The app was deleted after this build finished. Deleting an app takes its release history
		// with it, so the guards above see nothing at all — and a deploy here would RESURRECT a
		// workload somebody deliberately removed, past a guardrail that gated the removal.
		r.setAside(ctx, b, "app deleted")
		slog.InfoContext(ctx, "build reconcile: a build is not being deployed because its app was deleted after it finished", "build", b.ID, "app", b.App, "env", env)
		return
	}

	slog.InfoContext(ctx, "build reconcile: finishing a build whose caller went away", "build", b.ID, "app", b.App, "env", env, "image", image, "completed_at", b.CompletedAt)
	// NO CONFIRMATION. There is nobody here to give one, and a guardrail that holds a deploy is not
	// waived by the deploy happening to be unattended — that would make "run it and hang up" a way
	// around the guardrail. Confirm stays false and a held decision is recorded and honored.
	_, derr := r.engine.deploy(ctx, DeployRequest{App: b.App, Env: env, Image: image}, recoveredProvenance())
	if derr == nil {
		r.reap(ctx, b)
		return
	}
	if g, ok := AsGuardrail(derr); ok {
		// The guardrail had its say and it was already recorded in the audit trail by the deploy's own
		// decision row — held or denied, exactly as it would have been for the caller. Set the build
		// aside so the same decision is not re-recorded every minute, and LEAVE IT in place: the image
		// is good and paid for, so re-running the same build with a confirmation reuses it instead of
		// rebuilding.
		r.setAside(ctx, b, string(g.Code))
		slog.InfoContext(ctx, "build reconcile: the deploy of a recovered build was not allowed to proceed unattended", "build", b.ID, "app", b.App, "guardrail", g.Code, "needs_confirmation", g.NeedsConfirmation)
		return
	}
	// Anything else — an unreachable database, a cluster that refused the apply — is worth another
	// sweep, up to a cap. A failure that got as far as recording a release is caught by the
	// idempotency guard above and never retried; one that did not is transient often enough to try
	// again, and one that is permanent (an environment that no longer exists) stops costing anything
	// after buildFailureCap attempts.
	if r.failed(b.ID) {
		slog.WarnContext(ctx, "build reconcile: giving up on a stranded build after repeated failures", "build", b.ID, "app", b.App, "env", env, "attempts", buildFailureCap, "error", derr)
		return
	}
	slog.WarnContext(ctx, "build reconcile: finishing a stranded build failed", "build", b.ID, "app", b.App, "env", env, "error", derr)
}

// releasedSince reports the newest release recorded at or after t, and whether there is one — the
// question "has this app moved on since the build finished?".
func releasedSince(releases []Release, t time.Time) (Release, bool) {
	for i := len(releases) - 1; i >= 0; i-- {
		if !releases[i].CreatedAt.Before(t) {
			return releases[i], true
		}
	}
	return Release{}, false
}

// appDeletedSince reports whether the build's app was deleted after the build finished. It reads the
// audit trail rather than the release history BECAUSE the release history is what a delete removes:
// after `burrow app delete` there is nothing left for the guards above to compare against, and the
// append-only trail is the only record that the app was ever there (ADR-0027).
func (r *BuildReconciler) appDeletedSince(ctx context.Context, b StrandedBuild) (bool, error) {
	rows, err := r.engine.db.Audit(ctx, AuditFilter{App: b.App, Operation: auditOpAppDelete})
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Outcome == AuditExecuted && !row.Timestamp.Before(b.CompletedAt) {
			return true, nil
		}
	}
	return false, nil
}

// setAside records, on the build and in this process, that the control plane will not finish it. The
// ledger marker is what survives a restart; the in-process record is what holds when the marker
// cannot be written at all — an install whose Role predates the patch verb this needs — so a
// decision that could not be recorded is still made once rather than every sweep.
func (r *BuildReconciler) setAside(ctx context.Context, b StrandedBuild, reason string) {
	r.decided(b.ID)
	if err := r.engine.buildLedger.HoldBuild(ctx, b.ID, reason); err != nil {
		slog.WarnContext(ctx, "build reconcile: marking a build the control plane will not finish failed", "build", b.ID, "reason", reason, "error", err)
	}
}

// reap discards a build there is nothing further to do with. It is best-effort: a reap that fails
// leaves a build the next sweep will look at again and take the same no-op path on.
func (r *BuildReconciler) reap(ctx context.Context, b StrandedBuild) {
	if err := r.engine.buildLedger.ReapBuild(ctx, b.ID); err != nil {
		slog.WarnContext(ctx, "build reconcile: reaping a finished build failed", "build", b.ID, "error", err)
	}
}
