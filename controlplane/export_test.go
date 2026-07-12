// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"

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
