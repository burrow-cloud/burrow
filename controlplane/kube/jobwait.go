// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane"
)

// This file is the one Job wait loop (issue #352). Every Job Burrow runs — build, run, backup,
// restore — used to carry its own copy of the same loop, and every copy had the same hole: it polled
// Job.Status.Succeeded and .Failed, which a pod that never STARTS leaves both at zero. A missing
// Secret, an unpullable image, or a cluster with no room therefore produced no signal at all until
// the client-side deadline expired, at which point the caller was told how long it had waited and
// nothing about why. ADR-0074 names that shape and ADR-0072 §5 calls it what it is: a waiter that
// burns its full deadline when the cluster was saying "unschedulable" the entire time has converted
// a diagnosis into a shrug. It is worse for an agent than for a person — a timeout carries no signal
// distinguishing "slow" from "cannot start", so the agent's reasonable next move is to retry and
// wait the whole deadline again.
//
// The waiter fails fast on a blocking, human-fixable condition and keeps the deadline as the
// backstop for a Job wedged for a reason no pod reports. What counts as blocking is NOT decided here
// — it is ADR-0074 §2's closed vocabulary, read by the same functions the Deployment status path
// reads it with (waitingContainerIssueEvidence, schedulingIssueEvidence, issueRank, selectPodIssue).
// A second implementation of "why is this pod blocked" that could disagree with the status surface
// about the same pod would be worse than the bug this fixes.

// awaitJob polls the Job until it reaches a terminal state (Succeeded or Failed), a pod reports a
// blocking condition, the deadline expires, or ctx is cancelled. It returns the Job as last read on
// the terminal path; every other path returns an error, and a blocking condition or an expired
// deadline returns a *controlplane.JobBlockedError carrying a reason from the closed set.
//
// The blocking check runs BEFORE the first sleep, so a Job whose pod is already stuck when the
// waiter starts (a re-run reusing an existing Job, or a pod the scheduler rejected immediately)
// fails on the first pass rather than after one poll interval.
//
// It does not delete anything. A Job left behind by any of these paths keeps its pod and its logs
// for diagnosis, which is what every caller already did with a failed Job.
//
// grace is the configured unschedulable grace (ADR-0068 §6). The caller resolves it from the
// operational configuration and passes it in, so a waiter believes an Unschedulable pod at exactly
// the moment the status surface does.
func awaitJob(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout, poll, grace time.Duration) (*batchv1.Job, error) {
	return awaitJobObserved(ctx, client, namespace, name, timeout, poll, grace, nil)
}

// awaitJobObserved is awaitJob with an observer called once per poll on a Job that is still working,
// after the terminal and blocking checks and before the sleep. It exists for the in-cluster build,
// which reports what it is doing across the minutes it takes (issue #503) and has nowhere else to do
// it from: the wait loop is the only place that learns anything while the Job runs.
//
// The observer is a REPORTER, never a decision: it is called on the waiting goroutine, its result is
// ignored, and nothing it does changes whether the wait continues. A nil observer is the wait every
// other Job takes, unchanged.
func awaitJobObserved(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout, poll, grace time.Duration, observe func()) (*batchv1.Job, error) {
	jobs := client.BatchV1().Jobs(namespace)
	deadline := time.Now().Add(timeout)
	for {
		j, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("kube: reading job %q: %w", name, err)
		}
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
			return j, nil // terminal: the caller decides what a success and a failure each mean
		}
		// Neither counter has moved. Before concluding "still working", ask the pod — this is the
		// whole point of the file.
		if ev, ok := jobStartupIssue(ctx, client, namespace, name, grace); ok {
			// Wrapped, not replaced: the package's errors carry a "kube:" prefix, and errors.As still
			// reaches the reason a caller branches on.
			return nil, fmt.Errorf("kube: %w", &controlplane.JobBlockedError{Job: name, Reason: ev.Reason, Issue: ev.Message()})
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("kube: %w", jobDeadlineError(ctx, client, namespace, name, timeout))
		}
		if observe != nil {
			observe()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// jobStartupIssue reports the highest-ranked blocking condition any of the Job's pods reports, or
// ok=false when none does. Pods are selected by the same nameLabel=<job name> the Job's own template
// sets, which every Burrow Job (build, run, backup, restore) carries.
//
// Best-effort, exactly as the status path is: an unreadable pod list yields ok=false and the waiter
// keeps waiting. Enrichment must never be the thing that fails a Job.
func jobStartupIssue(ctx context.Context, client kubernetes.Interface, namespace, job string, grace time.Duration) (controlplane.IssueEvidence, bool) {
	ev, _, ok := selectPodIssue(ctx, client, namespace, nameLabel+"="+job, func(pod *corev1.Pod) (controlplane.IssueEvidence, bool) {
		return jobPodStartupEvidence(pod, grace)
	})
	return ev, ok
}

// jobPodStartupEvidence reads one Job pod for a condition that blocks it from STARTING. It differs
// from the Deployment path's podIssueEvidence in exactly two ways, both of them because a Job pod is
// not a Deployment pod:
//
//   - Only WAITING container state counts. A Job pod whose container terminated has run, and what it
//     did belongs to the Job's counters and to the caller: `burrow app run` reports a non-zero exit
//     as a result rather than an error (ADR-0048 §3), and an OOM kill fails the Job through
//     Status.Failed. Reading terminated state here would turn a completed run into a wait failure.
//     The one exception is a container that terminated WITHOUT EXECUTING — StartError — which is not
//     a run at all and leaves no result for the counters to carry (startErrorEvidence, issue #478).
//   - INIT containers are read too. Every Burrow Job that has one puts real work there — the build
//     Job clones the source in an init container mounting the credential Secret (ADR-0053 §4,
//     ADR-0057 §4) — so an init container that cannot start is precisely the case this exists for,
//     and it leaves the main container's status empty. Init containers are read FIRST because the
//     pod cannot get past them.
//
// The criterion itself is unchanged and is not re-decided here: blocking and human-fixable, never
// self-resolving (ADR-0074 §2). ContainerCreating and PodInitializing are a Job getting on with it,
// so they are not in the closed set, so they are not reported — which is what keeps a slow image
// pull from being turned into a failed backup.
func jobPodStartupEvidence(pod *corev1.Pod, grace time.Duration) (controlplane.IssueEvidence, bool) {
	var best controlplane.IssueEvidence
	bestRank := 0
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range statuses {
			ev, ok := waitingContainerIssueEvidence(pod, cs)
			if !ok {
				// A container the kubelet created but could not EXECUTE is the one exception to "only
				// waiting states count" — it terminated without running, so there is no result the
				// Job's counters could be carrying (issue #478). Everything else terminated is a run.
				ev, ok = startErrorEvidence(cs)
			}
			if !ok {
				continue
			}
			if r := issueRank(ev.Reason); r > bestRank {
				best, bestRank = ev, r
			}
		}
	}
	if bestRank > 0 {
		return best, true
	}
	// No container reports anything, which is what a pod that was never scheduled looks like. The
	// scheduling read carries the configured unschedulable grace with it — the SAME value the
	// Deployment path waits out before believing an Unschedulable pod, deliberately not a second
	// setting of this waiter's own (ADR-0068 §6; see unschedulableGrace in adapter.go).
	return schedulingIssueEvidence(pod, grace)
}

// jobDeadlineError builds the error for a Job that outlasted its client-side deadline without any
// pod reporting a blocking condition — the backstop, for a Job wedged for a reason nothing reports.
// It reports WHAT WAS OBSERVED rather than a bare elapsed time: the deadline having expired is the
// least useful fact available, and "the pod is still Pending with no scheduling verdict" or "the
// container has been ContainerCreating the whole time" is what tells a reader where to look next.
//
// The observation is best-effort. When the pods cannot be read the message still names the deadline,
// which is no worse than what every waiter reported before.
func jobDeadlineError(ctx context.Context, client kubernetes.Interface, namespace, job string, timeout time.Duration) error {
	detail := fmt.Sprintf("waited %s", timeout)
	if observed := observeJobPods(ctx, client, namespace, job); observed != "" {
		detail += "; " + observed
	}
	ev := controlplane.IssueEvidence{Reason: controlplane.ReasonDeadlineExceeded, Detail: detail}
	return &controlplane.JobBlockedError{Job: job, Reason: ev.Reason, Issue: ev.Message()}
}

// observeJobPods describes the Job's pods as they stand, in one line, for the deadline message. It
// names the states the CRITERION deliberately excludes — ContainerCreating, PodInitializing, a pod
// still Pending — because those are exactly what a Job that timed out without a blocking condition
// is doing, and naming them is the difference between a diagnosis and a shrug. It is a description
// for a human to read, not a classification: nothing branches on this text.
func observeJobPods(ctx context.Context, client kubernetes.Interface, namespace, job string) string {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + job})
	if err != nil {
		return ""
	}
	if len(pods.Items) == 0 {
		// No pod at all after the full deadline points at the Job controller or admission, not at
		// anything inside a container, and that is a different place to look.
		return "no pod was created for it"
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

// containerWaitStates renders each not-yet-running container's waiting reason, so the deadline
// message can say which container is stuck and on what.
func containerWaitStates(pod *corev1.Pod) string {
	var parts []string
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil {
				reason := w.Reason
				if reason == "" {
					reason = "waiting"
				}
				parts = append(parts, fmt.Sprintf("container %q: %s", cs.Name, reason))
			}
		}
	}
	return strings.Join(parts, ", ")
}
