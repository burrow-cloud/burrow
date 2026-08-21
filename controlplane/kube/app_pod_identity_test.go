// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The app hook's signature is pinned for the same reason placement_test.go pins the other two: a
// convenience change that quietly reshapes a public seam is a source break for an embedder who finds
// out at compile time in their own repository rather than in this one.
//
// THIS LINE WAS EDITED DELIBERATELY when the hook grew its error return, and that is worth saying
// out loud, because a pin edited quietly is a pin defeated. The two lines in placement_test.go were
// NOT edited: WithPodMutator and WithPlatformPodMutator keep their exact types, so an install wired
// through either needs no change. What moved is the spelling released in v0.14.0-rc.53 and adopted
// by nobody — see the record for why that is a break worth taking rather than a fourth name for one
// mechanism.
var _ func(*Adapter, func(PodIdentity, *corev1.PodSpec) error) *Adapter = (*Adapter).WithAppPodMutator

// recordIdentities returns a mutator that appends every identity it is handed, and the slice it
// appends to. The pod spec is left alone: these tests are about what the hook is TOLD, not what it
// does with it.
func recordIdentities(seen *[]PodIdentity) func(PodIdentity, *corev1.PodSpec) error {
	return func(id PodIdentity, _ *corev1.PodSpec) error { *seen = append(*seen, id); return nil }
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
func runJobWithIdentityHook(t *testing.T, ns, app string, fn func(PodIdentity, *corev1.PodSpec) error) *batchv1.Job {
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
	job := runJobWithIdentityHook(t, "apps", "shop", func(id PodIdentity, pod *corev1.PodSpec) error {
		seen = append(seen, id)
		pod.RuntimeClassName = ptrTo("kata")
		return nil
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
	a := New(client, "apps").WithAppPodMutator(func(id PodIdentity, pod *corev1.PodSpec) error {
		if id.App == "untrusted" {
			pod.RuntimeClassName = ptrTo("kata")
		}
		return nil
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
		WithAppPodMutator(func(PodIdentity, *corev1.PodSpec) error { identityCalls++; return nil }).
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
		WithAppPodMutator(func(PodIdentity, *corev1.PodSpec) error { t.Error("the replaced hook must not run"); return nil }).
		WithPodMutator(nil)
	if a.podMutator != nil {
		t.Fatal("WithPodMutator(nil) left a hook wired")
	}
	applyWorkloadFor(t, a, "shop") // would panic if nil were wrapped rather than cleared
}

// errRefused is what a hook that cannot decide returns. The tests below require the caller to be
// able to recover it with errors.Is, because an embedder's own error is the whole reason this
// return value exists: a refusal reported as an opaque string would leave them unable to tell their
// own failure from one of Burrow's.
var errRefused = errors.New("the app's runtime could not be read")

// refuse is a hook that shapes nothing and refuses everything. It also records whether it was asked,
// so a test can tell "the hook refused" from "the hook was never called".
func refuse(calls *int) func(PodIdentity, *corev1.PodSpec) error {
	return func(PodIdentity, *corev1.PodSpec) error {
		*calls++
		return errRefused
	}
}

// refusalClient is a fake cluster that REFUSES to create a run Job, so a test asserting "this run
// never started" fails fast rather than hanging. Without it a regression that let a refused run reach
// the API server would leave RunJob waiting out its ten-minute deadline, and a guard whose failure
// mode is a ten-minute hang is one somebody will delete rather than fix.
func refusalClient(t *testing.T) *fake.Clientset {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		t.Error("a run Job was created despite the mutator refusing")
		return true, nil, errors.New("this Job must never have been created")
	})
	return client
}

// TestARefusingAppPodMutatorFailsTheDeployAndWritesNothing is the reason the hook returns an error.
//
// A hook carrying per-app policy reads that policy from somewhere, and the read can fail. The
// outcomes available without an error return are to guess — running the pod under a policy nobody
// chose — or to poison the spec so the API server rejects it, which reports the refusal as whatever
// validation error the poison happens to trigger. This asserts the third outcome: the deploy fails,
// the hook's own error is recoverable from the one the caller gets, and NO Deployment exists.
func TestARefusingAppPodMutatorFailsTheDeployAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	calls := 0
	a := New(client, "apps").WithAppPodMutator(refuse(&calls))

	err := a.ApplyWorkload(ctx, controlplane.WorkloadSpec{App: "shop", Image: "shop:v1", Replicas: 1})
	if err == nil {
		t.Fatal("ApplyWorkload succeeded with a refusing mutator; a pod nobody could shape must not be written")
	}
	if !errors.Is(err, errRefused) {
		t.Errorf("ApplyWorkload error = %v, want the hook's own error to survive: an embedder cannot act on a refusal it cannot recognise", err)
	}
	if calls != 1 {
		t.Errorf("the hook was called %d times, want exactly 1: a refusal is not a conflict and must not be retried", calls)
	}
	if _, err := client.AppsV1().Deployments("apps").Get(ctx, "shop", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a Deployment exists after a refused mutator (get returned %v); the object must never reach the API server", err)
	}
}

// TestARefusingAppPodMutatorFailsAnUpdateWithoutTouchingTheLiveObject is the redeploy half. It
// matters more than the create: the app is already serving, and a refusal that half-applied would
// replace a working pod template with one shaped by a policy that could not be read.
func TestARefusingAppPodMutatorFailsAnUpdateWithoutTouchingTheLiveObject(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	spec := controlplane.WorkloadSpec{App: "shop", Image: "shop:v1", Replicas: 1}

	shaped := New(client, "apps").WithAppPodMutator(func(_ PodIdentity, pod *corev1.PodSpec) error {
		pod.RuntimeClassName = ptrTo("runsc")
		return nil
	})
	if err := shaped.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload (create): %v", err)
	}

	calls := 0
	spec.Image = "shop:v2"
	if err := New(client, "apps").WithAppPodMutator(refuse(&calls)).ApplyWorkload(ctx, spec); err == nil {
		t.Fatal("the redeploy succeeded with a refusing mutator")
	}
	if calls != 1 {
		t.Errorf("the hook was called %d times on the update path, want exactly 1", calls)
	}

	dep, err := client.AppsV1().Deployments("apps").Get(ctx, "shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "shop:v1" {
		t.Errorf("the live Deployment moved to %q; a refused redeploy must leave the serving object exactly as it was", got)
	}
	if got := dep.Spec.Template.Spec.RuntimeClassName; got == nil || *got != "runsc" {
		t.Errorf("the live pod template's RuntimeClassName = %v, want it untouched at \"runsc\"", got)
	}
}

// TestARefusingAppPodMutatorFailsARunAndCreatesNoJob is the other app-image path. The refusal has to
// arrive here as an error rather than as a Job nobody can schedule: RunJob is synchronous with a
// ten-minute bound, so a Job created anyway would report as a timeout ten minutes later instead of
// as the reason it never started.
func TestARefusingAppPodMutatorFailsARunAndCreatesNoJob(t *testing.T) {
	ctx := context.Background()
	client := refusalClient(t)
	calls := 0

	_, err := New(client, "apps").WithAppPodMutator(refuse(&calls)).RunJob(ctx, controlplane.RunSpec{
		App: "shop", ID: "r1", Image: "shop:v1", Command: []string{"echo", "hi"},
	})
	if err == nil {
		t.Fatal("RunJob succeeded with a refusing mutator")
	}
	if !errors.Is(err, errRefused) {
		t.Errorf("RunJob error = %v, want the hook's own error to survive", err)
	}
	if calls != 1 {
		t.Errorf("the hook was called %d times, want exactly 1", calls)
	}
}

// TestTheRefusalNamesTheAppAndWhichPodItWas pins what the adapter adds to the hook's own message. A
// hook writes its error without knowing which of an app's two pods it was applied to, and "the
// runtime could not be read" means something different on a redeploy than on a one-off command.
func TestTheRefusalNamesTheAppAndWhichPodItWas(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(a *Adapter) error
		want string
	}{
		{"deploy", func(a *Adapter) error {
			return a.ApplyWorkload(ctx, controlplane.WorkloadSpec{App: "shop", Image: "shop:v1", Replicas: 1})
		}, string(PodWorkloadDeployment)},
		{"run", func(a *Adapter) error {
			_, err := a.RunJob(ctx, controlplane.RunSpec{App: "shop", ID: "r1", Image: "shop:v1", Command: []string{"echo"}})
			return err
		}, string(PodWorkloadRun)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := tc.run(New(refusalClient(t), "apps").WithAppPodMutator(refuse(&calls)))
			if err == nil {
				t.Fatal("the operation succeeded with a refusing mutator")
			}
			for _, want := range []string{"shop", "apps", tc.want, errRefused.Error()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestAMutatorThatReturnsNilAuthorsWhatItAlwaysDid keeps ADR-0061 §3 standing across the widening: a
// hook that shapes the pod and reports no error produces the same object as before the error return
// existed, and an adapter with no hook at all is untouched.
func TestAMutatorThatReturnsNilAuthorsWhatItAlwaysDid(t *testing.T) {
	ctx := context.Background()
	spec := controlplane.WorkloadSpec{App: "shop", Image: "shop:v1", Replicas: 1}

	bare := fake.NewSimpleClientset()
	if err := New(bare, "apps").ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload (no hook): %v", err)
	}
	unhooked, err := bare.AppsV1().Deployments("apps").Get(ctx, "shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if unhooked.Spec.Template.Spec.NodeSelector != nil {
		t.Errorf("an adapter with no hook shaped the pod: %+v", unhooked.Spec.Template.Spec.NodeSelector)
	}

	hooked := fake.NewSimpleClientset()
	shape := func(pod *corev1.PodSpec) { pod.NodeSelector = map[string]string{"pool": "tenant"} }
	// The two spellings of the one hook must produce the SAME pod, which is what keeps ADR-0100 §3's
	// "one mechanism, two names" true now that only one of them can refuse.
	viaPlain := fake.NewSimpleClientset()
	if err := New(hooked, "apps").WithAppPodMutator(func(_ PodIdentity, pod *corev1.PodSpec) error {
		shape(pod)
		return nil
	}).ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload (identity hook): %v", err)
	}
	if err := New(viaPlain, "apps").WithPodMutator(shape).ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload (plain hook): %v", err)
	}
	a, err := hooked.AppsV1().Deployments("apps").Get(ctx, "shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, err := viaPlain.AppsV1().Deployments("apps").Get(ctx, "shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !apiequality.Semantic.DeepEqual(a.Spec.Template.Spec, b.Spec.Template.Spec) {
		t.Errorf("the two spellings authored different pods:\nWithAppPodMutator: %+v\nWithPodMutator:    %+v",
			a.Spec.Template.Spec, b.Spec.Template.Spec)
	}
}
