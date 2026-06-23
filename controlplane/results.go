// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

// The types below are the structured inputs and outputs of the engine's operations.
// Every operation returns a result an agent can reason over — what changed and, where
// relevant, the handle to undo it (ADR-0006) — rather than prose.

// DeployRequest is the small, code-free description of a deploy: a pullable image plus
// metadata (ADR-0004). No code travels here.
type DeployRequest struct {
	App      string
	Image    string
	Env      map[string]string
	Command  []string
	Replicas int32
}

// DeployResult reports the outcome of a successful deploy.
type DeployResult struct {
	// Release is the new release that is now running.
	Release Release
	// SupersededReleaseID is the release this deploy replaced, or "" if it was the
	// first deploy of the app. It is the handle a rollback would return to.
	SupersededReleaseID string
}

// StatusResult is the combined control-plane and cluster view of an app.
type StatusResult struct {
	App string
	// HasRelease reports whether the control plane has any release recorded for the
	// app; Release holds the most recent one when true.
	HasRelease bool
	Release    Release
	// Running reports whether a workload currently exists in the cluster; Deployment
	// holds its observed state when true.
	Running    bool
	Deployment DeploymentStatus
}

// ScaleResult reports the outcome of a scale.
type ScaleResult struct {
	App              string
	PreviousReplicas int32
	Replicas         int32
}

// RollbackResult reports the outcome of a rollback. A rollback is itself a forward
// deploy of a prior reference (ADR-0007), so it produces a new Release.
type RollbackResult struct {
	// Release is the new release created by the rollback (carrying the prior
	// reference) and now running.
	Release Release
	// RolledBackToReleaseID is the prior release whose reference was restored.
	RolledBackToReleaseID string
	// SupersededReleaseID is the release that was running before the rollback.
	SupersededReleaseID string
}
