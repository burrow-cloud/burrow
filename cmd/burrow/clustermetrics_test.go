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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// stubClusterMetricsClientset substitutes the clientset `cluster metrics install` acts with, so
// nothing in these tests reaches a live cluster or reads an ambient kubeconfig.
func stubClusterMetricsClientset(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	orig := clusterMetricsClientset
	clusterMetricsClientset = func(string, string) (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { clusterMetricsClientset = orig })
}

// metricsFakeClientset builds a fake cluster, optionally serving the Metrics API — the one signal
// detection reads.
func metricsFakeClientset(metricsServed bool) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}
	if metricsServed {
		cs.Resources = append(cs.Resources, &metav1.APIResourceList{GroupVersion: "metrics.k8s.io/v1beta1"})
	}
	return cs
}

// recordAppliedManifest substitutes the manifest apply seam with a recorder, so a test asserts what
// would be applied without touching a cluster.
func recordAppliedManifest(t *testing.T) *string {
	t.Helper()
	var applied string
	orig := applyFn
	applyFn = func(_ context.Context, _, _, manifests string, _ bool, _, _ io.Writer) error {
		applied = manifests
		return nil
	}
	t.Cleanup(func() { applyFn = orig })
	return &applied
}

// TestClusterMetricsInstallApplies asserts the motivating case (issue #524): a cluster with no
// Metrics API — installed before the baseline existed, or installed with --no-metrics-server — gets
// the pinned baseline without re-running `cluster install`. What is applied is the same embedded
// manifest install ensures, not a second copy of it.
func TestClusterMetricsInstallApplies(t *testing.T) {
	stubClusterMetricsClientset(t, metricsFakeClientset(false))
	applied := recordAppliedManifest(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "metrics", "install"}, &out, &errb); err != nil {
		t.Fatalf("cluster metrics install: %v\n%s", err, errb.String())
	}
	if *applied != metricsServerManifest {
		t.Errorf("applied %d bytes, want the embedded baseline manifest (%d bytes)", len(*applied), len(metricsServerManifest))
	}
	if s := out.String(); !strings.Contains(s, "installed") {
		t.Errorf("install output missing the installed message:\n%s", s)
	}
}

// TestClusterMetricsInstallLeavesAVendorCopyAlone asserts the detection is not bypassed by asking
// for the baseline explicitly. k3s, GKE, and AKS serve the Metrics API themselves; installing over
// one of those replaces a component the platform maintains with one Burrow pins.
func TestClusterMetricsInstallLeavesAVendorCopyAlone(t *testing.T) {
	stubClusterMetricsClientset(t, metricsFakeClientset(true))
	applied := recordAppliedManifest(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "metrics", "install"}, &out, &errb); err != nil {
		t.Fatalf("cluster metrics install: %v\n%s", err, errb.String())
	}
	if *applied != "" {
		t.Errorf("nothing may be applied to a cluster already serving the Metrics API, applied %d bytes", len(*applied))
	}
	if s := out.String(); !strings.Contains(s, "already serves the Metrics API") {
		t.Errorf("expected a present/leave-as-is message:\n%s", s)
	}
}

// TestClusterMetricsInstallDryRun asserts --dry-run prints the manifest and contacts no cluster at
// all — the clientset seam is not even reached, so the flag is safe to run against a context whose
// credentials would not permit the apply.
func TestClusterMetricsInstallDryRun(t *testing.T) {
	orig := clusterMetricsClientset
	clusterMetricsClientset = func(string, string) (kubernetes.Interface, error) {
		t.Error("--dry-run built a clientset; it must not contact the cluster")
		return metricsFakeClientset(false), nil
	}
	t.Cleanup(func() { clusterMetricsClientset = orig })

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "metrics", "install", "--dry-run"}, &out, &errb); err != nil {
		t.Fatalf("cluster metrics install --dry-run: %v\n%s", err, errb.String())
	}
	if out.String() != metricsServerManifest {
		t.Errorf("--dry-run printed %d bytes, want the embedded baseline manifest verbatim", out.Len())
	}
}

// TestClusterMetricsInstallFailsOnApplyError asserts this command exits non-zero when the apply
// fails, where install reports the same failure and carries on. The difference is deliberate: a
// baseline hiccup must not fail an already-installed control plane, but a command asked for nothing
// except the baseline cannot report success having not installed it.
func TestClusterMetricsInstallFailsOnApplyError(t *testing.T) {
	stubClusterMetricsClientset(t, metricsFakeClientset(false))
	orig := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error {
		return errors.New("clusterroles.rbac.authorization.k8s.io is forbidden")
	}
	t.Cleanup(func() { applyFn = orig })

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"cluster", "metrics", "install"}, &out, &errb)
	if err == nil {
		t.Fatalf("a failed apply must fail the command:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "installing the metrics-server baseline") {
		t.Errorf("error %q does not say what failed", err)
	}
}
