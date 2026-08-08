// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"time"

	"golang.org/x/mod/semver"
)

// DeployAutoForTest drives the shared deploy path with an AUTO provenance (ADR-0052 §5), so an
// external test can assert how the pull-based watcher's deploys are stamped and audited before the
// Phase 4b poller exists. It exists only in the test build (export_test.go) and is not part of the
// public API; the watcher itself will call the unexported deploy with the same provenance.
func (e *Engine) DeployAutoForTest(ctx context.Context, req DeployRequest, level AutoDeployLevel, tag string) (DeployResult, error) {
	return e.deploy(ctx, req, deployProvenance{trigger: TriggerAuto, level: level, tag: tag})
}

// ReconcileOnceForTest runs a single auto-deploy reconcile pass over every candidate (app, env), so
// an external test can drive the poller deterministically without the Run loop's timing. It is
// test-build only; production drives the same pass from Run on the injected cadence.
func (p *AutoDeployPoller) ReconcileOnceForTest(ctx context.Context) {
	p.reconcile(ctx)
}

// ReconcileOnceForTest runs a single stranded-build sweep, so an external test can drive the build
// reconciler deterministically without the Run loop's timing. It is test-build only; production
// drives the same sweep from Run on the injected cadence.
func (r *BuildReconciler) ReconcileOnceForTest(ctx context.Context) {
	r.reconcile(ctx)
}

// ObserveOnceForTest runs a single periodic observation pass, so an external test can drive the
// observer deterministically without the Run loop's timing. It is test-build only; production drives
// the same pass from Run on the injected cadence.
func (o *Observer) ObserveOnceForTest(ctx context.Context) {
	o.observe(ctx)
}

// DrainForTest takes in every watch event already delivered and then acts on whatever dwell has
// elapsed — the two steps the Run loop takes between sleeps, without the sleep. A test drives the
// watch path with it: arrange the cluster, call this, read the ledger. Test-build only.
func (o *Observer) DrainForTest(ctx context.Context) {
	o.drain(ctx)
	o.settle(ctx)
}

// SettleForTest acts on every latched transition whose dwell has elapsed, with no new event. It is
// how a test asserts a dwell: advance the injected clock, call this, and see whether the row opened.
// Test-build only.
func (o *Observer) SettleForTest(ctx context.Context) {
	o.settle(ctx)
}

// NextWakeForTest is how long the Run loop would sleep from here — the value that has to shrink to
// the earliest dwell deadline rather than to the periodic cadence, or a dwell is rounded up to the
// pass it was supposed to be finer than. Test-build only.
func (o *Observer) NextWakeForTest() time.Duration { return o.wait() }

// LatchedForTest is how many transitions the latch is holding: the pending ones and the open ones.
// A test asserts on it to show the observer's state is bounded by what Burrow manages rather than by
// how much has ever gone wrong. Test-build only.
func (o *Observer) LatchedForTest() int { return len(o.latch) }

// CompareTagsForTest compares two image tags by stable semver order (negative, zero, positive), for
// a test asserting the watcher never downgrades. Test-build only.
func CompareTagsForTest(a, b string) int {
	return semver.Compare(stableSemver(a), stableSemver(b))
}

// SameMinorForTest reports whether two tags share a major.minor, for a test asserting a patch-level
// app never crosses its minor. Test-build only.
func SameMinorForTest(a, b string) bool {
	return semver.MajorMinor(stableSemver(a)) == semver.MajorMinor(stableSemver(b))
}

// SameMajorForTest reports whether two tags share a major, for a test asserting a minor-level app
// never crosses its major. Test-build only.
func SameMajorForTest(a, b string) bool {
	return semver.Major(stableSemver(a)) == semver.Major(stableSemver(b))
}

// DependencyCheckDeadlineForTest exposes the bound on the deploy-time dependency check (ADR-0076
// §4), so a test can assert it is bounded without the constant becoming public API. It is
// deliberately not an operational limit: §4 asks for no knob, and a limit is a bound somebody is
// enforcing rather than how long a report-only step may hold a landed deploy (ADR-0068 §2).
func DependencyCheckDeadlineForTest() time.Duration { return dependencyCheckDeadline }

// DependencyDetailBytesForTest exposes the bound every dependency detail passes through, so a test
// can assert a composed detail still fits it. Test-build only.
func DependencyDetailBytesForTest() int { return dependencyDetailBytes }
