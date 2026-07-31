// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

// This file is jobwait.go's sibling for a Deployment (ADR-0072 §4-§5). The two answer the same
// question about different objects — "is this finished, and if not, why not" — and they answer it
// through THE SAME functions: deploymentRolledOut is the completion test the status surface already
// uses, and the reason a stalled rollout carries is produced by workloadStatus, which is what
// `burrow app status` reads. A second implementation of "why is this rollout wedged" that could
// disagree with the status surface about the same pod would be worse than the silence it replaces.
//
// It is READ-ONLY and takes no corrective action, for ADR-0074 §9's reason restated by ADR-0072 §6:
// once something notices a crash loop, rolling it back looks like a small addition, and it is not —
// the remedy for a failed deploy is a judgement about blast radius and data. Burrow reports; the
// hook decides.

// rolloutPoll is the interval between Deployment reads while waiting for a rollout to settle. It
// matches runJobPoll: both are a client-side poll of one object against the API server, and there is
// no reason for a reader of the two files to have to notice a difference.
const rolloutPoll = 2 * time.Second

// AwaitRollout waits for app's newest revision to settle, or reports why it did not, bounded by
// timeout (ADR-0072 §4-§5). Every observable condition is a RolloutOutcome; the error return is
// reserved for a call that could not be made at all.
func (a *Adapter) AwaitRollout(ctx context.Context, app string, timeout time.Duration) (controlplane.RolloutOutcome, error) {
	if err := validateAppIdentifier(app); err != nil {
		return controlplane.RolloutOutcome{}, err
	}
	deployments := a.client.AppsV1().Deployments(a.namespace)
	deadline := time.Now().Add(timeout)
	// unread is the most recent read failure, kept so an expired deadline can say the workload could
	// not be read rather than describing replica counts nobody ever saw. A read failure is NOT fatal
	// on its own: an API server hiccup in the middle of a rollout is not a failed deploy, and the
	// honest report for one that never clears is the deadline backstop.
	var unread string
	for {
		dep, err := deployments.Get(ctx, app, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// The registry says this app was rolled out and the cluster has no Deployment for it.
			// ADR-0074 §6 already coined the reason for exactly this shape, so the wait ends here
			// rather than burning its bound on an object that is not coming back.
			return controlplane.RolloutOutcome{
				Reason: controlplane.ReasonWorkloadMissing,
				Detail: "the app's Deployment is not in the cluster, so there is no rollout to wait for",
			}, nil
		case err != nil:
			unread = fmt.Sprintf("the app's Deployment could not be read (%v)", err)
		default:
			unread = ""
			if out, done := rolloutVerdict(ctx, a, dep); done {
				return out, nil
			}
		}
		if !time.Now().Before(deadline) {
			return rolloutDeadlineOutcome(ctx, a, app, timeout, unread), nil
		}
		select {
		case <-ctx.Done():
			// The caller gave up. Report it as the deadline backstop with the honest detail rather
			// than as an error: the hook still has to be told something, and "we stopped waiting" is
			// what happened.
			return controlplane.RolloutOutcome{
				Reason: controlplane.ReasonDeadlineExceeded,
				Detail: fmt.Sprintf("the wait was cancelled after less than %s (%v)", timeout, ctx.Err()),
			}, nil
		case <-time.After(rolloutPoll):
		}
	}
}

// rolloutVerdict reads one Deployment for a final answer, reporting done=false when the rollout is
// legitimately still in progress and the wait should keep going.
//
// The completion test comes FIRST. A Deployment that has fully rolled out is settled even if a pod
// happens to report something a moment later, and asking the pods first would let a pod being
// replaced at the tail of a successful rolling update fail an otherwise-complete deploy.
func rolloutVerdict(ctx context.Context, a *Adapter, dep *appsv1.Deployment) (controlplane.RolloutOutcome, bool) {
	var desired int32
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if deploymentRolledOut(dep, desired) {
		return controlplane.RolloutOutcome{Settled: true}, true
	}
	// Not complete. workloadStatus is the SAME inspection `burrow app status` performs — the pod's
	// blocking conditions ranked down to the one that names the fix, with the progress deadline as
	// the backstop — so the reason a hook is told is the reason a user reads for the same pod.
	st := a.workloadStatus(ctx, dep, unschedulableGrace(ctx, a.limits))
	if st.IssueReason != "" {
		return controlplane.RolloutOutcome{
			Reason: st.IssueReason,
			// Burrow's own summary, not the Issue prose: the crash-loop message carries a tail of the
			// APPLICATION's log, and this value is copied into the hook Job's environment where it
			// would persist in a Kubernetes object (see RolloutOutcome.Detail).
			Detail: rolloutReplicaSummary(st, desired),
		}, true
	}
	if desired == 0 {
		// A scaled-to-zero workload never satisfies the completion test, so waiting for it would
		// always end at the deadline. There is nothing rolling out and nothing wrong.
		return controlplane.RolloutOutcome{Settled: true}, true
	}
	return controlplane.RolloutOutcome{}, false // still legitimately in progress
}

// rolloutDeadlineOutcome builds the backstop: the bound expired without any pod reporting a blocking
// condition (ADR-0072 §5). It reports WHAT WAS OBSERVED — the replica counts, and each pod's phase
// and waiting reason — because the deadline having expired is the least useful fact available, and
// "one pod is Pending with its container ContainerCreating" is what tells a reader where to look.
//
// ContainerCreating and PodInitializing are named here precisely because the Issue criterion
// deliberately excludes them (ADR-0074 §2): they are a pod getting on with it, so they are never an
// Issue, and they are exactly what a rollout that timed out without an Issue is doing.
func rolloutDeadlineOutcome(ctx context.Context, a *Adapter, app string, timeout time.Duration, unread string) controlplane.RolloutOutcome {
	detail := fmt.Sprintf("waited %s", timeout)
	switch {
	case unread != "":
		detail += "; " + unread
	default:
		if observed := observeAppPods(ctx, a, app); observed != "" {
			detail += "; " + observed
		}
	}
	return controlplane.RolloutOutcome{Reason: controlplane.ReasonDeadlineExceeded, Detail: detail}
}

// rolloutReplicaSummary describes a rollout's progress in one Burrow-authored line. Every value in it
// is a count Kubernetes reported about the Deployment, so it can carry nothing the application wrote.
func rolloutReplicaSummary(st controlplane.WorkloadStatus, desired int32) string {
	return fmt.Sprintf("%d of %d replicas updated, %d ready", st.UpdatedReplicas, desired, st.ReadyReplicas)
}

// observeAppPods describes the app's pods as they stand, in one line, for the deadline detail. It is
// observeJobPods for a Deployment: the same description built from the same helper, selected by the
// same nameLabel the workload sets. Best-effort — an unreadable pod list yields "", and the detail
// still names the bound.
//
// It carries container WAITING REASONS and pod phases only. Both are Kubernetes' own short reason
// strings, never a message and never anything the container printed.
func observeAppPods(ctx context.Context, a *Adapter, app string) string {
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + app})
	if err != nil {
		return ""
	}
	if len(pods.Items) == 0 {
		return "the Deployment has no pods"
	}
	var parts []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		desc := fmt.Sprintf("pod %q is %s", pod.Name, pod.Status.Phase)
		if states := containerWaitStates(pod); states != "" {
			desc += " (" + states + ")"
		}
		parts = append(parts, desc)
	}
	return strings.Join(parts, "; ")
}
