// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// TestMetricsServerPresentDetectsAPIGroup proves detection keys off the metrics.k8s.io API group —
// the signal a vendor copy (k3s, GKE, AKS) or a prior install serves — so the baseline is skipped
// where the platform already ships it.
func TestMetricsServerPresentDetectsAPIGroup(t *testing.T) {
	present := fake.NewSimpleClientset()
	present.Resources = []*metav1.APIResourceList{
		{GroupVersion: "metrics.k8s.io/v1beta1"},
		{GroupVersion: "apps/v1"},
	}
	ok, err := metricsServerPresent(present)
	if err != nil {
		t.Fatalf("metricsServerPresent: %v", err)
	}
	if !ok {
		t.Errorf("metrics-server should be detected present when metrics.k8s.io is served")
	}

	absent := fake.NewSimpleClientset()
	absent.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}
	ok, err = metricsServerPresent(absent)
	if err != nil {
		t.Fatalf("metricsServerPresent: %v", err)
	}
	if ok {
		t.Errorf("metrics-server should be absent when only apps/v1 is served")
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
	if err := ensureMetricsServer(context.Background(), "", "", cs, false, false, &out, io.Discard); err != nil {
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
	if err := ensureMetricsServer(context.Background(), "", "", cs, false, false, &out, io.Discard); err != nil {
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
	if err := ensureMetricsServer(context.Background(), "", "", cs, true, false, &out, io.Discard); err != nil {
		t.Fatalf("ensureMetricsServer: %v", err)
	}
	if applied {
		t.Errorf("baseline must not be applied when opted out")
	}
	if !strings.Contains(out.String(), "Skipping the metrics-server baseline") {
		t.Errorf("expected a skip message, got %q", out.String())
	}
}
