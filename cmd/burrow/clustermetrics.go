// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

// clusterMetricsClientset builds the Kubernetes clientset the cluster-metrics subcommands act with.
// It is a package var so tests can substitute a fake; it defaults to the kubeconfig-driven clientset.
var clusterMetricsClientset = func(kubeconfig, kubeContext string) (kubernetes.Interface, error) {
	return clientsetForContext(kubeconfig, kubeContext)
}

// clusterMetricsOptions are the inputs to `burrow cluster metrics install`.
type clusterMetricsOptions struct {
	kubeconfig  string
	kubeContext string
	dryRun      bool
	force       bool
	verbose     bool
}

// newClusterMetricsCmd is `burrow cluster metrics install`: the standalone route to the
// metrics-server baseline of ADR-0054 §1.
//
// The baseline is auto-ensured by `burrow cluster install` and `burrow cluster bootstrap`, and by
// nothing else — so a cluster installed before the baseline existed, or one where
// `--no-metrics-server` was passed and the operator later changed their mind, had no way to get it
// short of re-running install against a live control plane (issue #524). What that costs is not
// obvious from the missing feature list: without the Metrics API nothing on the cluster reports what
// is actually CONSUMING CPU (`burrow cluster capacity` reports requests, which is what determines
// scheduling, not usage), so a component being starved — an ingress controller failing its own
// liveness probe under contention, say — is invisible for as long as nobody thinks to look. HPA
// autoscaling has the same dependency and degrades the same silent way.
//
// It is a subcommand rather than a step folded into `cluster upgrade` because the shape an operator
// already knows for a cluster component is `burrow cluster <component> install` — ingress, registry,
// and postgres are all spelled that way — and because it says out loud that the baseline is a thing
// you can have. metrics-server remains a BASELINE and not one of ADR-0054 §2's additive components:
// install still ensures it automatically, and this command is the repair for the clusters that
// automatic path never ran on.
func newClusterMetricsCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "metrics",
		Short: "Set up the cluster's metrics-server baseline (install)",
		Long: "metrics installs metrics-server, the baseline that serves live CPU and memory usage\n" +
			"through the Kubernetes Metrics API (ADR-0054). It powers `kubectl top`, `burrow app\n" +
			"autoscale` (HPA), and the utilization layer of capacity reporting. Without it nothing on\n" +
			"the cluster reports what is actually consuming CPU, so a starved component stays invisible.\n\n" +
			"`burrow cluster install` and `burrow cluster bootstrap` already ensure this baseline. This\n" +
			"command is for the clusters they did not: one installed before the baseline existed, or one\n" +
			"where `--no-metrics-server` was passed and you have since changed your mind.\n\n" +
			"It never installs over a copy the platform already ships. k3s, GKE, and AKS serve the\n" +
			"Metrics API themselves; EKS, DOKS, and kind do not. A cluster already serving it is left\n" +
			"exactly as it is.\n\n" +
			"A cluster where the Metrics API is registered and answering nothing is reported and left\n" +
			"alone too: that is a broken metrics-server, not a missing one, and re-applying a manifest\n" +
			"does not fix a kubelet it cannot scrape. `--force` applies the baseline anyway, for the case\n" +
			"where the workload is gone and only the APIService is left behind.\n\n" +
			"This is not `burrow addon install metrics`. That provisions a VictoriaMetrics instance for\n" +
			"one environment, which stores and answers queries about metrics over time; this installs the\n" +
			"cluster component that reports current usage at all, once per cluster.",
	}

	o := clusterMetricsOptions{}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the metrics-server baseline (skipped when the cluster already serves the Metrics API)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterMetricsInstall(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	install.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	bindLifecycleContext(install.Flags(), &o.kubeContext)
	install.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the manifest instead of applying it")
	install.Flags().BoolVar(&o.force, "force", false, "apply the baseline even when the Metrics API is already registered (replaces whatever registered it, including the platform's own copy)")
	install.Flags().BoolVar(&o.verbose, "verbose", false, "show every resource burrow applies instead of a summary")

	parent.AddCommand(install)
	return parent
}

// runClusterMetricsInstall ensures the pinned metrics-server baseline on the cluster the kubeconfig
// context names, through the same ensureMetricsServer install and bootstrap run — one detector, one
// embedded manifest, one set of messages, so the standalone route cannot drift from the automatic one.
//
// skip is false unconditionally: `--no-metrics-server` opts out of the baseline install ensures on
// the way past, and asking for that baseline by name is the opposite instruction. The flag stays
// exactly where it is on install and bootstrap; this command is what an operator who used it runs
// when they change their mind.
//
// The apply failure is returned rather than reported and swallowed. install treats a baseline hiccup
// as non-fatal because the control plane is already up and that is the run's real subject; here the
// baseline IS the subject, and a command that could not do the one thing it was asked to do must exit
// non-zero. A Metrics API that is registered and serving nothing exits non-zero for the same reason
// (ADR-0096 §3): the command was asked for a working Metrics API and there is not one.
func runClusterMetricsInstall(ctx context.Context, o clusterMetricsOptions, stdout, stderr io.Writer) error {
	// dry-run prints the manifest without contacting the cluster, the same way `cluster install
	// --dry-run` does, so it stays reviewable and pipeable. The detect-and-skip is left unresolved:
	// it needs the live read.
	if o.dryRun {
		fmt.Fprint(stdout, metricsServerManifest)
		return nil
	}

	// The real run installs a cluster-wide component, so the cluster has to be one somebody named
	// (cloud ADR-0038 §1); the dry run above renders a pinned manifest and contacts nothing.
	var err error
	o.kubeContext, err = lifecycleContext(o.kubeconfig, o.kubeContext, stderr)
	if err != nil {
		return err
	}

	cs, err := clusterMetricsClientset(o.kubeconfig, o.kubeContext)
	if err != nil {
		return err
	}
	return ensureMetricsServer(ctx, o.kubeconfig, o.kubeContext, cs, false, o.force, o.verbose, stdout, stderr)
}
