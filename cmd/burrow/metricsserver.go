// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"

	"k8s.io/client-go/kubernetes"
)

// metricsAPIGroup is the Kubernetes Metrics API group metrics-server serves. Its presence in
// API-group discovery means a metrics-server (or a vendor's equivalent on k3s, GKE, or AKS) is
// already registered, so Burrow's baseline should leave it alone. Discovery needs no RBAC.
const metricsAPIGroup = "metrics.k8s.io"

// metricsServerManifest is the pinned metrics-server baseline (upstream v0.7.2 with
// `--kubelet-insecure-tls` added), embedded so install/bootstrap can ensure it standalone — the
// same embedded-manifest pattern the in-cluster registry uses. See the manifest header for why the
// flag is added and the tradeoff it makes.
//
//go:embed manifests/metrics-server.yaml
var metricsServerManifest string

// metricsServerPresent reports whether the cluster already serves the Kubernetes Metrics API,
// detected by the metrics.k8s.io group in API-group discovery. A vendor copy (k3s, GKE, AKS) or a
// prior install registers this group, so a true result means the baseline must be left as is
// (ADR-0054 §1). Discovery needs no cluster-write access.
func metricsServerPresent(cs kubernetes.Interface) (bool, error) {
	groups, err := cs.Discovery().ServerGroups()
	if err != nil {
		return false, fmt.Errorf("checking whether the cluster serves the Metrics API: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == metricsAPIGroup {
			return true, nil
		}
	}
	return false, nil
}

// ensureMetricsServer auto-ensures the metrics-server baseline (ADR-0054 §1): metrics-server is a
// lightweight, detected baseline install/bootstrap ensures so `app autoscale` (HPA), `kubectl top`,
// and the utilization layer of capacity reporting behave the same on every cluster. Vendors ship it
// inconsistently — k3s, GKE, and AKS do; EKS, DOKS, and kind do not — so it is detected first and
// only ensured when absent, never installed over a vendor's copy.
//
//   - skip (from `--minimal` / `--no-metrics-server`) short-circuits to a one-line note so an
//     operator who manages metrics-server themselves is not overridden.
//   - a cluster that already serves the Metrics API is left untouched and reported present.
//   - otherwise the pinned baseline manifest is applied through the same apply seam install uses.
//
// It is best-effort in the sense the caller decides: it returns any apply error, but install treats
// a baseline failure as non-fatal (the control plane is already up), matching the capability
// summary's posture that a cluster read never fails an otherwise-successful install.
func ensureMetricsServer(ctx context.Context, kubeconfig, kubeContext string, cs kubernetes.Interface, skip, verbose bool, stdout, stderr io.Writer) error {
	if skip {
		fmt.Fprintln(stdout, "Skipping the metrics-server baseline (--no-metrics-server). `kubectl top`, HPA")
		fmt.Fprintln(stdout, "autoscaling, and utilization reporting need it; ensure it yourself or re-run without the flag.")
		return nil
	}

	present, err := metricsServerPresent(cs)
	if err != nil {
		return err
	}
	if present {
		fmt.Fprintln(stdout, "metrics-server: the cluster already serves the Metrics API, leaving it as is.")
		return nil
	}

	fmt.Fprintln(stdout, "Ensuring the metrics-server baseline (powers kubectl top, HPA autoscaling, and utilization reporting):")
	if err := applyFn(ctx, kubeconfig, kubeContext, metricsServerManifest, verbose, stdout, stderr); err != nil {
		return fmt.Errorf("installing the metrics-server baseline: %w", err)
	}
	fmt.Fprintln(stdout, "metrics-server: installed. It can take a moment to start serving; check `burrow cluster`.")
	return nil
}
