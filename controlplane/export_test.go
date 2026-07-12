// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "context"

// DeployAutoForTest drives the shared deploy path with an AUTO provenance (ADR-0052 §5), so an
// external test can assert how the pull-based watcher's deploys are stamped and audited before the
// Phase 4b poller exists. It exists only in the test build (export_test.go) and is not part of the
// public API; the watcher itself will call the unexported deploy with the same provenance.
func (e *Engine) DeployAutoForTest(ctx context.Context, req DeployRequest, level AutoDeployLevel, tag string) (DeployResult, error) {
	return e.deploy(ctx, req, deployProvenance{trigger: TriggerAuto, level: level, tag: tag})
}
