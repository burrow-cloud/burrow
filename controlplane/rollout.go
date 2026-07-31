// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

// A rollout does not finish; it either completes, fails, or hangs (ADR-0072 §5). Everything before
// this file could only answer "was the write accepted" — a release marked deployed meant the API
// server took the object, which is not the same fact as "the new pods are serving". This is the
// bounded wait that turns the second question into an answer something can act on, and the ONLY
// consumer of it today is the `post-deploy` hook, which exists to be told how it went (§4).
//
// THE OUTCOME SPEAKS ADR-0074's VOCABULARY AND COINS NOTHING (ADR-0072 §4). A rollout that did not
// settle carries a reason from the closed set the status surface and the failure ledger already use,
// so a hook branches on the same string an agent reads off `burrow app status` or `burrow failures`
// for the same pod at the same moment. A parallel set of outcome strings — "crashed", "stuck",
// "timeout" — would have been a second vocabulary describing one fact, and an agent reading both
// would get two answers.
//
// AND THE EXPIRY IS NOT A BARE TIMEOUT (§5). When the bound runs out with no pod reporting anything
// blocking, the reason is ReasonDeadlineExceeded — the closed set's declared BACKSTOP member — and
// the detail carries what the wait actually observed. That is issue #352's shape stated as a rule:
// a waiter that burns its full deadline and reports elapsed time, when the cluster was saying
// "unschedulable" the entire time, has converted a diagnosis into a shrug.

// RolloutOutcome is what a bounded wait for a rollout to settle observed (ADR-0072 §4-§5). It is the
// answer a `post-deploy` hook is told, and it is deliberately small: an outcome, a reason from a
// closed set, and one Burrow-authored line of context.
type RolloutOutcome struct {
	// Settled reports that the newest revision finished rolling out and is serving — the completion
	// test `kubectl rollout status` uses, not merely that the write was accepted.
	//
	// WHAT IT DOES NOT MEAN is ADR-0072 §7, and it is worth reading before reading "succeeded": the
	// rollout completed and no pod reported a blocking condition. Since ADR-0076 an app that declares
	// a health endpoint (or is published) carries a readiness probe, so for those apps this is a much
	// stronger statement than it was — a pod that never passes its probe never becomes available and
	// the rollout stalls into ReasonProgressDeadlineExceeded. For an app that declares nothing and is
	// not published there is still no probe, and Kubernetes marks a pod ready as soon as its
	// container starts, so an app that boots and returns 500 to every request settles as a success.
	// That is why a smoke test is the natural post-deploy hook: Burrow tells the hook the deploy
	// HAPPENED and the hook decides whether it WORKED.
	Settled bool
	// Reason names why the rollout did not settle. It is a member of the closed vocabulary
	// LedgerReasons() enumerates — ADR-0074 §2's IssueReason set plus §6's intent-versus-cluster
	// absences — and is empty exactly when Settled is true.
	//
	// §6's absences are in scope here for one case and one only: a workload that is GONE while the
	// registry says it was rolled out is ReasonWorkloadMissing, which is precisely the reason ADR-0074
	// coined for it. Reaching for a new string there would have been the one thing ADR-0072 §4 asks
	// this not to do.
	Reason string
	// Detail is ONE Burrow-authored line describing what the wait observed — replica counts, a pod's
	// phase, a container's waiting reason. It is what makes an expired deadline a diagnosis rather
	// than an elapsed time (§5).
	//
	// IT IS NEVER APPLICATION OUTPUT. The live status surface's crash-loop Issue carries a bounded
	// tail of the application's previous log, which may contain anything the app printed — a
	// connection string, a token in a stack frame (ADR-0074 §9). That is an acceptable trade for a
	// value read live and discarded; it is not one for a value copied into a Job's environment, where
	// it would sit in a Kubernetes object for anyone with read access to the namespace. So the hook
	// is told the reason, which is the part it branches on, and the app's own output stays in
	// `burrow app logs` and `burrow app status`, which still carry it.
	Detail string
}

// Failed reports that the wait reached a verdict and the verdict was negative. It is the inverse of
// Settled, named so a caller reads the two cases in the words the hook is told them in.
func (o RolloutOutcome) Failed() bool { return !o.Settled }

// Outcome renders the outcome in the closed two-value vocabulary a post hook is handed: "succeeded"
// or "failed" (ADR-0072 §4). There is no third value. A wait that could not read the cluster is not
// a third kind of answer — it is a wait that ran out of time without observing a settle, which is
// ReasonDeadlineExceeded with a detail that says the workload could not be read.
func (o RolloutOutcome) Outcome() string {
	if o.Settled {
		return OutcomeSucceeded
	}
	return OutcomeFailed
}

// The two values RolloutOutcome.Outcome renders, and therefore the two values a `post-deploy` hook
// ever sees in BURROW_DEPLOY_OUTCOME (ADR-0072 §4).
const (
	// OutcomeSucceeded is a rollout that completed with no pod reporting a blocking condition.
	OutcomeSucceeded = "succeeded"
	// OutcomeFailed is every other verdict, always accompanied by a reason from the closed set.
	OutcomeFailed = "failed"
)
