// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// TestRunJobSpecAndCapture asserts RunJob builds a one-shot Job in the app namespace from the app's
// own image and command, injects the app's config env and per-app Secret via envFrom, sets the
// requested ttlSecondsAfterFinished with BackoffLimit 0 and RestartPolicy Never, and captures the
// finished pod's exit code — surfacing a non-zero exit as a structured result, not an error
// (ADR-0048 §2, §3, §7).
func TestRunJobSpecAndCapture(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	var created []*batchv1.Job
	client.PrependReactor("create", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		created = append(created, a.(clienttesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy())
		return false, nil, nil // let the tracker store it too
	})
	// The Job is observed Failed (a non-zero exit fails the BackoffLimit-0 Job); RunJob must still
	// capture the exit code and return a result rather than an error.
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	})

	// A finished pod for the run Job, labelled like the Job so the capture finds it by nameLabel.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "burrow-run-r1-xyz", Namespace: "apps",
			Labels: map[string]string{nameLabel: "burrow-run-r1"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  runContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 5}},
			}},
		},
	}
	if _, err := client.CoreV1().Pods("apps").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	a := New(client, "apps")
	res, err := a.RunJob(ctx, controlplane.RunSpec{
		App:        "shop",
		ID:         "r1",
		Image:      "busybox:1.36",
		Command:    []string{"sh", "-c", "echo hi"},
		Env:        map[string]string{"FOO": "bar"},
		TTLSeconds: 42,
	})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if res.ExitCode != 5 {
		t.Errorf("exit = %d, want 5 (captured from the terminated pod)", res.ExitCode)
	}

	if len(created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(created))
	}
	job := created[0]
	if job.Name != "burrow-run-r1" || job.Namespace != "apps" {
		t.Errorf("job = %s/%s, want apps/burrow-run-r1", job.Namespace, job.Name)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 42 {
		t.Errorf("ttlSecondsAfterFinished = %v, want 42", job.Spec.TTLSecondsAfterFinished)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}

	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "busybox:1.36" {
		t.Errorf("image = %q, want busybox:1.36", c.Image)
	}
	// The app's per-app Secret is injected via envFrom so DATABASE_URL and every secret appear as the
	// running app sees them (ADR-0048 §2).
	var gotSecretRef bool
	for _, ef := range c.EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == controlplane.AppSecretName("shop") {
			gotSecretRef = true
		}
	}
	if !gotSecretRef {
		t.Errorf("run Job does not envFrom the app's per-app Secret %q", controlplane.AppSecretName("shop"))
	}
	// Non-secret config is rendered as container env too.
	var gotFoo bool
	for _, e := range c.Env {
		if e.Name == "FOO" && e.Value == "bar" {
			gotFoo = true
		}
	}
	if !gotFoo {
		t.Errorf("config env FOO=bar not rendered: %+v", c.Env)
	}
}

// TestRunJobRejectsBadApp asserts a bad app identifier is rejected before any Job is built.
func TestRunJobRejectsBadApp(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps")
	if _, err := a.RunJob(ctx, controlplane.RunSpec{App: "Bad_Name", ID: "r1", Command: []string{"echo"}}); err == nil {
		t.Error("RunJob should reject a bad app identifier")
	}
	jobs, _ := client.BatchV1().Jobs("apps").List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("no Job should be created for a bad app, got %d", len(jobs.Items))
	}
}
