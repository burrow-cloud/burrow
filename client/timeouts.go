// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// How long a request may take, per request rather than per client.
//
// A blanket bound cannot be right for both halves of this API. Most calls read something and answer
// in milliseconds, and a minute is a generous ceiling for them. A few WAIT ON THE CLUSTER before
// they answer — a deploy waits for its rollout to settle, a build waits for a build Job, a run waits
// for a one-off Job — and for those a minute is shorter than the work the control plane is entitled
// to do. The blanket sixty seconds this replaces was chosen before any call legitimately took
// minutes; once a deploy started waiting, it turned a deploy that burrowd went on to complete into a
// reported failure, and the natural response to a failed deploy is to deploy again (issue #404).
//
// So the long budgets are DERIVED, never chosen: each is the control plane's own declared bound for
// that call (controlplane/apiwait.go) plus one margin. Nothing here restates a duration the server
// owns, which is what stops the two drifting apart the next time a limit's ceiling moves — raise
// deploy.settle_timeout's ceiling and DeployTimeout follows it in the same compile.
const (
	// DefaultTimeout bounds a call that does not wait on the cluster. It is the bound this package
	// has always applied; what changed is that it no longer applies to the calls that wait.
	DefaultTimeout = 60 * time.Second

	// TimeoutMargin is added to every derived budget to cover what the server's own bound does not:
	// TLS and connection setup, the API-server proxy hop, reading a large response body, and the
	// difference between two clocks. It is deliberately one flat number — a percentage of a
	// ceiling would be minutes of slack on the long calls and no help on the short ones.
	TimeoutMargin = time.Minute

	// DeployTimeout bounds a deploy or a rollback. Both run lifecycle hooks and wait for the rollout
	// to settle; a rollback's phases are a subset of a deploy's, so the deploy budget covers it.
	DeployTimeout = controlplane.MaxDeployWait + TimeoutMargin

	// BuildTimeout bounds an in-cluster build, which is a build Job followed by the deploy the built
	// image rejoins (ADR-0053 §4) — so it is the longest call in the API by some way.
	BuildTimeout = controlplane.MaxBuildWait + TimeoutMargin

	// RunTimeout bounds a one-off command (ADR-0048).
	RunTimeout = controlplane.MaxRunWait + TimeoutMargin

	// BackupTimeout bounds a backup, a restore, and the add-on removal that takes a final backup
	// before destroying a volume (ADR-0064 §5).
	BackupTimeout = controlplane.MaxBackupWait + TimeoutMargin

	// RestoreInstanceTimeout bounds a physical restore of a whole Postgres instance (ADR-0066 §4):
	// the safety backup, the wait for the pre-restore volume to go, and the recovery.
	RestoreInstanceTimeout = controlplane.MaxRestoreInstanceWait + TimeoutMargin

	// ProviderTimeout bounds registering a provider, which for an object store verifies the
	// destination with a run of calls to the vendor before it answers (ADR-0063 §2-§4).
	ProviderTimeout = controlplane.MaxProviderWait + TimeoutMargin
)

// budgets is one client's per-request timeout table. A test substitutes a table of milliseconds so
// the per-request behaviour is exercised without waiting out a real bound.
type budgets struct {
	def    time.Duration
	deploy time.Duration
	build  time.Duration
	run    time.Duration
	backup time.Duration
	// restoreInstance is separate from backup because a physical restore waits for a recovery, which
	// is bounded by how much data there is rather than by a Job's timeout — giving it the backup
	// budget would report a recovery that is still running as a failure, which is the exact defect
	// issue #404 was.
	restoreInstance time.Duration
	provider        time.Duration
}

// derivedBudgets is the table every constructed client gets: the constants above, each derived from
// the control plane's declared bound for that call.
func derivedBudgets() budgets {
	return budgets{
		def:             DefaultTimeout,
		deploy:          DeployTimeout,
		build:           BuildTimeout,
		run:             RunTimeout,
		backup:          BackupTimeout,
		restoreInstance: RestoreInstanceTimeout,
		provider:        ProviderTimeout,
	}
}
