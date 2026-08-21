// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

const (
	// runJobTimeout caps how long burrowd waits for a one-off command Job to finish (ADR-0048 §3),
	// lifecycle hooks included (ADR-0072). It is declared in controlplane/apiwait.go rather than
	// here, because a client's own bound has to outlast it (issue #404).
	runJobTimeout = controlplane.RunJobTimeout
	// runJobPoll is the interval between Job-status reads while waiting.
	runJobPoll = 2 * time.Second
	// runContainerName is the single container a run Job carries. It is fixed (not the app name) so
	// the pod-log lookup and capture never depend on the app identifier.
	runContainerName = "run"
)

// RunJob runs spec.Command in a one-shot Job in the app namespace, built from the app's own current
// image and its config env plus per-app Secret via envFrom (ADR-0048 §2), then waits for it to
// finish and captures the pod's output and the container's exit code into a RunResult (ADR-0048 §3).
// A non-zero exit is a normal structured outcome, NOT an error: the error return is reserved for a
// launch, poll, or timeout failure, and for an app pod mutator that refused to shape the Job's pod
// (WithAppPodMutator), in which case no Job is created at all. The finished Job is garbage-collected by Kubernetes' native
// ttlSecondsAfterFinished (ADR-0048 §7), set from spec.TTLSeconds — no imperative reap.
func (a *Adapter) RunJob(ctx context.Context, spec controlplane.RunSpec) (controlplane.RunResult, error) {
	if err := validateAppIdentifier(spec.App); err != nil {
		return controlplane.RunResult{}, err
	}
	name := fmt.Sprintf("burrow-run-%s", spec.ID)
	job, err := a.runJob(name, spec)
	if err != nil {
		return controlplane.RunResult{}, fmt.Errorf("kube: building run job %q: %w", name, err)
	}
	jobs := a.client.BatchV1().Jobs(a.namespace)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return controlplane.RunResult{}, fmt.Errorf("kube: creating run job %q: %w", name, err)
	}

	// awaitJob returns as soon as the Job is terminal, or as soon as its pod reports a condition that
	// means it will never start (issue #352). `burrow app run` is the sharpest case for that: it is
	// synchronous with a TEN-MINUTE bound (ADR-0048 §3), so before this a misconfigured app meant a
	// user or an agent waited ten minutes to be told "timed out", with nothing anywhere naming the
	// Secret that was missing.
	if _, err := awaitJob(ctx, a.client, a.namespace, name, runJobTimeout, runJobPoll, unschedulableGrace(ctx, a.limits)); err != nil {
		// The deadline backstop keeps its TimedOut result: a caller distinguishes "we stopped
		// waiting" from "the pod could not start", and only the former is a candidate for a retry.
		var blocked *controlplane.JobBlockedError
		if errors.As(err, &blocked) && blocked.TimedOut() {
			return controlplane.RunResult{TimedOut: true}, err
		}
		return controlplane.RunResult{}, err
	}
	// Terminal (Complete or Failed): capture the pod's output and exit code before the TTL controller
	// garbage-collects it. Both terminal states carry the same answer (ADR-0048 §3) — a non-zero exit
	// is a result, not an error, which is why awaitJob deliberately ignores terminated containers.
	return a.captureRun(ctx, name)
}

// captureRun reads a finished run Job's pod for the container's exit code and its combined log output
// (ADR-0048 §3, §6). Kubernetes' pod-log API returns stdout and stderr as one interleaved stream, so
// the captured text lands in Stdout; Stderr is reserved for a future separation. Best-effort in the
// ordinary case: a missing pod or an unreadable log yields whatever was captured, never an error —
// the Job already reached a terminal state, which is the answer the caller returns.
//
// The ONE case it refuses to answer for is a pod whose run container never ran (issue #478). Before,
// that returned the zero RunResult — exit code 0 and no output, which is indistinguishable from a
// command that succeeded silently — so a check pod whose init container could not execute reported
// as a clean, empty run and the caller was left to guess. When the pod names a container that was
// created but never executed, that is returned as a JobBlockedError carrying the kubelet's own
// message, on the same closed vocabulary every other blocked Job uses.
func (a *Adapter) captureRun(ctx context.Context, jobName string) (controlplane.RunResult, error) {
	var res controlplane.RunResult
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + jobName})
	if err != nil {
		return res, nil
	}
	var blocked controlplane.IssueEvidence
	blockedRank := 0
	for _, pod := range pods.Items {
		var terminated *corev1.ContainerStateTerminated
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil {
				terminated = cs.State.Terminated
				break
			}
		}
		if terminated == nil {
			// A pod that never ran to a terminal state carries no exit code. Ask why before moving on:
			// an init container the kubelet could not execute is the reason the run container has no
			// state at all, and it is the whole diagnosis.
			for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
				for _, cs := range statuses {
					if ev, ok := startErrorEvidence(cs); ok {
						if r := issueRank(ev.Reason); r > blockedRank {
							blocked, blockedRank = ev, r
						}
					}
				}
			}
			continue
		}
		res.ExitCode = int(terminated.ExitCode)
		if stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx); err == nil {
			data, _ := io.ReadAll(stream)
			stream.Close()
			res.Stdout = string(data)
		}
		return res, nil
	}
	if blockedRank > 0 {
		return controlplane.RunResult{}, fmt.Errorf("kube: %w",
			&controlplane.JobBlockedError{Job: jobName, Reason: blocked.Reason, Issue: blocked.Message()})
	}
	return res, nil
}

// runJob builds the one-shot Job for a run: the app's own current image and command, its config env,
// and its per-app Secret via envFrom (ADR-0048 §2), in the adapter's app namespace. RestartPolicy
// Never and BackoffLimit 0 make a single attempt whose exit code is the result — no retry masking a
// non-zero exit. TTLSecondsAfterFinished sets the native garbage-collection window (ADR-0048 §7).
//
// The pod is then handed to the ADR-0061 mutator, exactly as the app Deployment's pod template is:
// a run is the app's own image in the app's namespace with the app's environment, so it belongs
// under the same placement and runtime policy the app itself was deployed under.
func (a *Adapter) runJob(name string, spec controlplane.RunSpec) (*batchv1.Job, error) {
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue}
	var backoff int32
	ttl := spec.TTLSeconds

	// The app's config and its per-app Secret, assembled by the SAME function the app's own Deployment
	// is (ADR-0048 §2), so DATABASE_URL and every other secret resolve as the app sees them — through
	// exactly the door the app reads them through.
	//
	// THAT SHARED FUNCTION IS THE POINT. A key the app marked file-only (ADR-0089 §4) is not in this
	// Job's environment either, which matters most here of all: a run is where a shell gets started
	// and an environment variable is inherited by every child process, so a Job that sourced the
	// Secret wholesale would put a credential back exactly where the app took it out of. It arrives
	// as the file it is instead.
	env, envFrom, volumes, mounts := appEnvironment(spec.App, spec.Env, spec.SecretFiles, spec.SecretEnvKeys)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Volumes:       volumes,
					Containers: []corev1.Container{{
						Name:         runContainerName,
						Image:        spec.Image,
						Command:      spec.Command,
						Env:          env,
						EnvFrom:      envFrom,
						VolumeMounts: mounts,
					}},
				},
			},
		},
	}

	// A run that carries a ProbeSpec is the deploy-time dependency check (ADR-0076 §4): the app's own
	// image, with Burrow's binary made executable inside it by an init container, because the image
	// this most needs to run in is the one with no shell and no client tools. It is attached BEFORE
	// the mutators below so the init container takes the same placement policy the check container
	// does — a pod whose two halves could land under different constraints would be a pod that never
	// schedules.
	withProbe(&job.Spec.Template.Spec, spec.Probe, a.probeInitContainer())

	// Apply the ADR-0061 extension point last, over the fully-constructed pod spec, exactly as
	// buildDeployment does — same adapter, same hook, same app. Without it a run Job is authored with
	// no toleration and no runtimeClassName on a cluster whose app pods only schedule with them, and
	// the failure is quiet: the Job sits Pending until RunJob's ten-minute deadline and reports a
	// timeout rather than an unschedulable pod. No mutator leaves the Job exactly as built above.
	//
	// The identity handed over names the SAME app the Deployment's does, which is the point: a
	// per-app policy the deploy was given reaches the run pod without the wiring author restating it.
	// Workload distinguishes the two, because a run pod is a Job pod and a hook may need to know.
	// A hook that refuses stops the run here, with the Job built and never created. That is the same
	// outcome the deploy path takes, and it matters more here than there: a run is synchronous with a
	// ten-minute bound, so a refusal that produced a Job anyway would be reported to the caller as a
	// timeout ten minutes later instead of as the reason it did not start.
	if err := a.applyAppPodMutator(PodIdentity{App: spec.App, Namespace: a.namespace, Workload: PodWorkloadRun},
		&job.Spec.Template.Spec); err != nil {
		return nil, err
	}
	return job, nil
}
