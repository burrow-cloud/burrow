// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// TestMetricsServerManifestContents asserts the embedded baseline manifest carries the pieces that
// make it work turnkey across cluster types: the pinned image, the APIService that registers the
// Metrics API, and the `--kubelet-insecure-tls` flag managed and self-hosted clusters (DOKS, EKS,
// kind) need so metrics-server can reach the kubelet without a cluster-CA-signed serving cert.
func TestMetricsServerManifestContents(t *testing.T) {
	for _, want := range []string{
		"kind: APIService",
		"name: v1beta1.metrics.k8s.io",
		"registry.k8s.io/metrics-server/metrics-server:v0.7.2", // pinned image
		"--kubelet-insecure-tls",                               // the cross-cluster caveat, handled
		"name: metrics-server",
		"namespace: kube-system",
	} {
		if !strings.Contains(metricsServerManifest, want) {
			t.Errorf("embedded metrics-server manifest missing %q", want)
		}
	}
}

// TestMetricsServerStateDetectsAPIGroup proves detection keys off the metrics.k8s.io API group —
// the signal a vendor copy (k3s, GKE, AKS) or a prior install serves — so the baseline is skipped
// where the platform already ships it.
func TestMetricsServerStateDetectsAPIGroup(t *testing.T) {
	present := fake.NewSimpleClientset()
	present.Resources = []*metav1.APIResourceList{
		{GroupVersion: "metrics.k8s.io/v1beta1"},
		{GroupVersion: "apps/v1"},
	}
	state, err := metricsServerState(present)
	if err != nil {
		t.Fatalf("metricsServerState: %v", err)
	}
	if !state.Present || !state.Registered {
		t.Errorf("metrics-server should be serving when metrics.k8s.io/v1beta1 is served, got %+v", state)
	}

	absent := fake.NewSimpleClientset()
	absent.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}
	state, err = metricsServerState(absent)
	if err != nil {
		t.Fatalf("metricsServerState: %v", err)
	}
	if state.Present || state.Registered {
		t.Errorf("metrics-server should be absent when only apps/v1 is served, got %+v", state)
	}
}

// staleMetricsClientset is a cluster whose Metrics API is registered and serving nothing — issue
// #561's cluster. The group is returned with an EMPTY version list, which is not a contrivance: the
// aggregation layer keeps the group when its backing service stops answering and marks the version
// stale, and client-go's SplitGroupsAndResources drops a stale GroupVersion while still appending
// the group. The fake clientset cannot express it (its discovery derives groups FROM versions), so
// discovery is substituted wholesale.
type staleMetricsClientset struct {
	kubernetes.Interface
	groups *metav1.APIGroupList
}

func (c staleMetricsClientset) Discovery() discovery.DiscoveryInterface {
	return groupListDiscovery{groups: c.groups}
}

// groupListDiscovery answers ServerGroups from a fixed list and nothing else — every other method
// is the embedded nil interface and panics if a detector reaches for one, which is the assertion
// that detection stays a single discovery read needing no RBAC (ADR-0034).
type groupListDiscovery struct {
	discovery.DiscoveryInterface
	groups *metav1.APIGroupList
}

func (d groupListDiscovery) ServerGroups() (*metav1.APIGroupList, error) { return d.groups, nil }

// staleMetricsCluster is a cluster registering metrics.k8s.io with no usable version behind it.
func staleMetricsCluster() kubernetes.Interface {
	return staleMetricsClientset{
		Interface: fake.NewSimpleClientset(),
		groups: &metav1.APIGroupList{Groups: []metav1.APIGroup{
			{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}}},
			{Name: "metrics.k8s.io"}, // registered, every version stale
		}},
	}
}

// TestMetricsServerStateStaleVersionIsNotServing is issue #561 itself: a cluster whose metrics.k8s.io
// group is registered with no usable version reported the baseline as present, so the command that
// exists to repair it applied nothing and printed success. The state is registered AND NOT serving —
// a detector that answers only "present or absent" gets this cluster wrong whichever it picks.
func TestMetricsServerStateStaleVersionIsNotServing(t *testing.T) {
	state, err := metricsServerState(staleMetricsCluster())
	if err != nil {
		t.Fatalf("metricsServerState: %v", err)
	}
	if state.Present {
		t.Errorf("a stale metrics.k8s.io group must not read as serving, got %+v", state)
	}
	if !state.Registered {
		t.Errorf("a stale metrics.k8s.io group is still registered, got %+v", state)
	}
}

// TestEnsureMetricsServerStaleReportsAndRefuses proves what the command does about that state: it
// applies nothing — the registration may be a vendor's, and re-applying repairs none of the causes —
// says out loud that metrics-server is installed and broken rather than missing, and returns
// errMetricsAPINotServing so the standalone command exits non-zero (ADR-0096 §3).
func TestEnsureMetricsServerStaleReportsAndRefuses(t *testing.T) {
	origApply := applyFn
	applied := false
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error {
		applied = true
		return nil
	}
	t.Cleanup(func() { applyFn = origApply })

	var out bytes.Buffer
	err := ensureMetricsServer(context.Background(), "", "", staleMetricsCluster(), false, false, false, &out, io.Discard)
	if !errors.Is(err, errMetricsAPINotServing) {
		t.Fatalf("ensureMetricsServer = %v, want errMetricsAPINotServing", err)
	}
	if applied {
		t.Errorf("nothing may be applied over a registration burrow did not make")
	}
	for _, want := range []string{"REGISTERED but not serving", "Nothing was applied", "--force"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q, got %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "already serves the Metrics API") {
		t.Errorf("a broken baseline must never be reported as already serving, got %q", out.String())
	}
}

// TestEnsureMetricsServerForceAppliesOverRegistration proves the one repair re-applying performs is
// still reachable: --force applies the pinned baseline over a registration that serves nothing, for
// the cluster where the workload was deleted and only the APIService is left.
func TestEnsureMetricsServerForceAppliesOverRegistration(t *testing.T) {
	origApply := applyFn
	var appliedManifest string
	applyFn = func(_ context.Context, _, _, manifests string, _ bool, _, _ io.Writer) error {
		appliedManifest = manifests
		return nil
	}
	t.Cleanup(func() { applyFn = origApply })

	var out bytes.Buffer
	if err := ensureMetricsServer(context.Background(), "", "", staleMetricsCluster(), false, true, false, &out, io.Discard); err != nil {
		t.Fatalf("ensureMetricsServer --force: %v", err)
	}
	if appliedManifest != metricsServerManifest {
		t.Errorf("--force must apply the embedded baseline, got %d bytes", len(appliedManifest))
	}
	if !strings.Contains(out.String(), "--force") {
		t.Errorf("expected the forced apply to say so, got %q", out.String())
	}
}

// TestEnsureMetricsServerPresentSkipsInstall proves the vendor-copy path: a cluster already serving
// the Metrics API is left untouched — the baseline manifest is never applied — and reported present.
func TestEnsureMetricsServerPresentSkipsInstall(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "metrics.k8s.io/v1beta1"}}

	origApply := applyFn
	applied := false
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error {
		applied = true
		return nil
	}
	t.Cleanup(func() { applyFn = origApply })

	var out bytes.Buffer
	if err := ensureMetricsServer(context.Background(), "", "", cs, false, false, false, &out, io.Discard); err != nil {
		t.Fatalf("ensureMetricsServer: %v", err)
	}
	if applied {
		t.Errorf("baseline must not be applied when the cluster already serves the Metrics API")
	}
	if !strings.Contains(out.String(), "already serves the Metrics API") {
		t.Errorf("expected a present/leave-as-is message, got %q", out.String())
	}
}

// TestEnsureMetricsServerAbsentInstalls proves the ensure path: a cluster with no Metrics API gets
// the pinned baseline applied through the same apply seam install uses, with the embedded manifest.
func TestEnsureMetricsServerAbsentInstalls(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}

	origApply := applyFn
	var appliedManifest string
	applyFn = func(_ context.Context, _, _, manifests string, _ bool, _, _ io.Writer) error {
		appliedManifest = manifests
		return nil
	}
	t.Cleanup(func() { applyFn = origApply })

	var out bytes.Buffer
	if err := ensureMetricsServer(context.Background(), "", "", cs, false, false, false, &out, io.Discard); err != nil {
		t.Fatalf("ensureMetricsServer: %v", err)
	}
	if appliedManifest != metricsServerManifest {
		t.Errorf("expected the embedded baseline manifest to be applied, got %d bytes", len(appliedManifest))
	}
	if !strings.Contains(out.String(), "installed") {
		t.Errorf("expected an installed message, got %q", out.String())
	}
}

// TestEnsureMetricsServerOptOut proves --minimal / --no-metrics-server short-circuits before any
// cluster read or apply: nothing is installed and the operator is told the baseline was skipped.
func TestEnsureMetricsServerOptOut(t *testing.T) {
	// A cluster that is ABSENT the Metrics API — so only the skip flag can prevent an install.
	cs := fake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}

	origApply := applyFn
	applied := false
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error {
		applied = true
		return nil
	}
	t.Cleanup(func() { applyFn = origApply })

	var out bytes.Buffer
	if err := ensureMetricsServer(context.Background(), "", "", cs, true, false, false, &out, io.Discard); err != nil {
		t.Fatalf("ensureMetricsServer: %v", err)
	}
	if applied {
		t.Errorf("baseline must not be applied when opted out")
	}
	if !strings.Contains(out.String(), "Skipping the metrics-server baseline") {
		t.Errorf("expected a skip message, got %q", out.String())
	}
}
