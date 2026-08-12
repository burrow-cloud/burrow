// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"

	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// errMetricsAPINotServing is the registered-but-not-serving outcome: the metrics.k8s.io group exists
// and answers nothing, so there is a metrics-server on this cluster and it is broken. It is a
// sentinel because the two callers report it differently — the standalone command exits non-zero on
// it, install prints nothing further, and neither may suggest re-running install, which reaches the
// same refusal (ADR-0096 §3). ensureMetricsServer has already written the full diagnosis to stdout
// by the time this is returned.
var errMetricsAPINotServing = errors.New("the cluster registers the Metrics API but nothing is serving it")

// metricsServerManifest is the pinned metrics-server baseline (upstream v0.7.2 with
// `--kubelet-insecure-tls` added), embedded so install/bootstrap can ensure it standalone — the
// same embedded-manifest pattern the in-cluster registry uses. See the manifest header for why the
// flag is added and the tradeoff it makes.
//
//go:embed manifests/metrics-server.yaml
var metricsServerManifest string

// metricsServerState reports what the cluster says about the Kubernetes Metrics API: serving
// (Present), registered and answering nothing (Registered without Present), or absent. It is the
// capability detector `burrow cluster` renders, not a second copy of it — one detector is why the
// install command and the capability report cannot disagree about the same cluster (ADR-0096 §2).
// Discovery needs no cluster-write access.
func metricsServerState(cs kubernetes.Interface) (controlplane.MetricsServerCapability, error) {
	caps, err := kube.DetectMetricsServer(cs)
	if err != nil {
		return caps, fmt.Errorf("checking whether the cluster serves the Metrics API: %w", err)
	}
	return caps, nil
}

// ensureMetricsServer auto-ensures the metrics-server baseline (ADR-0054 §1): metrics-server is a
// lightweight, detected baseline install/bootstrap ensures so `app autoscale` (HPA), `kubectl top`,
// and the utilization layer of capacity reporting behave the same on every cluster. Vendors ship it
// inconsistently — k3s, GKE, and AKS do; EKS, DOKS, and kind do not — so it is detected first and
// only ensured when absent, never installed over a vendor's copy.
//
//   - skip (from `--minimal` / `--no-metrics-server`) short-circuits to a one-line note so an
//     operator who manages metrics-server themselves is not overridden. It names the standalone
//     command, because opting out once should not be a one-way door (issue #524).
//   - a cluster that already SERVES the Metrics API is left untouched and reported present.
//   - a cluster that registers the group and serves nothing is reported and left alone, and the
//     caller is handed errMetricsAPINotServing. See below for why nothing is applied.
//   - otherwise the pinned baseline manifest is applied through the same apply seam install uses.
//
// force applies the baseline whatever the detector said. It exists for the one repair re-applying
// genuinely performs — an APIService left behind by objects that were deleted — and it is opt-in
// because the same apply over a vendor's registration replaces their metrics-server with Burrow's.
//
// The registered-but-not-serving state is REPORTED and not repaired, which is the whole judgement in
// this file (ADR-0096 §3). Discovery says the group is registered; it does not say by whom. Applying
// the pinned baseline there would overwrite a k3s/GKE/AKS copy that is merely down or slow, turning a
// transient outage into a permanent replacement — and where the registration is Burrow's own, the
// manifest on the cluster is already the manifest that would be applied, so the apply changes nothing
// and prints "installed" over a cluster where `kubectl top` is still dead. The causes that produce
// this state live outside the manifest (a kubelet too slow to scrape, a serving cert the API server
// will not accept, a starved node), so the honest report IS the repair: it tells the operator the
// thing they came to install is already there and broken, which is what they could not find out.
//
// It is best-effort in the sense the caller decides: it returns any apply error, but install treats
// a baseline failure as non-fatal (the control plane is already up), matching the capability
// summary's posture that a cluster read never fails an otherwise-successful install.
func ensureMetricsServer(ctx context.Context, kubeconfig, kubeContext string, cs kubernetes.Interface, skip, force, verbose bool, stdout, stderr io.Writer) error {
	if skip {
		fmt.Fprintln(stdout, "Skipping the metrics-server baseline (--no-metrics-server). `kubectl top`, HPA")
		fmt.Fprintln(stdout, "autoscaling, and utilization reporting need it; ensure it yourself, or add it later with")
		fmt.Fprintln(stdout, "`burrow cluster metrics install`.")
		return nil
	}

	state, err := metricsServerState(cs)
	if err != nil {
		return err
	}
	switch {
	case force && state.Registered:
		fmt.Fprintln(stdout, "metrics-server: the Metrics API is already registered; applying the baseline over it (--force).")
	case state.Present:
		fmt.Fprintln(stdout, "metrics-server: the cluster already serves the Metrics API, leaving it as is.")
		return nil
	case state.Registered:
		writeMetricsAPINotServing(stdout)
		return errMetricsAPINotServing
	}

	fmt.Fprintln(stdout, "Ensuring the metrics-server baseline (powers kubectl top, HPA autoscaling, and utilization reporting):")
	if err := applyFn(ctx, kubeconfig, kubeContext, metricsServerManifest, verbose, stdout, stderr); err != nil {
		return fmt.Errorf("installing the metrics-server baseline: %w", err)
	}
	fmt.Fprintln(stdout, "metrics-server: installed. It can take a moment to start serving; check `burrow cluster`.")
	return nil
}

// writeMetricsAPINotServing reports the registered-but-not-serving state. It is written out in full
// rather than summarized in one line because it is the state an operator arrives at this command
// least prepared for: they came to install a thing that turns out to be already installed, and the
// reason it does not work is somewhere this command cannot reach. So it says what is true, why
// nothing was applied, where to look, and the one case in which re-applying is the answer.
func writeMetricsAPINotServing(stdout io.Writer) {
	fmt.Fprintln(stdout, "metrics-server: the Metrics API is REGISTERED but not serving. Something owns metrics.k8s.io")
	fmt.Fprintln(stdout, "on this cluster and is answering nothing, so `kubectl top`, HPA autoscaling, and utilization")
	fmt.Fprintln(stdout, "reporting are all dead — this is not a missing baseline, it is a broken one.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Nothing was applied. The registration may be the platform's own copy (k3s, GKE, AKS), and")
	fmt.Fprintln(stdout, "re-applying a manifest does not fix the usual causes — a kubelet metrics-server cannot scrape,")
	fmt.Fprintln(stdout, "a serving certificate the API server rejects, a node with nothing left to schedule on:")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "  kubectl -n kube-system get pods -l k8s-app=metrics-server")
	fmt.Fprintln(stdout, "  kubectl -n kube-system logs -l k8s-app=metrics-server --tail=50")
	fmt.Fprintln(stdout, "  kubectl get apiservice v1beta1.metrics.k8s.io -o wide")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "If the workload is gone and only the APIService is left, re-apply the baseline over it with")
	fmt.Fprintln(stdout, "`burrow cluster metrics install --force`. That replaces whatever registered the API with")
	fmt.Fprintln(stdout, "Burrow's pinned copy, so it is deliberate rather than the default.")
}
