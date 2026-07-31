// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// appContainer returns the applied Deployment's single app container.
func appContainer(t *testing.T, client *fake.Clientset, app string) corev1.Container {
	t.Helper()
	dep, err := client.AppsV1().Deployments(ns).Get(context.Background(), app, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment %q: %v", app, err)
	}
	if n := len(dep.Spec.Template.Spec.Containers); n != 1 {
		t.Fatalf("containers = %d, want 1", n)
	}
	return dep.Spec.Template.Spec.Containers[0]
}

// TestNoProbeWhenNoneResolved: the zero ReadinessCheck authors nothing, so an app whose port Burrow
// does not know produces exactly the Deployment it produced before probes existed (ADR-0076 §3).
func TestNoProbeWhenNoneResolved(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	if err := a.ApplyWorkload(context.Background(), cp.WorkloadSpec{App: "worker", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	c := appContainer(t, client, "worker")
	if c.ReadinessProbe != nil {
		t.Errorf("ReadinessProbe = %+v, want nil when no probe was resolved", c.ReadinessProbe)
	}
}

// TestTCPReadinessProbeShape covers the conservative default's rendered form, including the timings.
// The thresholds are asserted because they ARE the conservatism (ADR-0076 §6): a serving pod must
// fail for about a minute before it is pulled out of its Service, and the per-attempt timeout is
// generous so a loaded pod is not evicted from the load balancer for being briefly slow.
func TestTCPReadinessProbeShape(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	spec := cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1, Readiness: cp.ReadinessCheck{Port: 8080}}
	if err := a.ApplyWorkload(context.Background(), spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	p := appContainer(t, client, "web").ReadinessProbe
	if p == nil {
		t.Fatal("ReadinessProbe is nil, want a TCP check")
	}
	if p.TCPSocket == nil || p.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("TCPSocket = %+v, want a connect to port 8080", p.TCPSocket)
	}
	if p.HTTPGet != nil {
		t.Errorf("HTTPGet = %+v, want nil: the default probe never guesses a path", p.HTTPGet)
	}
	if p.PeriodSeconds != cp.ReadinessPeriodSeconds || p.TimeoutSeconds != cp.ReadinessTimeoutSeconds ||
		p.FailureThreshold != cp.ReadinessFailureThreshold || p.SuccessThreshold != cp.ReadinessSuccessThreshold ||
		p.InitialDelaySeconds != cp.ReadinessInitialDelaySeconds {
		t.Errorf("probe timings = %+v, want the conservative constants", p)
	}
	if p.FailureThreshold*p.PeriodSeconds < 60 {
		t.Errorf("a serving pod leaves its Service after %ds of failure; ADR-0076 §6 wants roughly a minute or more",
			p.FailureThreshold*p.PeriodSeconds)
	}
}

// TestHTTPReadinessProbeShape covers the declared-endpoint form.
func TestHTTPReadinessProbeShape(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	spec := cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1, Readiness: cp.ReadinessCheck{Port: 9000, Path: "/healthz"}}
	if err := a.ApplyWorkload(context.Background(), spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	p := appContainer(t, client, "web").ReadinessProbe
	if p == nil || p.HTTPGet == nil {
		t.Fatalf("ReadinessProbe = %+v, want an HTTP GET", p)
	}
	if p.HTTPGet.Path != "/healthz" || p.HTTPGet.Port.IntValue() != 9000 {
		t.Errorf("HTTPGet = %+v, want GET /healthz on 9000", p.HTTPGet)
	}
	if p.TCPSocket != nil {
		t.Errorf("TCPSocket = %+v, want nil once a path is declared", p.TCPSocket)
	}
}

// TestBurrowNeverAuthorsALivenessProbe is ADR-0076 §1, asserted over every shape a workload can
// take. A liveness probe RESTARTS the container, so one that is even slightly wrong kills a working
// process repeatedly and presents as CrashLoopBackOff — manufacturing the exact failure it was
// installed to detect, under the load that made the process briefly slow. A startup probe is checked
// alongside it because its only purpose is to protect a slow boot from a liveness probe, and there
// is no liveness probe to protect it from.
func TestBurrowNeverAuthorsALivenessProbe(t *testing.T) {
	specs := []cp.WorkloadSpec{
		{App: "a", Image: "img:1", Replicas: 1},
		{App: "b", Image: "img:1", Replicas: 1, Readiness: cp.ReadinessCheck{Port: 8080}},
		{App: "c", Image: "img:1", Replicas: 1, Readiness: cp.ReadinessCheck{Port: 9000, Path: "/healthz"}},
		{App: "d", Image: "img:1", Replicas: 3, MetricsPort: 9100, Readiness: cp.ReadinessCheck{Port: 8080}},
	}
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	for _, spec := range specs {
		if err := a.ApplyWorkload(context.Background(), spec); err != nil {
			t.Fatalf("ApplyWorkload(%s): %v", spec.App, err)
		}
		c := appContainer(t, client, spec.App)
		if c.LivenessProbe != nil {
			t.Errorf("%s: LivenessProbe = %+v, want nil — Burrow never sets one by default (ADR-0076 §1)", spec.App, c.LivenessProbe)
		}
		if c.StartupProbe != nil {
			t.Errorf("%s: StartupProbe = %+v, want nil", spec.App, c.StartupProbe)
		}
	}
}

// TestReadinessProbeNeverChecksADependency is the rendered-object half of ADR-0076 §2's guard, and
// it is here because this is the file a contributor would edit to "improve" the probe.
//
// It asserts that whatever a user declares, the authored probe addresses THIS POD and nothing else:
// no Exec handler (which could run psql, curl, or anything at all), no gRPC handler, and — the one
// that matters most — an empty Host on the HTTP and TCP handlers, because setting Host is precisely
// how a Kubernetes probe is pointed at another machine. Kubernetes defaults an empty Host to the
// pod's own IP.
//
// If this test is failing because a probe now names a host or runs a command, the change is wrong
// even if it works: one shared Postgres backs every app in an environment, so a readiness probe that
// touched it would remove every replica of every app from service the moment that database blipped.
func TestReadinessProbeNeverChecksADependency(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	for _, r := range []cp.ReadinessCheck{{Port: 8080}, {Port: 9000, Path: "/healthz"}, {Port: 5432, Path: "/ready"}} {
		spec := cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1, Readiness: r}
		if err := a.ApplyWorkload(context.Background(), spec); err != nil {
			t.Fatalf("ApplyWorkload: %v", err)
		}
		p := appContainer(t, client, "web").ReadinessProbe
		if p == nil {
			t.Fatalf("ReadinessProbe is nil for %+v", r)
		}
		if p.Exec != nil {
			t.Errorf("%+v: Exec = %+v, want nil — a probe that runs a command can reach anything", r, p.Exec)
		}
		if p.GRPC != nil {
			t.Errorf("%+v: GRPC = %+v, want nil", r, p.GRPC)
		}
		if p.HTTPGet != nil && p.HTTPGet.Host != "" {
			t.Errorf("%+v: HTTPGet.Host = %q, want empty so the probe addresses this pod and no other host", r, p.HTTPGet.Host)
		}
		if p.TCPSocket != nil && p.TCPSocket.Host != "" {
			t.Errorf("%+v: TCPSocket.Host = %q, want empty so the probe addresses this pod and no other host", r, p.TCPSocket.Host)
		}
	}
}

// TestReadinessProbeIsReappliedOnUpdate: ApplyWorkload rebuilds the whole pod template, so removing
// a declared endpoint removes the probe from a workload that already exists rather than leaving the
// old one behind. Unsetting must actually unset.
func TestReadinessProbeIsReappliedOnUpdate(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1, Readiness: cp.ReadinessCheck{Port: 9000, Path: "/healthz"}}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1, Readiness: cp.ReadinessCheck{Port: 8080}}); err != nil {
		t.Fatalf("ApplyWorkload (update): %v", err)
	}
	p := appContainer(t, client, "web").ReadinessProbe
	if p == nil || p.HTTPGet != nil || p.TCPSocket == nil {
		t.Fatalf("ReadinessProbe = %+v, want the HTTP check replaced by a TCP one", p)
	}
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload (unset): %v", err)
	}
	if p := appContainer(t, client, "web").ReadinessProbe; p != nil {
		t.Errorf("ReadinessProbe = %+v after unsetting, want nil", p)
	}
}

// TestAFailingProbeSurfacesAsProgressDeadlineExceeded pins the vocabulary a failing readiness probe
// arrives in. A pod that starts but never passes its probe reports NO blocking pod condition — it is
// running, it is simply not ready — so the rollout stalls and the Deployment's Progressing condition
// goes False with ProgressDeadlineExceeded. That is an existing member of ADR-0074 §2's closed
// IssueReason set, and readiness deliberately adds no new one: an agent already branches on it, and a
// parallel reason invented here would be a second name for a state the surface can already describe.
//
// The rollout stalling rather than the app going down is also the §6 posture made concrete: the old
// ReplicaSet keeps serving, so a probe that is wrong costs a release, not an outage.
func TestAFailingProbeSurfacesAsProgressDeadlineExceeded(t *testing.T) {
	dep := wedgedRolloutDeployment("img:2")
	dep.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}},
		PeriodSeconds:    cp.ReadinessPeriodSeconds,
		FailureThreshold: cp.ReadinessFailureThreshold,
	}
	dep.Status.Conditions = append(dep.Status.Conditions, appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
	})
	a := kube.New(fake.NewSimpleClientset(dep), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.IssueReason != cp.ReasonProgressDeadlineExceeded {
		t.Errorf("issue reason = %q, want %q from the existing closed set", st.IssueReason, cp.ReasonProgressDeadlineExceeded)
	}
	if !cp.IsIssueReason(st.IssueReason) {
		t.Errorf("issue reason %q is not a member of the closed IssueReason set", st.IssueReason)
	}
	if st.Available {
		t.Error("a rollout whose new pods never pass readiness must not read as available")
	}
}
