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

// The widened app hook keeps ITS exact signature too, for the same reason placement_test.go pins the
// other two: a convenience change that quietly reshapes a public seam is a source break for an
// embedder who finds out at compile time in their own repository rather than in this one.
var _ func(*Adapter, func(PodIdentity, *corev1.PodSpec)) *Adapter = (*Adapter).WithAppPodMutator

// recordIdentities returns a mutator that appends every identity it is handed, and the slice it
// appends to. The pod spec is left alone: these tests are about what the hook is TOLD, not what it
// does with it.
func recordIdentities(seen *[]PodIdentity) func(PodIdentity, *corev1.PodSpec) {
	return func(id PodIdentity, _ *corev1.PodSpec) { *seen = append(*seen, id) }
}

// applyWorkloadFor drives ApplyWorkload against a fake clientset for one app, so the identity under
// test is the one produced on the real create path rather than one read off an internal builder.
func applyWorkloadFor(t *testing.T, a controlplane.Kubernetes, app string) {
	t.Helper()
	if err := a.ApplyWorkload(context.Background(), controlplane.WorkloadSpec{
		App: app, Image: app + ":v1", Replicas: 1,
	}); err != nil {
		t.Fatalf("ApplyWorkload(%s): %v", app, err)
	}
}

// runJobWithIdentityHook drives RunJob to completion against a fake clientset with the widened hook
// wired, and returns the Job the adapter sent to the API server. The Job reads back terminal on the
// first get so RunJob returns immediately; no pod is seeded, so nothing is captured — this is about
// the authored object and the identity that accompanied it.
func runJobWithIdentityHook(t *testing.T, ns, app string, fn func(PodIdentity, *corev1.PodSpec)) *batchv1.Job {
	t.Helper()
	client := fake.NewSimpleClientset()

	var created *batchv1.Job
	client.PrependReactor("create", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		created = a.(clienttesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		return false, nil, nil
	})
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})

	if _, err := New(client, ns).WithAppPodMutator(fn).RunJob(context.Background(), controlplane.RunSpec{
		App: app, ID: "r1", Image: app + ":v1", Command: []string{"./migrate"}, TTLSeconds: 60,
	}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if created == nil {
		t.Fatal("RunJob created no Job")
	}
	return created
}

// TestAppPodMutatorIsToldWhichAppTheDeploymentIsFor is the whole point of the widened hook. An
// identity-free mutator can only carry policy that is true of EVERY app on the cluster; a per-app
// runtime class, a per-app node pool, a per-app pull secret are all unsayable through it. The hook
// is handed the app the Deployment is for and the namespace it is being written into, both of which
// the deploy path already had in hand.
func TestAppPodMutatorIsToldWhichAppTheDeploymentIsFor(t *testing.T) {
	var seen []PodIdentity
	a := New(fake.NewSimpleClientset(), "apps").WithAppPodMutator(recordIdentities(&seen))
	applyWorkloadFor(t, a, "shop")

	want := PodIdentity{App: "shop", Namespace: "apps", Workload: PodWorkloadDeployment}
	if len(seen) != 1 || seen[0] != want {
		t.Errorf("hook saw %+v, want exactly [%+v]", seen, want)
	}
}

// TestAppPodMutatorIsToldTheSameAppForARun pins that a one-off command carries the SAME identity as
// the deploy it runs beside. `burrow app run` executes the app's own image, in the app's namespace,
// with the app's environment, so a per-app policy has to reach it without the wiring author
// restating it — and Workload still distinguishes the two, because a run pod is a Job pod that
// arrives with RestartPolicy Never already set.
func TestAppPodMutatorIsToldTheSameAppForARun(t *testing.T) {
	var seen []PodIdentity
	job := runJobWithIdentityHook(t, "apps", "shop", func(id PodIdentity, pod *corev1.PodSpec) {
		seen = append(seen, id)
		pod.RuntimeClassName = ptrTo("kata")
	})

	want := PodIdentity{App: "shop", Namespace: "apps", Workload: PodWorkloadRun}
	if len(seen) != 1 || seen[0] != want {
		t.Errorf("hook saw %+v, want exactly [%+v]", seen, want)
	}
	pod := job.Spec.Template.Spec
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "kata" {
		t.Errorf("runtimeClassName = %v, want kata — the widened hook must still reach the pod it names", pod.RuntimeClassName)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never — the hook adjusts the pod, it does not rewrite what the run path depends on", pod.RestartPolicy)
	}
}

// TestAppPodIdentityReportsTheEnvironmentNamespace is why the identity says namespace rather than
// environment. An environment-scoped view is a copy of the adapter with a different namespace
// (ADR-0035 phase 2), and the seam carries a Kubernetes fact — the namespace the object is actually
// written into — rather than a name whose meaning belongs to whoever wired the hook. A hook told
// "apps" for a pod landing in "shop-staging" would apply the wrong policy and look correct.
func TestAppPodIdentityReportsTheEnvironmentNamespace(t *testing.T) {
	var seen []PodIdentity
	a := New(fake.NewSimpleClientset(), "apps").WithAppPodMutator(recordIdentities(&seen))
	applyWorkloadFor(t, a.WithNamespace("shop-staging"), "shop")

	want := PodIdentity{App: "shop", Namespace: "shop-staging", Workload: PodWorkloadDeployment}
	if len(seen) != 1 || seen[0] != want {
		t.Errorf("hook saw %+v, want exactly [%+v]; a seam wired once must follow the app into a named environment", seen, want)
	}
}

// TestAppPodMutatorCarriesPerAppPolicy is the behavioural half: the same wired hook applies
// DIFFERENT policy to two apps in the same cluster. This is the case an identity-free hook cannot
// serve except by keying off a container image or a label to reconstruct a classification the engine
// already had — the shape ADR-0073 §2 rejects for the platform split, where a wrong branch puts the
// wrong policy on the wrong workload.
func TestAppPodMutatorCarriesPerAppPolicy(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAppPodMutator(func(id PodIdentity, pod *corev1.PodSpec) {
		if id.App == "untrusted" {
			pod.RuntimeClassName = ptrTo("kata")
		}
	})
	applyWorkloadFor(t, a, "untrusted")
	applyWorkloadFor(t, a, "trusted")

	for _, tc := range []struct {
		app  string
		want *string
	}{
		{"untrusted", ptrTo("kata")},
		{"trusted", nil},
	} {
		dep, err := client.AppsV1().Deployments("apps").Get(ctx, tc.app, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get Deployment %s: %v", tc.app, err)
		}
		got := dep.Spec.Template.Spec.RuntimeClassName
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s ran under runtimeClass %q, want none — policy leaked onto an app it was not written for", tc.app, *got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("%s runtimeClassName = %v, want %q", tc.app, got, *tc.want)
		}
	}
}

// TestWithPodMutatorStillWiresTheSameSingleHook is the compatibility guarantee. The older spelling is
// a public seam an embedder may already be compiled against, so it keeps working unchanged — and it
// wires the SAME field, so the two spellings do not stack. Wiring one after the other replaces,
// which is what wiring any hook twice has always done; two hooks running over one pod would be a new
// behaviour nobody asked for and an idempotency trap on top of ADR-0061's existing one.
func TestWithPodMutatorStillWiresTheSameSingleHook(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	var identityCalls, plainCalls int
	a := New(client, "apps").
		WithAppPodMutator(func(PodIdentity, *corev1.PodSpec) { identityCalls++ }).
		WithPodMutator(func(pod *corev1.PodSpec) {
			plainCalls++
			pod.NodeSelector = map[string]string{"pool": "tenant"}
		})
	applyWorkloadFor(t, a, "shop")

	if plainCalls != 1 {
		t.Errorf("the WithPodMutator hook ran %d times, want 1 — the older spelling must keep working", plainCalls)
	}
	if identityCalls != 0 {
		t.Errorf("the replaced hook ran %d times, want 0 — the two spellings wire one hook, they do not stack", identityCalls)
	}
	dep, err := client.AppsV1().Deployments("apps").Get(ctx, "shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if dep.Spec.Template.Spec.NodeSelector["pool"] != "tenant" {
		t.Errorf("nodeSelector = %v, want pool=tenant", dep.Spec.Template.Spec.NodeSelector)
	}
}

// TestWithPodMutatorNilLeavesNoHookWired keeps the older spelling's nil case exactly as it was. It
// used to assign fn straight onto the field, so passing nil meant "no hook"; wrapping it in a
// closure would turn that into a hook that runs and calls a nil function, which panics on the first
// deploy — the kind of regression a compatibility shim is supposed to make impossible.
func TestWithPodMutatorNilLeavesNoHookWired(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := New(client, "apps").
		WithAppPodMutator(func(PodIdentity, *corev1.PodSpec) { t.Error("the replaced hook must not run") }).
		WithPodMutator(nil)
	if a.podMutator != nil {
		t.Fatal("WithPodMutator(nil) left a hook wired")
	}
	applyWorkloadFor(t, a, "shop") // would panic if nil were wrapped rather than cleared
}
