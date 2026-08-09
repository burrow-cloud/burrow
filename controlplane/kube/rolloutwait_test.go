// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// These tests carry the same assertion jobwait_test.go's do, for the same reason: the defect a
// settle-wait can have is not returning the wrong answer, it is NOT RETURNING — burning its whole
// bound and then reporting elapsed time (issue #352, ADR-0072 §5). So every case here is bounded by
// mustSettleWithin, and a regression to "poll and hope" shows up as a stall.

// settleFailFast is the budget for a wait that must reach its verdict on the first pass. It is far
// shorter than any bound a test passes in and comfortably longer than a fake clientset needs.
const settleFailFast = 15 * time.Second

// rolloutNS is the namespace these waits act in.
const rolloutNS = "default"

// mustSettleWithin runs AwaitRollout and returns its outcome, failing the test if it has not
// returned within settleFailFast.
func mustSettleWithin(t *testing.T, a *Adapter, app string, timeout time.Duration) controlplane.RolloutOutcome {
	t.Helper()
	type result struct {
		out controlplane.RolloutOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := a.AwaitRollout(context.Background(), app, timeout)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("AwaitRollout: %v", r.err)
		}
		return r.out
	case <-time.After(settleFailFast):
		t.Fatalf("AwaitRollout did not return within %s: a settle wait that cannot reach a verdict from what the cluster is already reporting has converted a diagnosis into a shrug (ADR-0072 §5)", settleFailFast)
		return controlplane.RolloutOutcome{}
	}
}

// seedDeployment creates a Deployment with the given status, so a test can pose the cluster state a
// wait has to reach a verdict from.
func seedDeployment(t *testing.T, client *fake.Clientset, namespace, app string, replicas int32, status appsv1.DeploymentStatus) {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: namespace, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "img:1"}}}},
		},
		Status: status,
	}
	if _, err := client.AppsV1().Deployments(namespace).Create(context.Background(), dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
}

// seedAppPod creates a pod labelled the way the app's workload labels its pods, so the wait's
// selector finds it — the same selector the status surface uses.
func seedAppPod(t *testing.T, client *fake.Clientset, namespace, app, name string, status corev1.PodStatus) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{nameLabel: app}},
		Status:     status,
	}
	if _, err := client.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
}

// TestAwaitRolloutSettlesOnACompletedRollout asserts the completion test is the one
// `kubectl rollout status` uses — the new revision is the only one left and it is available — and
// not merely "some pods are ready", which the OLD ReplicaSet satisfies throughout a rolling update.
func TestAwaitRolloutSettlesOnACompletedRollout(t *testing.T) {
	client := fake.NewSimpleClientset()
	seedDeployment(t, client, rolloutNS, "web", 2, appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2,
	})
	out := mustSettleWithin(t, New(client, rolloutNS), "web", time.Minute)
	if !out.Settled {
		t.Fatalf("outcome = %+v, want settled", out)
	}
	if out.Reason != "" {
		t.Errorf("reason = %q on a settled rollout, want empty", out.Reason)
	}
	if out.Outcome() != controlplane.OutcomeSucceeded {
		t.Errorf("Outcome() = %q, want %q", out.Outcome(), controlplane.OutcomeSucceeded)
	}
}

// TestAwaitRolloutSettlesDespiteARecentRestart guards the boundary issue #416 draws. The status
// surface now attaches an Issue to a workload that is SERVING but whose container was killed and
// came back — and rolloutVerdict treats any IssueReason on an unfinished rollout as the reason it is
// wedged. A survived kill is not that: it is a fact about the app, not a verdict on this release, and
// a deploy must not be reported failed because the previous pod had been OOM-killed a minute ago.
//
// The completion test running FIRST is what keeps the two apart, and this is the test that says so.
func TestAwaitRolloutSettlesDespiteARecentRestart(t *testing.T) {
	client := fake.NewSimpleClientset()
	seedDeployment(t, client, rolloutNS, "web", 1, appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
	})
	at := metav1.NewTime(time.Now().Add(-time.Minute))
	seedAppPod(t, client, rolloutNS, "web", "web-restarted", corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Ready: true, RestartCount: 3,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: controlplane.ReasonOOMKilled, ExitCode: 137, FinishedAt: at,
			}},
		}},
	})

	out := mustSettleWithin(t, New(client, rolloutNS), "web", time.Minute)
	if !out.Settled || out.Reason != "" {
		t.Fatalf("outcome = %+v, want settled with no reason: the release rolled out, and a kill it survived is not a failed deploy", out)
	}
}

// TestAwaitRolloutFailsFastOnACrashLoop is the case the post phase exists for. The old ReplicaSet is
// still serving, so a naive readiness check would call this healthy; the wait must instead return
// the reason the status surface reports for the same pod, and return it immediately rather than at
// the end of its bound.
func TestAwaitRolloutFailsFastOnACrashLoop(t *testing.T) {
	client := fake.NewSimpleClientset()
	seedDeployment(t, client, rolloutNS, "web", 2, appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 3, UpdatedReplicas: 1, ReadyReplicas: 2, AvailableReplicas: 2,
	})
	seedAppPod(t, client, rolloutNS, "web", "web-new", corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: controlplane.ReasonCrashLoopBackOff}},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
			},
		}},
	})

	// An hour, so a wait that did not fail fast could not possibly finish inside the budget.
	out := mustSettleWithin(t, New(client, rolloutNS), "web", time.Hour)
	if out.Settled {
		t.Fatalf("outcome = %+v, want a failure: the new revision is crash-looping", out)
	}
	if out.Reason != controlplane.ReasonCrashLoopBackOff {
		t.Errorf("reason = %q, want %q", out.Reason, controlplane.ReasonCrashLoopBackOff)
	}
	if !controlplane.IsLedgerReason(out.Reason) {
		t.Errorf("reason %q is outside ADR-0074's closed vocabulary", out.Reason)
	}
	// The detail is Burrow's own summary of the rollout, never the application's log tail: this value
	// is copied into the hook Job's environment, where it would persist in a Kubernetes object.
	if !strings.Contains(out.Detail, "replicas") {
		t.Errorf("detail = %q, want Burrow's own replica summary", out.Detail)
	}
}

// TestAwaitRolloutReportsWhatItObservedWhenTheBoundExpires is ADR-0072 §5. Nothing blocking is
// reported — the pod is simply still creating its container, which the Issue criterion deliberately
// excludes because it resolves on its own — so the wait reaches the BACKSTOP reason, and its detail
// has to name what was seen rather than only how long it waited.
func TestAwaitRolloutReportsWhatItObservedWhenTheBoundExpires(t *testing.T) {
	client := fake.NewSimpleClientset()
	seedDeployment(t, client, rolloutNS, "web", 1, appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 0, AvailableReplicas: 0,
	})
	seedAppPod(t, client, rolloutNS, "web", "web-new", corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}},
	})

	out := mustSettleWithin(t, New(client, rolloutNS), "web", time.Millisecond)
	if out.Settled {
		t.Fatalf("outcome = %+v, want a failure", out)
	}
	if out.Reason != controlplane.ReasonDeadlineExceeded {
		t.Errorf("reason = %q, want the backstop %q", out.Reason, controlplane.ReasonDeadlineExceeded)
	}
	for _, want := range []string{"web-new", "Pending", "ContainerCreating"} {
		if !strings.Contains(out.Detail, want) {
			t.Errorf("detail = %q, want it to name %q — an elapsed time alone is a shrug (ADR-0072 §5)", out.Detail, want)
		}
	}
}

// TestAwaitRolloutReportsAReadinessProbeThatNeverPasses is what ADR-0076 changed about the meaning
// of "succeeded". An app that boots but cannot serve reports NO blocking pod condition — the
// container is running, it is simply never ready — so before readiness probes existed this rollout
// settled as a success. With a probe the pod never becomes available, the rollout stalls, and
// Kubernetes marks the Deployment's Progressing condition False with ProgressDeadlineExceeded, which
// is already a member of the closed set. The post hook is told that, not a coined new name.
func TestAwaitRolloutReportsAReadinessProbeThatNeverPasses(t *testing.T) {
	client := fake.NewSimpleClientset()
	seedDeployment(t, client, rolloutNS, "web", 1, appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		Conditions: []appsv1.DeploymentCondition{{
			Type:   appsv1.DeploymentProgressing,
			Status: corev1.ConditionFalse,
			Reason: controlplane.ReasonProgressDeadlineExceeded,
		}},
	})
	seedAppPod(t, client, rolloutNS, "web", "web-new", corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: false, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
	})

	out := mustSettleWithin(t, New(client, rolloutNS), "web", time.Hour)
	if out.Reason != controlplane.ReasonProgressDeadlineExceeded {
		t.Errorf("reason = %q, want %q", out.Reason, controlplane.ReasonProgressDeadlineExceeded)
	}
}

// TestAwaitRolloutReportsAMissingWorkload asserts the wait reaches for ADR-0074 §6's existing reason
// for an absence rather than coining one, and does not wait out its bound on an object that is not
// coming back.
func TestAwaitRolloutReportsAMissingWorkload(t *testing.T) {
	out := mustSettleWithin(t, New(fake.NewSimpleClientset(), rolloutNS), "web", time.Hour)
	if out.Settled {
		t.Fatalf("outcome = %+v, want a failure", out)
	}
	if out.Reason != controlplane.ReasonWorkloadMissing {
		t.Errorf("reason = %q, want %q", out.Reason, controlplane.ReasonWorkloadMissing)
	}
}

// TestAwaitRolloutMutatesNothing is ADR-0072 §6 as a test rather than a comment: the wait observes a
// failed rollout and leaves it exactly as it found it. Burrow reports; the hook decides. The obvious
// next step — scale it, restart it, roll it back — is a mutation performed with nobody present.
func TestAwaitRolloutMutatesNothing(t *testing.T) {
	client := fake.NewSimpleClientset()
	seedDeployment(t, client, rolloutNS, "web", 1, appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 0, AvailableReplicas: 0,
	})
	seedAppPod(t, client, rolloutNS, "web", "web-new", corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: controlplane.ReasonCrashLoopBackOff}},
		}},
	})
	client.ClearActions()

	if out := mustSettleWithin(t, New(client, rolloutNS), "web", time.Hour); out.Settled {
		t.Fatalf("outcome = %+v, want a failure", out)
	}
	for _, action := range client.Actions() {
		switch action.GetVerb() {
		case "get", "list", "watch":
		default:
			t.Errorf("the settle wait performed a %s on %s: it is read-only (ADR-0072 §6)",
				action.GetVerb(), action.GetResource().Resource)
		}
	}
}

// TestAwaitRolloutRejectsABadApp asserts the one thing that IS an error rather than an outcome: a
// call that could not be made at all.
func TestAwaitRolloutRejectsABadApp(t *testing.T) {
	if _, err := New(fake.NewSimpleClientset(), rolloutNS).AwaitRollout(context.Background(), "Bad_Name", time.Minute); err == nil {
		t.Error("AwaitRollout should reject a bad app identifier")
	}
}

// TestAwaitRolloutNamesAPodThatIsRunningAndNotReady is the shape of the live failure in issue #546:
// the new pod was up, had been for five minutes, was serving nothing, and reported no blocking
// condition — so nothing classified it and the only thing the wait could say was that it had waited.
// "Running" is the word a reader takes for good news, so the observation has to say the other half.
func TestAwaitRolloutNamesAPodThatIsRunningAndNotReady(t *testing.T) {
	client := fake.NewSimpleClientset()
	seedDeployment(t, client, rolloutNS, "web", 1, appsv1.DeploymentStatus{
		ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
	})
	seedAppPod(t, client, rolloutNS, "web", "web-new", corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Ready: false, Started: ptr(true),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}},
	})

	out := mustSettleWithin(t, New(client, rolloutNS), "web", 10*time.Millisecond)
	if out.Settled {
		t.Fatalf("outcome = %+v, want a failure: no new replica ever became ready", out)
	}
	if out.Reason != controlplane.ReasonDeadlineExceeded {
		t.Errorf("reason = %q, want the backstop %q: nothing blocking was reported", out.Reason, controlplane.ReasonDeadlineExceeded)
	}
	if !strings.Contains(out.Detail, `pod "web-new" is Running but not ready`) {
		t.Errorf("detail = %q, want it to say the pod is up and not ready", out.Detail)
	}
}

// ptr returns a pointer to v, for the API's optional bool fields.
func ptr[T any](v T) *T { return &v }
