// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// These tests are issue #352's, and the assertion that carries them is the one about TIME. Every
// waiter here is bounded by a constant measured in minutes (ten for run and backup, thirty for
// build), so a waiter that does not fail fast does not fail the test by returning the wrong answer —
// it fails by not returning at all. failFastLimit is the budget: comfortably longer than the work
// (the fake answers instantly, and awaitJob checks the pod before its first sleep) and far shorter
// than any deadline, so a regression to "poll the counters and hope" shows up as a stall, which is
// exactly the defect.
const failFastLimit = 15 * time.Second

// mustReturnWithin runs f and returns its error, failing the test if f has not returned within
// failFastLimit. f keeps running after a failure — it is polling a fake clientset, and the test
// binary outlives it — because the alternative is threading a cancel into every waiter purely to
// make a failure tidy.
func mustReturnWithin(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(failFastLimit):
		t.Fatalf("%s did not return within %s: it is still polling Job.Status.Succeeded/.Failed, which a pod that cannot start leaves at zero — this is the timeout-instead-of-a-diagnosis defect of issue #352", what, failFastLimit)
		return nil
	}
}

// pendingJobs makes every Get of a Job report a Job that has neither succeeded nor failed — the
// state a Job sits in for its whole deadline when its pod cannot start, and the reason the counters
// alone can never distinguish "still working" from "will never start".
func pendingJobs(client *fake.Clientset, namespace string) {
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}, nil
	})
}

// seedJobPod creates a pod labelled as the Job's own template labels it, so the waiter's selector
// (nameLabel=<job>) finds it — the same selector the Deployment status path uses for an app.
func seedJobPod(t *testing.T, client *fake.Clientset, namespace, job string, status corev1.PodStatus) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job + "-abcde", Namespace: namespace,
			Labels: map[string]string{nameLabel: job},
		},
		Status: status,
	}
	if _, err := client.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
}

// missingSecretStatus is the exact shape of the failure that prompted issue #352: an app Secret that
// is not there, so the kubelet cannot resolve the container's env and parks it in
// CreateContainerConfigError. Job.Status.Failed and .Succeeded both stay at zero forever.
func missingSecretStatus(container, secret string) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: container,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason:  controlplane.ReasonCreateContainerConfigError,
				Message: fmt.Sprintf("secret %q not found", secret),
			}},
		}},
	}
}

// requireBlocked asserts the error is a *controlplane.JobBlockedError carrying reason, so a CALLER
// CAN BRANCH — the point of the reason coming from the closed set rather than from prose (ADR-0074
// §5) — and that the message names each of wants, the fix being what makes the failure actionable.
func requireBlocked(t *testing.T, err error, reason string, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a blocking error naming %s, got nil", reason)
	}
	var blocked *controlplane.JobBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v (%T), want *controlplane.JobBlockedError so a caller can branch on the reason", err, err)
	}
	if blocked.Reason != reason {
		t.Errorf("reason = %q, want %q", blocked.Reason, reason)
	}
	if !controlplane.IsIssueReason(blocked.Reason) {
		t.Errorf("reason %q is outside the closed IssueReason set", blocked.Reason)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not name %q", err.Error(), w)
		}
	}
}

// TestRunJobFailsFastOnMissingSecret is issue #352's headline case on ADR-0048's ten-minute
// synchronous path: `burrow app run` against an app whose Secret is missing. Before the fix this
// returned "did not complete within 10m0s" after ten minutes and named nothing; it must now return
// in one poll naming the Secret.
func TestRunJobFailsFastOnMissingSecret(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	pendingJobs(client, "apps")
	seedJobPod(t, client, "apps", "burrow-run-r1", missingSecretStatus(runContainerName, "web"))

	a := New(client, "apps")
	var res controlplane.RunResult
	err := mustReturnWithin(t, "RunJob", func() error {
		var e error
		res, e = a.RunJob(ctx, controlplane.RunSpec{App: "web", ID: "r1", Image: "busybox:1.36", Command: []string{"true"}})
		return e
	})
	requireBlocked(t, err, controlplane.ReasonCreateContainerConfigError, `secret "web" not found`, runContainerName)
	if res.TimedOut {
		t.Error("TimedOut = true, want false: the pod could not start, which is a diagnosis, not a run worth retrying")
	}
}

// TestRunBackupJobFailsFastOnMissingSecret covers the backup waiter (ADR-0032) on the same shape —
// here the superuser Secret the pg_dump container reads its password from.
func TestRunBackupJobFailsFastOnMissingSecret(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	pendingJobs(client, addonNS)
	seedJobPod(t, client, addonNS, "burrow-pg-backup-bk1", missingSecretStatus("pg", "burrow-pg-superuser"))

	a := New(client, "apps").WithAddonNamespace(addonNS)
	err := mustReturnWithin(t, "RunBackupJob", func() error {
		_, e := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, "bk1")
		return e
	})
	requireBlocked(t, err, controlplane.ReasonCreateContainerConfigError, `secret "burrow-pg-superuser" not found`)
}

// TestRunRestoreJobFailsFastOnUnschedulablePod covers the restore waiter, and the scheduling half of
// the inspection. Restore is the path where this matters most: it is run during an incident, and ten
// minutes of silence followed by "timed out" is ten minutes lost to learning nothing.
//
// The condition's LastTransitionTime is set well past unschedulableGrace, because a pod that has
// only just been marked unschedulable is still within the window the Deployment path (and therefore
// this one) waits before believing it.
func TestRunRestoreJobFailsFastOnUnschedulablePod(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	pendingJobs(client, addonNS)
	seedJobPod(t, client, addonNS, "burrow-pg-restore-bk1", corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			Message:            "0/1 nodes are available: 1 Insufficient cpu.",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * unschedulableGrace)),
		}},
	})

	a := New(client, "apps").WithAddonNamespace(addonNS)
	err := mustReturnWithin(t, "RunRestoreJob", func() error {
		return a.RunRestoreJob(ctx, "shop", controlplane.DefaultEnvironment, "bk1")
	})
	requireBlocked(t, err, controlplane.ReasonUnschedulable, "Insufficient cpu")
}

// TestBuildFailsFastOnInitContainerImagePull covers the build waiter and the INIT container, which
// is where a build Job does its clone (ADR-0053 §4). Its thirty-minute deadline is the longest in
// the product, and a builder or git image that cannot be pulled leaves both Job counters at zero for
// every second of it.
func TestBuildFailsFastOnInitContainerImagePull(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"

	client := fake.NewSimpleClientset()
	pendingJobs(client, buildNamespace)
	seedJobPod(t, client, buildNamespace, buildJobName(source, target), corev1.PodStatus{
		Phase: corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  cloneContainerName,
			Image: "alpine/git:2.45.2",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason:  controlplane.ReasonImagePullBackOff,
				Message: `Back-off pulling image "alpine/git:2.45.2"`,
			}},
		}},
	})

	err := mustReturnWithin(t, "Build", func() error {
		_, e := NewBuilder(client).Build(ctx, source, target, false, controlplane.SourceCredential{})
		return e
	})
	requireBlocked(t, err, controlplane.ReasonImagePullBackOff, "alpine/git:2.45.2")
}

// TestJobWaitKeepsWaitingThroughTransientStates is the other half of the criterion, and the one that
// protects a slow backup from being turned into a failed one. ContainerCreating and PodInitializing
// are a Job getting on with it — they resolve on their own — so they are not in the closed set and
// must not end a wait. The pod here reports them while the Job is still at zero on both counters,
// and only then succeeds; the waiter must ride it out and return the success.
func TestJobWaitKeepsWaitingThroughTransientStates(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	gets := 0
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		gets++
		st := batchv1.JobStatus{}
		if gets > 2 { // two passes with the pod merely starting, then the Job completes
			st.Succeeded = 1
		}
		return true, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS}, Status: st}, nil
	})
	seedJobPod(t, client, addonNS, "burrow-pg-backup-bk1", corev1.PodStatus{
		Phase: corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  "init",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "pg",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}},
		// A pod scheduled only moments ago is inside unschedulableGrace even when the scheduler has
		// already rejected it once, which a rolling cluster does routinely.
		Conditions: []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			Message:            "0/1 nodes are available: 1 Insufficient cpu.",
			LastTransitionTime: metav1.NewTime(time.Now()),
		}},
	})

	a := New(client, "apps").WithAddonNamespace(addonNS)
	err := mustReturnWithin(t, "RunBackupJob", func() error {
		_, e := a.RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, "bk1")
		return e
	})
	if err != nil {
		t.Fatalf("a starting pod must not fail the wait — ContainerCreating, PodInitializing and a just-rejected schedule all resolve on their own: %v", err)
	}
}

// TestJobDeadlineReportsWhatWasObserved covers the backstop. The deadline stays, for a Job wedged
// for a reason no pod reports; what changes is that expiring it reports what the waiter SAW rather
// than only how long it waited. awaitJob is called directly with a millisecond deadline because the
// product constants are minutes long and the behaviour under test is the message, not the duration.
func TestJobDeadlineReportsWhatWasObserved(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	pendingJobs(client, addonNS)
	seedJobPod(t, client, addonNS, "burrow-pg-backup-bk1", corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "pg",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}},
	})

	_, err := awaitJob(ctx, client, addonNS, "burrow-pg-backup-bk1", time.Millisecond, time.Millisecond)
	requireBlocked(t, err, controlplane.ReasonDeadlineExceeded,
		"burrow-pg-backup-bk1-abcde", // the pod, so a reader can go straight to it
		"Pending",                    // its phase
		"ContainerCreating",          // and what it was actually doing when time ran out
	)
	if strings.Contains(err.Error(), "was terminated") {
		t.Error("the deadline message must not claim the Job was terminated: Burrow stops waiting, it does not kill the Job")
	}
}

// TestJobDeadlineReportsNoPod covers the deadline case with no pod at all, which points at the Job
// controller or at admission rather than at anything inside a container — a different place to look,
// so the message says so.
func TestJobDeadlineReportsNoPod(t *testing.T) {
	client := fake.NewSimpleClientset()
	pendingJobs(client, addonNS)

	_, err := awaitJob(context.Background(), client, addonNS, "burrow-pg-backup-bk1", time.Millisecond, time.Millisecond)
	requireBlocked(t, err, controlplane.ReasonDeadlineExceeded, "no pod was created for it")
}

// TestJobPodStartupEvidenceIgnoresTerminatedContainers pins the one place the Job criterion is
// deliberately NARROWER than the Deployment one. A Job pod whose container terminated has run: its
// exit code is the answer `burrow app run` returns as a RESULT (ADR-0048 §3), and an OOM kill fails
// the Job through its own counters. Reading terminated state in the waiter would convert both into
// a wait failure, so the waiter must find nothing here even though the Deployment path would report
// the OOM kill.
func TestJobPodStartupEvidenceIgnoresTerminatedContainers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "burrow-run-r1-abcde"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: runContainerName,
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
			},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: runContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: controlplane.ReasonOOMKilled, ExitCode: 137,
				}},
			}},
		},
	}
	if ev, ok := jobPodStartupEvidence(pod); ok {
		t.Errorf("jobPodStartupEvidence reported %q for a container that already RAN; its outcome belongs to the Job's counters and to the caller", ev.Reason)
	}
	// The Deployment path, by contrast, does report it — the two are deliberately different, and this
	// asserts the difference is the intended one rather than a shared function having been narrowed.
	if _, ok := podIssueEvidence(pod); !ok {
		t.Error("podIssueEvidence must still report an OOM-killed container for a Deployment")
	}
}

// TestJobPodStartupEvidenceHonoursUnschedulableGrace asserts the waiter inherits the Deployment
// path's thirty seconds rather than carrying a second, scattered value of its own (ADR-0068 §6):
// the same pod is invisible inside the grace and reported outside it.
func TestJobPodStartupEvidenceHonoursUnschedulableGrace(t *testing.T) {
	podAt := func(age time.Duration) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			Message:            "0/1 nodes are available: 1 Insufficient cpu.",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
		}}}}
	}
	if _, ok := jobPodStartupEvidence(podAt(unschedulableGrace / 2)); ok {
		t.Error("a pod rejected moments ago must not fail a Job wait: the scheduler marks a pod Unschedulable after ONE attempt, which normal scheduling churn produces")
	}
	ev, ok := jobPodStartupEvidence(podAt(2 * unschedulableGrace))
	if !ok || ev.Reason != controlplane.ReasonUnschedulable {
		t.Errorf("jobPodStartupEvidence(past grace) = %q, %v, want %q, true", ev.Reason, ok, controlplane.ReasonUnschedulable)
	}
}
