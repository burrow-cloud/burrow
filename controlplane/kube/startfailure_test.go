// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// Issue #478's second half: the reporting. A dependency check pod sat in Init:StartError with the
// kubelet naming the exact executable it could not run, and what reached the operator was
// "skipped (CheckNotRun) — the check did not run to completion". Nothing in the stack was looking.
//
// The hole was specific. Every Job waiter reads WAITING container state, because a terminated
// container has run and its outcome belongs to the Job's counters. A container the kubelet could not
// EXECUTE terminates without ever running, so it has no waiting state to read and no result for the
// counters to carry — and once the Job's Failed counter ticked, the terminal path captured the run
// container, found it had never started, and returned the zero RunResult: exit code 0, no output,
// indistinguishable from a command that succeeded silently.

// failedJobs makes every Get of a Job report one that has FAILED — the terminal state a pod whose
// init container could not be executed produces almost immediately, since the run Job is authored
// with backoffLimit 0 and restartPolicy Never. It is the state that made the waiter's fast-fail path
// irrelevant here: awaitJob sees a terminal Job on its first read and never asks the pod anything.
func failedJobs(client *fake.Clientset, namespace string) {
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	})
}

// startErrorStatus is the pod exactly as the cluster reported it for issue #478: the probe's init
// container terminated with StartError, and the check container never left PodInitializing.
func startErrorStatus() corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  probeInstallContainer,
			Image: "ghcr.io/burrow-cloud/burrowd:v0.14.0-rc.5",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason:   controlplane.ReasonStartError,
				ExitCode: 128,
				Message:  `failed to create containerd task: failed to create shim task: OCI runtime create failed: unable to start container process: error during container init: exec: "/burrowd": stat /burrowd: no such file or directory`,
			}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  runContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		}},
	}
}

// TestRunJobReportsAContainerThatNeverRan is the regression. A Job whose command was never executed
// must return the pod's own reason, on the closed vocabulary, rather than a clean empty result.
func TestRunJobReportsAContainerThatNeverRan(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	failedJobs(client, "apps")
	seedJobPod(t, client, "apps", "burrow-run-r1", startErrorStatus())

	a := New(client, "apps")
	res, err := a.RunJob(ctx, controlplane.RunSpec{
		App: "web", ID: "r1", Image: "repo/web:1.0.0",
		Command: []string{controlplane.ProbePath, controlplane.ProbeCheckCommand},
		Probe:   &controlplane.ProbeSpec{},
	})
	requireBlocked(t, err, controlplane.ReasonStartError, probeInstallContainer, "/burrowd")
	if res.ExitCode != 0 || res.Stdout != "" {
		t.Errorf("result = %+v, want the zero result: nothing ran, so there is no exit code and no output to report", res)
	}
	if res.TimedOut {
		t.Error("TimedOut = true, want false: the container could not be executed, which no amount of waiting fixes")
	}
}

// TestStartErrorIsReadDuringTheWaitToo covers the window before the Job's counters move. The two
// paths must agree: whichever of them observes the pod first, the answer is the same reason and the
// same prose, because there is one renderer (controlplane.IssueEvidence.Message).
func TestStartErrorIsReadDuringTheWaitToo(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	pendingJobs(client, "apps") // neither counter has moved yet
	seedJobPod(t, client, "apps", "burrow-run-r2", startErrorStatus())

	a := New(client, "apps")
	err := mustReturnWithin(t, "RunJob", func() error {
		_, e := a.RunJob(ctx, controlplane.RunSpec{App: "web", ID: "r2", Image: "repo/web:1.0.0", Command: []string{"true"}})
		return e
	})
	requireBlocked(t, err, controlplane.ReasonStartError, probeInstallContainer, "/burrowd")
}

// TestBackupBlamesTheContainerThatNeverRan is the same failure on the backup path, where the wrong
// answer was worse than no answer. The shipping container could not be executed, so it wrote no
// termination message; the Job's failure was therefore read as "no container said why", which the
// code resolves in favour of the step that runs FIRST — pg_dump. So a backup whose dump had
// succeeded was recorded as a dump failure, pointing whoever read it at the one part that worked.
func TestBackupBlamesTheContainerThatNeverRan(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	failedJobs(client, "burrow-addons")
	seedJobPod(t, client, "burrow-addons", "burrow-backup-shop-bk1", corev1.PodStatus{
		Phase: corev1.PodFailed,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  backupDumpContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  backupShipContainer,
			Image: "ghcr.io/burrow-cloud/burrowd:v0.14.0-rc.5",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason:   controlplane.ReasonStartError,
				ExitCode: 128,
				Message:  `unable to start container process: exec: "/burrowd": stat /burrowd: no such file or directory`,
			}},
		}},
	})

	a := New(client, "apps").WithAddonNamespace("burrow-addons")
	outcome, err := a.runBackupJobAwait(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "burrow-backup-shop-bk1", Namespace: "burrow-addons"},
	}, "burrow-backup-shop-bk1", nil)
	if err == nil {
		t.Fatal("the backup Job failed and the call reported success")
	}
	if outcome.Reason == controlplane.BackupReasonDumpFailed {
		t.Error("reason = DumpFailed, but the dump completed — this is the misdiagnosis that pointed at pg_dump while the shipping container was the one that never ran")
	}
	if outcome.Reason != controlplane.ReasonStartError {
		t.Errorf("reason = %q, want %q", outcome.Reason, controlplane.ReasonStartError)
	}
	if !strings.Contains(outcome.Detail, backupShipContainer) {
		t.Errorf("detail = %q, does not name the container that could not be executed", outcome.Detail)
	}
}

// TestAnOrdinaryNonZeroExitIsStillAResult is the boundary this must not cross. `burrow app run`
// reports a non-zero exit as a RESULT, not an error (ADR-0048 §3) — a test suite that exits 1 is the
// answer, not a failure to run — so only a container that never executed may become a blocking
// error. Widening it to any terminated container would turn every failing command into an error.
func TestAnOrdinaryNonZeroExitIsStillAResult(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	failedJobs(client, "apps")
	seedJobPod(t, client, "apps", "burrow-run-r3", corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  runContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 7}},
		}},
	})

	a := New(client, "apps")
	res, err := a.RunJob(ctx, controlplane.RunSpec{App: "web", ID: "r3", Image: "repo/web:1.0.0", Command: []string{"false"}})
	if err != nil {
		t.Fatalf("RunJob: %v, want a result — a command that ran and exited 7 answered the question it was asked", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}
}
