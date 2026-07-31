// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// configured builds a limits supplier holding one cluster value, the way `burrow cluster config set`
// leaves the store.
func configured(code controlplane.LimitCode, value string) controlplane.ClusterConfigFunc {
	cfg := controlplane.OperationalConfig{}.With(code, value)
	return func(context.Context) controlplane.OperationalConfig { return cfg }
}

// TestUnwiredAdapterKeepsTheOldConstants is the compatibility assertion behind the whole move
// (ADR-0068 §6): an adapter with no operational-limits supplier — every test in this package, and any
// install that has set nothing — resolves each limit to the number that used to be compiled in.
func TestUnwiredAdapterKeepsTheOldConstants(t *testing.T) {
	ctx := context.Background()
	if got := unschedulableGrace(ctx, nil); got != 30*time.Second {
		t.Errorf("unwired unschedulable grace = %s, want 30s", got)
	}
	if got := buildJobTTLSeconds(ctx, nil); got != 3*24*60*60 {
		t.Errorf("unwired build Job TTL = %ds, want 259200s (3 days)", got)
	}
	// 744h is 31 days, which is exactly what the literal `-retentionPeriod=1` meant: VictoriaMetrics
	// counts a bare number in months and its month is 31 days.
	args := addonArgs(controlplane.AddonSpec{Type: controlplane.AddonMetrics, Port: 8428},
		controlplane.ClusterConfigFunc(nil).ClusterDuration(ctx, controlplane.LimitAddonMetricRetention))
	if want := "-retentionPeriod=2678400s"; !hasArg(args, want) {
		t.Errorf("unwired metrics args = %v, want %s (the 31 days `-retentionPeriod=1` meant)", args, want)
	}
}

// TestUnschedulableGraceIsConfigured covers the occupant the record cares most about. The grace an
// operator sets is what the pod inspection waits out, and a cluster whose autoscaler takes ninety
// seconds to provision a node can say so instead of watching every deploy flip its apps to
// not-available.
func TestUnschedulableGraceIsConfigured(t *testing.T) {
	ctx := context.Background()
	pod := unschedulablePodFor(90 * time.Second)

	// With the built-in thirty seconds, a pod refused ninety seconds ago is reported.
	if ev, ok := podIssueEvidence(pod, unschedulableGrace(ctx, nil)); !ok || ev.Reason != controlplane.ReasonUnschedulable {
		t.Errorf("podIssueEvidence(90s old, 30s grace) = %q, %v, want unschedulable", ev.Reason, ok)
	}

	// Raise the grace past it and the same pod is a cluster still working on it.
	a := New(fake.NewSimpleClientset(), "apps").
		WithOperationalLimits(configured(controlplane.LimitUnschedulableGrace, "5m"))
	grace := unschedulableGrace(ctx, a.limits)
	if grace != 5*time.Minute {
		t.Fatalf("configured grace = %s, want 5m", grace)
	}
	if ev, ok := podIssueEvidence(pod, grace); ok {
		t.Errorf("podIssueEvidence(90s old, 5m grace) reported %q; the operator set a grace this pod is still inside", ev.Reason)
	}
}

// TestUnschedulableGraceIsSingleSourcedAcrossSurfaces is ADR-0068 §6's obligation, and the reason
// this is one value rather than two. The Deployment status path and the Job waiters judge the same
// pod, and a surface silent at thirty seconds beside one reporting at twenty would not merely be
// tuned differently — the two would hold different definitions of "failure", and an agent reading
// both would get contradictory answers about one pod at one moment.
func TestUnschedulableGraceIsSingleSourcedAcrossSurfaces(t *testing.T) {
	ctx := context.Background()
	limits := configured(controlplane.LimitUnschedulableGrace, "2m")
	grace := unschedulableGrace(ctx, limits)

	for _, age := range []time.Duration{time.Minute, 3 * time.Minute} {
		pod := unschedulablePodFor(age)
		_, deployment := podIssueEvidence(pod, grace)
		_, jobWait := jobPodStartupEvidence(pod, grace)
		if deployment != jobWait {
			t.Errorf("a pod unschedulable for %s: status surface reports %v, Job waiter reports %v — the two must agree on what unschedulable-for-too-long means", age, deployment, jobWait)
		}
		if want := age > grace; deployment != want {
			t.Errorf("a pod unschedulable for %s under a %s grace: reported = %v, want %v", age, grace, deployment, want)
		}
	}
}

// TestBuildJobRetentionIsConfigured asserts the operator's retention reaches the field Kubernetes
// actually reaps on. It is set at creation, so it applies to the next build rather than to a Job
// already waiting to be collected.
func TestBuildJobRetentionIsConfigured(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	b := NewBuilder(client).WithOperationalLimits(configured(controlplane.LimitBuildJobRetention, "1h"))
	if _, err := b.Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	ttl := (*created)[0].Spec.TTLSecondsAfterFinished
	if ttl == nil || *ttl != 3600 {
		t.Errorf("ttlSecondsAfterFinished = %v, want 3600 (the configured hour)", ttl)
	}
}

// TestMetricsRetentionIsConfigured asserts the operator's retention reaches the VictoriaMetrics flag
// on the add-on's container, in a form VictoriaMetrics documents.
func TestMetricsRetentionIsConfigured(t *testing.T) {
	ctx := context.Background()
	const addonNS = "burrow-addons"
	client := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: vmagentServiceAccount, Namespace: addonNS},
	})
	a := New(client, "apps").WithAddonNamespace(addonNS).
		WithOperationalLimits(configured(controlplane.LimitAddonMetricRetention, "168h"))

	spec := controlplane.AddonSpec{Type: controlplane.AddonMetrics, Backend: "victoriametrics", Image: "victoria-metrics:test", Port: 8428}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	dep, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the metrics Deployment: %v", err)
	}
	args := dep.Spec.Template.Spec.Containers[0].Args
	if want := "-retentionPeriod=604800s"; !hasArg(args, want) {
		t.Errorf("metrics args = %v, want %s (the configured week)", args, want)
	}
}

// TestMetricRetentionIsRenderedInSeconds pins the FORM the flag takes, not only its value. A Go
// duration renders as "168h0m0s", and passing that through would be relying on VictoriaMetrics'
// parser being lenient about a shape nothing else produces; seconds is a suffix it documents.
func TestMetricRetentionIsRenderedInSeconds(t *testing.T) {
	args := addonArgs(controlplane.AddonSpec{Type: controlplane.AddonMetrics, Port: 8428}, 168*time.Hour)
	for _, a := range args {
		if strings.HasPrefix(a, "-retentionPeriod=") {
			if a != "-retentionPeriod=604800s" {
				t.Errorf("retention arg = %q, want -retentionPeriod=604800s", a)
			}
			return
		}
	}
	t.Errorf("args = %v, no -retentionPeriod at all", args)
}

// TestUnreadableConfigurationDoesNotBreakTheOperation covers the posture a configuration read has to
// take on the deploy and status paths: a database that cannot be read yields the built-in defaults,
// not a failed operation. Validating a value is the write's job, where an operator is present.
func TestUnreadableConfigurationDoesNotBreakTheOperation(t *testing.T) {
	limits := controlplane.ClusterConfigFrom(func(context.Context) (controlplane.OperationalConfig, error) {
		return controlplane.OperationalConfig{}, errors.New("database unavailable")
	})
	if got := unschedulableGrace(context.Background(), limits); got != 30*time.Second {
		t.Errorf("grace with an unreadable configuration = %s, want the built-in 30s", got)
	}
}

// unschedulablePodFor builds a pod the scheduler refused age ago, the shape both the status surface
// and the Job waiters read.
func unschedulablePodFor(age time.Duration) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			Message:            "0/1 nodes are available: 1 Insufficient cpu.",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
		}},
	}}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
