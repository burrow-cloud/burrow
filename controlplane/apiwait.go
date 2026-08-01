// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "time"

// How long an API call may occupy the server, declared in one place so a CLIENT can derive its own
// bound from it instead of choosing one next to it.
//
// Several control-plane calls wait on the cluster before they answer: a deploy waits for its rollout
// to settle, a build waits for a build Job, a run waits for a one-off Job. Each of those waits is
// bounded on the server by a number the control plane owns. A client that gives up FIRST does not
// stop any of it — burrowd carries on, finishes the rollout, runs the hook, and records the release —
// it only reports a failure that did not happen, and the obvious response to a failed deploy is to
// deploy again.
//
// That is not hypothetical. The direct-URL client carried a blanket sixty-second HTTP timeout chosen
// before any call legitimately took minutes; once a deploy started waiting for its rollout the
// shorter of the two bounds won, and a successful deploy was reported as failed (issue #404). The two
// numbers were never wrong individually — they were chosen independently, which is the defect this
// file exists to remove. Everything a caller needs to size its own bound is declared here, and
// `client` derives its per-request budgets from these constants rather than restating them.
//
// The values are CEILINGS, not expectations. A deploy normally answers in seconds; MaxDeployWait is
// what a deploy is ALLOWED to take when every phase runs to its own bound, which is the only number
// a client bound may safely be compared against. Sizing a client to the typical case is how the bug
// comes back the first time an operator raises a limit.
const (
	// MaxDeploySettleTimeout is the largest value an operator may set the deploy.settle_timeout
	// limit to. It is the limit catalogue's declared ceiling (see knownLimits), stated as a
	// constant so the catalogue and the callers that must outlast it read the same symbol.
	MaxDeploySettleTimeout = 30 * time.Minute

	// RunJobTimeout is how long the control plane waits for a one-off command Job to reach a
	// terminal state before it reports the command as timed out (ADR-0048). Lifecycle hooks are
	// the same machinery and take the same bound (ADR-0072), so a deploy can spend it twice: once
	// on a `pre-deploy` hook and once on a `post-deploy` one.
	RunJobTimeout = 10 * time.Minute

	// BuildJobTimeout is how long the control plane waits for an in-cluster build Job (ADR-0053).
	// A build ends in a deploy, so a build call can spend this AND the whole deploy budget.
	BuildJobTimeout = 30 * time.Minute

	// BackupJobTimeout is how long the control plane waits for a backup or restore Job
	// (ADR-0032, ADR-0063).
	BackupJobTimeout = 10 * time.Minute

	// PostgresRecoveryTimeout is how long the control plane waits for an instance recovered from its
	// pgBackRest repository to start serving (ADR-0066 §4). It is much larger than a backup's bound
	// because a recovery restores a base backup and then replays archived write-ahead log, both
	// proportional to the data — and unlike a backup, giving up on the wait does NOT stop the work:
	// the recovering `Cluster` is left in place and the operator watches it.
	PostgresRecoveryTimeout = 30 * time.Minute

	// PostgresClaimRemovalTimeout is how long the control plane waits for the pre-restore instance's
	// data claim to actually disappear before recovering into its name (ADR-0066 §4). A claim is held
	// by the pvc-protection finalizer while any pod mounts it, so the delete returns long before the
	// object does — and creating the recovering instance too early would reattach it to the volume it
	// was asked to leave behind.
	PostgresClaimRemovalTimeout = 5 * time.Minute

	// ObjectStoreCallTimeout bounds ONE HTTP call to an S3-compatible object store (ADR-0063).
	// Registering an object-storage provider makes several in a row — create the bucket, write,
	// read and delete the probe object, read and reconcile the lifecycle configuration — so the
	// call's own bound is a multiple of this, not this.
	ObjectStoreCallTimeout = 30 * time.Second
)

// The longest one API request may take, per call, assembled from the bounds above. Each is the sum
// of the phases that request runs, every phase at its own ceiling.
const (
	// MaxDeployWait is the longest a deploy request can occupy the server: a `pre-deploy` hook Job,
	// then the rollout settle, then the deploy-time dependency check (ADR-0076 §4), then the
	// `post-deploy` hook's Job (ADR-0072 §4).
	//
	// The settle bound appears ONCE, and that is load-bearing. Both the check and the hook want to
	// know how the rollout went, and each used to wait for itself — so on a WEDGED rollout, which is
	// precisely the case a bound exists for, a deploy ran the settle timeout twice and this constant
	// had to declare the doubled figure to every client that derives from it (issue #407). The deploy
	// now settles once and hands the same observation to both, so the ceiling is the bound the
	// operator actually configured rather than two of them.
	//
	// Rollback runs a `pre-rollback` hook and the same `post-deploy` settle and hook, which is strictly
	// less than this, so a caller that sizes rollback to the deploy budget covers it.
	MaxDeployWait = RunJobTimeout + // pre-deploy hook
		MaxDeploySettleTimeout + // the rollout settle, made once and shared
		dependencyCheckDeadline + // deploy-time dependency check
		RunJobTimeout // post-deploy hook Job

	// MaxBuildWait is the longest a build request can occupy the server: the build Job, then the
	// deploy the built image rejoins (ADR-0053 §4).
	MaxBuildWait = BuildJobTimeout + MaxDeployWait

	// MaxRunWait is the longest a one-off command request can occupy the server.
	MaxRunWait = RunJobTimeout

	// MaxBackupWait is the longest a backup, a restore, or an add-on removal that takes a final
	// backup first (ADR-0064 §5) can occupy the server.
	MaxBackupWait = BackupJobTimeout

	// MaxRestoreInstanceWait is the longest a physical restore can occupy the server: the safety
	// backup of the state it is about to destroy, the wait for that state's volume to go, and the
	// recovery itself (ADR-0066 §4).
	//
	// The DATABASE_URL cutover afterwards is not a phase with its own ceiling — it is a handful of
	// admin statements and a Secret write per app — and it is not added here rather than being given
	// an invented bound, which is the rule this file holds: a client's budget is a sum of bounds the
	// control plane declares, never of ones a reader estimated.
	MaxRestoreInstanceWait = MaxBackupWait + PostgresClaimRemovalTimeout + PostgresRecoveryTimeout

	// MaxProviderWait is the longest registering an object-storage provider can occupy the server:
	// six object-store calls at their own bound — create the bucket, put/get/delete the probe
	// object, and read then write the lifecycle configuration (ADR-0063 §2-§4).
	MaxProviderWait = 6 * ObjectStoreCallTimeout
)
