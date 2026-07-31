// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// cnpgWaitTimeout bounds the wait for the operator's controller to roll out. It matches the ingress
// and cert-manager waits: long enough to cover an image pull on a small node, short enough that a
// wedged install is reported rather than sat on.
const cnpgWaitTimeout = 3 * time.Minute

// clusterPostgresClientset builds the Kubernetes clientset the cluster-postgres subcommands act
// with. It is a package var so tests can substitute a fake; it defaults to the kubeconfig-driven
// clientset.
var clusterPostgresClientset = func(kubeconfig string) (kubernetes.Interface, error) {
	return clientset(kubeconfig)
}

// detectCloudNativePGFn is the detection seam this command reads the cluster through. It is the
// control plane's own detector, not a second copy of it, so `cluster postgres install` decides
// whether to install from the same read `burrow cluster` reports.
var detectCloudNativePGFn = kube.DetectCloudNativePG

// clusterPostgresOptions are the inputs to `burrow cluster postgres install`.
type clusterPostgresOptions struct {
	kubeconfig string
	dryRun     bool
	wait       bool
	verbose    bool
}

// newClusterPostgresCmd is `burrow cluster postgres install`: the operator-CLI setup step that puts
// the CloudNativePG operator on the cluster (ADR-0066 §1).
//
// It is a setup command and not part of `burrow install` for the same reason
// `cluster ingress install` is: additive cluster components are opt-in subcommands, not `--with-*`
// flags on install (ADR-0054). It is separate from the AGENT surface for a harder reason —
// installing CRDs needs cluster-admin, which the agent does not have and must not. ADR-0066's
// Consequences name this as a real narrowing of ADR-0034's demand-driven model: a human runs this
// once, with their own kubeconfig, before the add-on can use it.
func newClusterPostgresCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "postgres",
		Short: "Set up the cluster's PostgreSQL operator (install)",
		Long: "postgres provisions the CloudNativePG operator, the cluster-wide prerequisite the\n" +
			"Postgres add-on's mechanism runs on (ADR-0066). It is a one-time setup an operator runs\n" +
			"with their kubeconfig, not an agent operation: it installs cluster-scoped CustomResource\n" +
			"Definitions, which needs cluster-admin.\n\n" +
			"This is not `burrow addon install postgres`. That provisions a database instance for one\n" +
			"environment and is what an app attaches to; this installs the operator underneath it, once\n" +
			"per cluster.",
	}

	o := clusterPostgresOptions{}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the CloudNativePG operator (skipped when it is already running)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterPostgresInstall(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	install.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	install.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the plan instead of applying it")
	install.Flags().BoolVar(&o.wait, "wait", true, "wait for the operator's controller to become ready")
	install.Flags().BoolVar(&o.verbose, "verbose", false, "show every resource burrow applies instead of a summary")

	parent.AddCommand(install)
	return parent
}

// runClusterPostgresInstall installs the pinned CloudNativePG release when the cluster does not
// already run it, then (with --wait) confirms the controller rolled out.
//
// The skip is keyed on a RUNNING controller, not on the CRDs being served. CRDs are cluster-scoped
// and outlive the operator that installed them, so an install that keyed off the API group would
// skip on a cluster whose cnpg-system namespace had been deleted — leaving CRDs that accept a
// `Cluster` object nothing will ever reconcile. Applying over an orphaned install is the repair, and
// the apply is a server-side apply with force-conflicts, so it adopts what is already there.
func runClusterPostgresInstall(ctx context.Context, o clusterPostgresOptions, stdout, stderr io.Writer) error {
	manifest := kube.CNPGManifestURL(kube.CNPGVersion)

	// dry-run prints the plan without contacting the cluster, so an operator can see what an install
	// would apply — and that it needs cluster-admin — before running it. The detect-and-skip is left
	// unresolved here: it needs the live read.
	if o.dryRun {
		writeClusterPostgresDryRunPlan(stdout, manifest)
		return nil
	}

	cs, err := clusterPostgresClientset(o.kubeconfig)
	if err != nil {
		return err
	}
	cnpg, err := detectCloudNativePGFn(ctx, cs)
	if err != nil {
		return err
	}

	writeClusterPostgresPlan(stdout, manifest, cnpg)

	fmt.Fprintln(stdout, "\nInstalling:")
	r := installReporter{w: stdout, verbose: o.verbose}
	if cnpg.Ready {
		r.done("CloudNativePG", cloudNativePGPresentStatus(cnpg)+", leaving it as is")
		writeClusterPostgresDone(stdout)
		return nil
	}

	r.working("CloudNativePG", "installing")
	detail, err := applyURLDetail(ctx, o.kubeconfig, manifest, o.verbose, stdout, stderr)
	if err != nil {
		return err
	}
	status := "installed " + kube.CNPGVersion + parenthesize(detail)
	if o.wait {
		r.working("CloudNativePG", "waiting for the controller")
		if err := waitForDeployment(ctx, cs, kube.CNPGNamespace, kube.CNPGControllerDeployment,
			"CloudNativePG operator", io.Discard, cnpgWaitTimeout); err != nil {
			return err
		}
		status += ", controller ready"
	}
	r.done("CloudNativePG", status)

	writeClusterPostgresDone(stdout)
	return nil
}

// cloudNativePGPresentStatus describes an already-running operator on the install's status line,
// naming its release and saying plainly when it is not the release Burrow targets. A skipped install
// that says only "already present" hides the one fact that matters on a skip.
func cloudNativePGPresentStatus(c controlplane.CloudNativePGCapability) string {
	switch {
	case c.Version == "":
		return "already running (version unknown)"
	case c.Version != kube.CNPGVersion:
		return "already running " + c.Version + " (Burrow targets " + kube.CNPGVersion + ")"
	default:
		return "already running " + c.Version
	}
}

// writeClusterPostgresPlan prints the live plan: what this run will apply, or that it will skip,
// and the cluster-admin requirement. The requirement is printed on every run, not only on failure —
// a permission error from a partial CRD apply is a poor way to learn what the command needed.
func writeClusterPostgresPlan(w io.Writer, manifest string, c controlplane.CloudNativePGCapability) {
	fmt.Fprintln(w, "Plan. Against your current cluster, postgres install will:")
	switch {
	case c.Ready:
		fmt.Fprintf(w, "  - CloudNativePG: %s, skip.\n", cloudNativePGPresentStatus(c))
	case c.Present:
		fmt.Fprintf(w, "  - re-apply CloudNativePG %s: its CRDs are installed but no controller is running, so apply %s\n", kube.CNPGVersion, manifest)
	default:
		fmt.Fprintf(w, "  - install CloudNativePG %s: apply %s\n", kube.CNPGVersion, manifest)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, clusterAdminNotice(w))
}

// writeClusterPostgresDryRunPlan prints the plan without contacting the cluster, so the install stays
// conditional ("if absent") — there has been no live read to resolve it.
func writeClusterPostgresDryRunPlan(w io.Writer, manifest string) {
	fmt.Fprintln(w, "Plan (dry run). Against your current cluster, postgres install would:")
	fmt.Fprintf(w, "  - install CloudNativePG %s if no controller is running: apply %s\n", kube.CNPGVersion, manifest)
	fmt.Fprintln(w)
	fmt.Fprintln(w, clusterAdminNotice(w))
}

// clusterAdminNotice states the privilege this command needs and why the agent cannot run it
// (ADR-0066 Consequences). This string surfaces to users, so it stays plain (no em-dashes).
func clusterAdminNotice(w io.Writer) string {
	return note(w) + "this installs cluster-scoped CustomResourceDefinitions and cluster RBAC, so it " +
		"needs cluster-admin on this kube context. It is an operator step: the agent has no such access " +
		"and cannot run it."
}

// writeClusterPostgresDone prints the closing block: what is ready, and the honest limit that the
// add-on does not use the operator yet (ADR-0009).
func writeClusterPostgresDone(w io.Writer) {
	fmt.Fprintln(w, "\nDone. The CloudNativePG operator is on the cluster.")
	fmt.Fprintln(w, "Check it anytime: burrow cluster")
	fmt.Fprintln(w, note(w)+"`burrow addon install postgres` still stands up its own Deployment by default.")
	fmt.Fprintln(w, "  Pass --cnpg to run a new instance on the operator instead. That path is opt-in while the")
	fmt.Fprintln(w, "  rest of ADR-0066 lands: backups still go through the same dump path, and removing a")
	fmt.Fprintln(w, "  CloudNativePG instance is not built yet.")
}
