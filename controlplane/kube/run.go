// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
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
	// runJobTimeout caps how long burrowd waits for a one-off command Job to finish (ADR-0048 §3).
	runJobTimeout = 10 * time.Minute
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
// launch, poll, or timeout failure. The finished Job is garbage-collected by Kubernetes' native
// ttlSecondsAfterFinished (ADR-0048 §7), set from spec.TTLSeconds — no imperative reap.
func (a *Adapter) RunJob(ctx context.Context, spec controlplane.RunSpec) (controlplane.RunResult, error) {
	if err := validateAppIdentifier(spec.App); err != nil {
		return controlplane.RunResult{}, err
	}
	name := fmt.Sprintf("burrow-run-%s", spec.ID)
	job := a.runJob(name, spec)
	jobs := a.client.BatchV1().Jobs(a.namespace)
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return controlplane.RunResult{}, fmt.Errorf("kube: creating run job %q: %w", name, err)
	}

	deadline := time.Now().Add(runJobTimeout)
	for {
		j, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return controlplane.RunResult{}, fmt.Errorf("kube: reading run job %q: %w", name, err)
		}
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
			// Terminal (Complete or Failed): capture the pod's output and exit code before the TTL
			// controller garbage-collects it. Both terminal states carry the same answer (ADR-0048 §3).
			return a.captureRun(ctx, name), nil
		}
		if time.Now().After(deadline) {
			return controlplane.RunResult{TimedOut: true}, fmt.Errorf("kube: run job %q did not complete within %s", name, runJobTimeout)
		}
		select {
		case <-ctx.Done():
			return controlplane.RunResult{}, ctx.Err()
		case <-time.After(runJobPoll):
		}
	}
}

// captureRun reads a finished run Job's pod for the container's exit code and its combined log output
// (ADR-0048 §3, §6). Kubernetes' pod-log API returns stdout and stderr as one interleaved stream, so
// the captured text lands in Stdout; Stderr is reserved for a future separation. Best-effort: a
// missing pod or an unreadable log yields whatever was captured, never an error — the Job already
// reached a terminal state, which is the answer the caller returns.
func (a *Adapter) captureRun(ctx context.Context, jobName string) controlplane.RunResult {
	var res controlplane.RunResult
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + jobName})
	if err != nil {
		return res
	}
	for _, pod := range pods.Items {
		var terminated *corev1.ContainerStateTerminated
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil {
				terminated = cs.State.Terminated
				break
			}
		}
		if terminated == nil {
			continue // a pod that never ran to a terminal state carries no exit code
		}
		res.ExitCode = int(terminated.ExitCode)
		if stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx); err == nil {
			data, _ := io.ReadAll(stream)
			stream.Close()
			res.Stdout = string(data)
		}
		return res
	}
	return res
}

// runJob builds the one-shot Job for a run: the app's own current image and command, its config env,
// and its per-app Secret via envFrom (ADR-0048 §2), in the adapter's app namespace. RestartPolicy
// Never and BackoffLimit 0 make a single attempt whose exit code is the result — no retry masking a
// non-zero exit. TTLSecondsAfterFinished sets the native garbage-collection window (ADR-0048 §7).
func (a *Adapter) runJob(name string, spec controlplane.RunSpec) *batchv1.Job {
	labels := map[string]string{nameLabel: name, managedByLabel: managedByValue}
	var backoff int32
	ttl := spec.TTLSeconds

	var env []corev1.EnvVar
	for _, k := range sortedKeys(spec.Env) { // deterministic order
		env = append(env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
	}
	// Source the app's per-app Secret as env exactly as the running workload does (ADR-0048 §2), so
	// DATABASE_URL and every secret resolve as the app sees them. Optional: the Secret may not exist.
	envFrom := []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: controlplane.AppSecretName(spec.App)},
			Optional:             boolPtr(true),
		},
	}}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    runContainerName,
						Image:   spec.Image,
						Command: spec.Command,
						Env:     env,
						EnvFrom: envFrom,
					}},
				},
			},
		},
	}
}
