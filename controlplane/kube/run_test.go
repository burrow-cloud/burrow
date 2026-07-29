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

// runJobFor drives RunJob to completion against a fake clientset and returns the Job the adapter
// actually sent to the API server. The Job is reported terminal on the first read so RunJob returns
// immediately; no pod is seeded, so the capture is empty — this helper is about the authored object.
func runJobFor(t *testing.T, mutator func(*corev1.PodSpec)) *batchv1.Job {
	t.Helper()
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	var created *batchv1.Job
	client.PrependReactor("create", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		created = a.(clienttesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		return false, nil, nil
	})
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})

	adapter := New(client, "apps")
	if mutator != nil {
		adapter = adapter.WithPodMutator(mutator)
	}
	if _, err := adapter.RunJob(ctx, controlplane.RunSpec{
		App: "shop", ID: "r1", Image: "shop:v9",
		Command: []string{"./migrate"}, TTLSeconds: 60,
	}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if created == nil {
		t.Fatal("RunJob created no Job")
	}
	return created
}

// TestRunJobPodMutatorApplied is the guarantee that a one-off command lands under the same placement
// and runtime policy as the app it belongs to. `burrow app run` executes the app's own image, in the
// app's namespace, with the app's environment, so on a cluster whose app pods only schedule with a
// toleration — or whose workloads must name a sandboxed runtimeClassName — the run Job needs the
// identical fields. Without the hook the Job is authored bare and the failure is quiet: the pod sits
// Pending until RunJob's ten-minute deadline and reports a timeout, not an unschedulable pod
// (ADR-0061, ADR-0048 §3).
func TestRunJobPodMutatorApplied(t *testing.T) {
	tol := corev1.Toleration{
		Key: "gpu", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule,
	}
	job := runJobFor(t, func(pod *corev1.PodSpec) {
		pod.RuntimeClassName = ptrTo("kata")
		pod.Tolerations = []corev1.Toleration{tol}
		pod.NodeSelector = map[string]string{"pool": "sandboxed"}
	})

	pod := job.Spec.Template.Spec
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "kata" {
		t.Errorf("runtimeClassName = %v, want kata — the run Job runs unsandboxed on a cluster that mandates one", pod.RuntimeClassName)
	}
	if len(pod.Tolerations) != 1 || pod.Tolerations[0] != tol {
		t.Errorf("tolerations = %+v, want exactly %+v", pod.Tolerations, tol)
	}
	if pod.NodeSelector["pool"] != "sandboxed" {
		t.Errorf("nodeSelector = %v, want pool=sandboxed", pod.NodeSelector)
	}
	// The hook adjusts the pod for the cluster; it does not rewrite what the run path depends on.
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want Never", pod.RestartPolicy)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Image != "shop:v9" {
		t.Errorf("containers = %+v, want the app's own image", pod.Containers)
	}
}

// TestRunJobNoPodMutatorLeavesDefault is ADR-0061 §3's obligation on the run path: with no mutator
// wired, the Job is exactly what it was before the seam reached here — no runtime class, no
// toleration, no node selector — so an install that wires nothing sees no change at all.
func TestRunJobNoPodMutatorLeavesDefault(t *testing.T) {
	pod := runJobFor(t, nil).Spec.Template.Spec

	if pod.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %q, want unset with no mutator", *pod.RuntimeClassName)
	}
	if len(pod.Tolerations) != 0 {
		t.Errorf("tolerations = %+v, want none with no mutator", pod.Tolerations)
	}
	if len(pod.NodeSelector) != 0 {
		t.Errorf("nodeSelector = %v, want none with no mutator", pod.NodeSelector)
	}
}

// TestRunJobPodMutatorSeesConstructedPodSpec pins the ordering: the hook runs over the FULLY-built
// pod spec, so a mutator can key its decision off the containers, image, and env the engine composed
// — the same contract the deploy path's hook has. A hook applied to a half-built spec would see an
// empty container list and silently make the wrong choice.
func TestRunJobPodMutatorSeesConstructedPodSpec(t *testing.T) {
	var sawContainers int
	var sawImage string
	var sawSecretRef bool
	runJobFor(t, func(pod *corev1.PodSpec) {
		sawContainers = len(pod.Containers)
		if len(pod.Containers) > 0 {
			sawImage = pod.Containers[0].Image
			for _, ef := range pod.Containers[0].EnvFrom {
				if ef.SecretRef != nil && ef.SecretRef.Name == controlplane.AppSecretName("shop") {
					sawSecretRef = true
				}
			}
		}
	})

	if sawContainers != 1 {
		t.Errorf("mutator saw %d containers, want 1 — it ran before the pod was built", sawContainers)
	}
	if sawImage != "shop:v9" {
		t.Errorf("mutator saw image %q, want shop:v9", sawImage)
	}
	if !sawSecretRef {
		t.Error("mutator did not see the app's per-app Secret envFrom source")
	}
}

func ptrTo[T any](v T) *T { return &v }
