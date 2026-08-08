// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// watchDeadline bounds how long a test waits for an event that should already be on its way. It is
// a failure bound, not a timing assumption: every wait here is satisfied by the first delivery.
const watchDeadline = 5 * time.Second

// awaitEvent takes the next event of one kind, failing the test if it does not arrive.
func awaitEvent(t *testing.T, events <-chan cp.WorkloadEvent, kind cp.WorkloadEventKind) cp.WorkloadEvent {
	t.Helper()
	deadline := time.After(watchDeadline)
	for {
		select {
		case ev := <-events:
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event arrived within %s", kind, watchDeadline)
			return cp.WorkloadEvent{}
		}
	}
}

// TestWatchWorkloadsReportsTheCurrentPictureThenSyncs: the watch opens by reporting every managed
// workload in its namespace and then saying it has a complete picture, which is what resumes
// coverage (ADR-0079 §1, §4). The state it reports is derived through the same function
// ListWorkloads uses, so a ledger row and a status answer cannot disagree.
func TestWatchWorkloadsReportsTheCurrentPictureThenSyncs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	events := make(chan cp.WorkloadEvent, 16)
	if err := a.WatchWorkloads(ctx, events); err != nil {
		t.Fatalf("WatchWorkloads: %v", err)
	}

	ev := awaitEvent(t, events, cp.WorkloadChanged)
	if ev.Status.App != "web" || ev.Namespace != ns {
		t.Errorf("event = %+v, want the web workload in %s", ev, ns)
	}
	want, err := a.WorkloadStatus(ctx, "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if ev.Status != want {
		t.Errorf("the watch derived %+v and the status surface derived %+v; they must be the same function", ev.Status, want)
	}
	awaitEvent(t, events, cp.WorkloadSynced)
}

// TestWatchWorkloadsReportsAPodCondition: almost every reason in the ledger's vocabulary is a
// container or scheduling condition, and a Deployment's own object barely moves while one is
// happening. A watch over Deployments alone would miss most of what the ledger exists to record.
func TestWatchWorkloadsReportsAPodCondition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	events := make(chan cp.WorkloadEvent, 32)
	if err := a.WatchWorkloads(ctx, events); err != nil {
		t.Fatalf("WatchWorkloads: %v", err)
	}
	awaitEvent(t, events, cp.WorkloadSynced)

	// A pod of that app starts crash-looping. Nothing about the Deployment changes.
	if _, err := client.CoreV1().Pods(ns).Create(ctx, watchedCrashLoopPod("web"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}

	deadline := time.After(watchDeadline)
	for {
		select {
		case ev := <-events:
			if ev.Kind == cp.WorkloadChanged && ev.Status.IssueReason == cp.ReasonCrashLoopBackOff {
				return
			}
		case <-deadline:
			t.Fatalf("a crash-looping pod produced no %s event within %s", cp.ReasonCrashLoopBackOff, watchDeadline)
		}
	}
}

// TestWatchWorkloadsDoesNotApplyTheUnschedulableGrace: the status surface withholds a scheduling
// failure younger than status.unschedulable_grace because a live read is a question about this
// moment. The watch reports the edge, and its consumer holds the transition for the dwell
// (ADR-0079 §2–§3) — the same value spent once. Applying it here as well would double it and blind
// the ledger to the first thirty seconds of everything.
func TestWatchWorkloadsDoesNotApplyTheUnschedulableGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	events := make(chan cp.WorkloadEvent, 32)
	if err := a.WatchWorkloads(ctx, events); err != nil {
		t.Fatalf("WatchWorkloads: %v", err)
	}
	awaitEvent(t, events, cp.WorkloadSynced)

	// Refused by the scheduler one second ago — well inside the grace the live surface applies.
	if _, err := client.CoreV1().Pods(ns).Create(ctx, watchedUnschedulablePod("web", time.Second), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}

	// The status surface says nothing about it yet.
	st, err := a.WorkloadStatus(ctx, "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.IssueReason != "" {
		t.Fatalf("the status surface reported %q inside the grace; this test's premise is gone", st.IssueReason)
	}

	deadline := time.After(watchDeadline)
	for {
		select {
		case ev := <-events:
			if ev.Kind == cp.WorkloadChanged && ev.Status.IssueReason == cp.ReasonUnschedulable {
				return
			}
		case <-deadline:
			t.Fatalf("the watch withheld a scheduling refusal the consumer's dwell is supposed to hold")
		}
	}
}

// TestWatchWorkloadsSaysWhenItLostItsPlace: a watch that reconnects silently is indistinguishable,
// in an empty ledger, from a cluster in which nothing happened. A stream the server ends with an
// error is a re-list, and a re-list reports current state rather than what was missed — so it is a
// gap, and it has to be visible (ADR-0079 §4).
func TestWatchWorkloadsSaysWhenItLostItsPlace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fake.NewSimpleClientset()
	watcher := watch.NewRaceFreeFake()
	client.PrependWatchReactor("deployments", k8stesting.DefaultWatchReactor(watcher, nil))
	a := kube.New(client, ns)
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	events := make(chan cp.WorkloadEvent, 32)
	if err := a.WatchWorkloads(ctx, events); err != nil {
		t.Fatalf("WatchWorkloads: %v", err)
	}
	awaitEvent(t, events, cp.WorkloadSynced)

	watcher.Error(&metav1.Status{Message: "too old resource version"})

	ev := awaitEvent(t, events, cp.WorkloadDropped)
	if ev.Namespace != ns {
		t.Errorf("dropped event = %+v, want it to name the namespace that stopped being covered", ev)
	}
	if ev.Detail == "" {
		t.Errorf("a dropped watch reported no reason; the coverage record has nothing to say")
	}
}

// TestWatchWorkloadsRefusesToStartOnAnUnreadableNamespace: establishing runs synchronously so a
// namespace that cannot be watched at all is an error the caller can report and retry, rather than a
// goroutine failing forever in a log while coverage claims the namespace is watched.
func TestWatchWorkloadsRefusesToStartOnAnUnreadableNamespace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server is unreachable")
	})
	a := kube.New(client, ns)

	if err := a.WatchWorkloads(ctx, make(chan cp.WorkloadEvent, 1)); err == nil {
		t.Fatal("WatchWorkloads succeeded against a namespace it could not list")
	}
}

// watchedCrashLoopPod is a pod of app whose container is in CrashLoopBackOff, labelled the way
// ApplyWorkload labels the pods it authors.
func watchedCrashLoopPod(app string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app + "-abc",
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/name": app, "app.kubernetes.io/managed-by": "burrow"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: app,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: cp.ReasonCrashLoopBackOff,
				}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			}},
		},
	}
}

// watchedUnschedulablePod is a pod of app the scheduler refused `age` ago, labelled the way
// ApplyWorkload labels the pods it authors — the label the watch selects by.
func watchedUnschedulablePod(app string, age time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app + "-def",
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/name": app, "app.kubernetes.io/managed-by": "burrow"},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{
				Type:               corev1.PodScheduled,
				Status:             corev1.ConditionFalse,
				Reason:             cp.ReasonUnschedulable,
				Message:            "0/3 nodes are available: insufficient cpu",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
			}},
		},
	}
}
