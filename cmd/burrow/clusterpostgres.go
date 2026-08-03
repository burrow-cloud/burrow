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
var clusterPostgresClientset = func(kubeconfig, kubeContext string) (kubernetes.Interface, error) {
	return clientsetForContext(kubeconfig, kubeContext)
}

// detectCloudNativePGFn is the detection seam this command reads the cluster through. It is the
// control plane's own detector, not a second copy of it, so `cluster postgres install` decides
// whether to install from the same read `burrow cluster` reports.
var detectCloudNativePGFn = kube.DetectCloudNativePG

// detectPgBackRestFn and detectCertManagerFn are the other two reads this command makes: the backup
// plugin it installs beside the operator, and the prerequisite that plugin's manifest needs.
var (
	detectPgBackRestFn  = kube.DetectPgBackRest
	detectCertManagerFn = kube.DetectCertManager
)

// clusterPostgresOptions are the inputs to `burrow cluster postgres install`.
type clusterPostgresOptions struct {
	kubeconfig  string
	kubeContext string
	dryRun      bool
	wait        bool
	verbose     bool
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
		Short: "Set up the cluster's PostgreSQL operator and backup plugin (install)",
		Long: "postgres provisions the CloudNativePG operator and its pgBackRest backup plugin, the\n" +
			"cluster-wide prerequisites the Postgres add-on runs on (ADR-0066). It is a one-time setup an\n" +
			"operator runs with their kubeconfig, not an agent operation: it installs cluster-scoped\n" +
			"CustomResource Definitions, which needs cluster-admin.\n\n" +
			"This is not `burrow addon install postgres`. That provisions a database instance for one\n" +
			"environment and is what an app attaches to; this installs the operator underneath it, once\n" +
			"per cluster.",
	}

	o := clusterPostgresOptions{}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the CloudNativePG operator and pgBackRest plugin (each skipped when already running)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A cluster-lifecycle command acts on a kubeconfig context rather than the active
			// target (ADR-0078 §3), so say which context that is whenever the target names another
			// one — the choice is deliberate, and the person reading it should not have to infer it.
			noteLifecycleContext(o.kubeconfig, o.kubeContext, cmd.ErrOrStderr())
			return runClusterPostgresInstall(cmd.Context(), o, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	install.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	bindLifecycleContext(install.Flags(), &o.kubeContext)
	install.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the plan instead of applying it")
	install.Flags().BoolVar(&o.wait, "wait", true, "wait for the operator's controller to become ready")
	install.Flags().BoolVar(&o.verbose, "verbose", false, "show the manifest URLs in the plan, and every resource burrow applies instead of a summary")

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

	cs, err := clusterPostgresClientset(o.kubeconfig, o.kubeContext)
	if err != nil {
		return err
	}
	cnpg, err := detectCloudNativePGFn(ctx, cs)
	if err != nil {
		return err
	}
	// The backup plugin's two reads happen HERE, before the plan is printed, rather than inside
	// installPgBackRest where they used to. A plan that announced the plugin and then skipped it
	// further down — because its controller was already running, or because cert-manager is absent —
	// described a run that did not happen, and the reader had to notice the difference themselves. All
	// three reads are read-only discovery, so resolving them up front buys a plan that is accurate
	// about both components rather than one.
	plugin, err := detectPgBackRestFn(ctx, cs)
	if err != nil {
		return err
	}
	certs, err := detectCertManagerFn(cs)
	if err != nil {
		return err
	}

	writeClusterPostgresPlan(stdout, o.verbose, manifest, cnpg, plugin, certs)

	fmt.Fprintln(stdout, "\nInstalling:")
	r := installReporter{w: stdout, verbose: o.verbose}

	// A running operator is left alone, but the run CONTINUES to the backup plugin. It used to return
	// here, which meant a cluster that already had CloudNativePG never got the plugin installed at
	// all — `cluster postgres install` on it reported success having done nothing, and the instances
	// that followed archived nowhere. The two components are installed independently for the same
	// reason each is detected independently: one being present says nothing about the other.
	if cnpg.Ready {
		r.done("CloudNativePG", cloudNativePGPresentStatus(cnpg)+", leaving it as is")
	} else {
		r.working("CloudNativePG", "installing")
		detail, err := applyURLDetail(ctx, o.kubeconfig, o.kubeContext, manifest, o.verbose, stdout, stderr)
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
	}

	if err := installPgBackRest(ctx, o, r, cs, plugin, certs, stdout, stderr); err != nil {
		return err
	}
	writeClusterPostgresDone(stdout)
	return nil
}

// installPgBackRest applies the pinned pgBackRest plugin release when the cluster does not already
// run it (ADR-0066 §3). It is the same detect-and-skip shape as the operator above, keyed on a
// RUNNING controller for the same reason: the CRDs outlive the controller, and a `Stanza` written
// against a served CRD with nothing behind it is accepted and reconciled by nothing.
//
// IT IS SKIPPED, NOT FAILED, WITHOUT CERT-MANAGER. The plugin's manifest contains cert-manager
// Certificate and Issuer objects — the operator and the plugin authenticate to each other over TLS —
// so applying it on a cluster without cert-manager fails part-way through and leaves exactly the
// half-installed state this command exists to avoid. An operator who has not set up ingress yet has
// no cert-manager and did not ask for one here; they get the operator, a plain instance, and a line
// saying what is missing and which command installs it.
//
// The two reads it branches on are made by the caller, before the plan is printed, and passed in: the
// plan states which of these three outcomes this run will take, so it cannot promise one and deliver
// another.
func installPgBackRest(ctx context.Context, o clusterPostgresOptions, r installReporter, cs kubernetes.Interface,
	plugin controlplane.PgBackRestCapability, certs controlplane.CertManagerCapability, stdout, stderr io.Writer) error {
	if plugin.Ready {
		r.done("pgBackRest plugin", "already running, leaving it as is")
		return nil
	}
	if !certs.Present {
		r.skipped("pgBackRest plugin", "cert-manager is not installed and the plugin's manifest needs it; "+
			"run `burrow cluster ingress install` first, then re-run this")
		return nil
	}

	r.working("pgBackRest plugin", "installing")
	manifest := kube.PgBackRestManifestURL(kube.PgBackRestVersion)
	detail, err := applyURLDetail(ctx, o.kubeconfig, o.kubeContext, manifest, o.verbose, stdout, stderr)
	if err != nil {
		return err
	}
	status := "installed " + kube.PgBackRestVersion + parenthesize(detail)
	if o.wait {
		r.working("pgBackRest plugin", "waiting for the controller")
		if err := waitForDeployment(ctx, cs, kube.PgBackRestNamespace, kube.PgBackRestControllerDeployment,
			"pgBackRest plugin", io.Discard, cnpgWaitTimeout); err != nil {
			return err
		}
		status += ", controller ready"
	}
	r.done("pgBackRest plugin", status)
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

// writeClusterPostgresPlan prints the live plan: the two components this run will apply, what each one
// is for, and the cluster-admin requirement. The requirement is printed on every run, not only on
// failure — a permission error from a partial CRD apply is a poor way to learn what the command
// needed.
//
// The plan leads with the components rather than with the manifest URLs it applies them from
// (issue #461). The URLs are the longest thing on the line, and putting them there pushed the
// version — the value a reader is actually scanning for — past the edge of the terminal. They move
// under --verbose, indented beneath the component they belong to, and the cluster-admin note says so:
// the operator deciding whether to trust this is the one who needs them, and they need to know where
// to look.
func writeClusterPostgresPlan(w io.Writer, verbose bool, manifest string, c controlplane.CloudNativePGCapability,
	plugin controlplane.PgBackRestCapability, certs controlplane.CertManagerCapability) {
	fmt.Fprintln(w, "This will install, on your current kube context:")
	writePlanRows(w, verbose, []planRow{
		cloudNativePGPlanRow(manifest, c),
		pgBackRestPlanRow(plugin, certs),
	})
	fmt.Fprintln(w)
	writeClusterAdminNotice(w, verbose)
}

// cloudNativePGPlanRow describes the operator's line in the live plan, across the three states the
// detection distinguishes: running (nothing to do, and the release named, since a cluster on an older
// operator validates a different schema), CRDs served with no controller behind them (the repair), and
// absent (the install).
func cloudNativePGPlanRow(manifest string, c controlplane.CloudNativePGCapability) planRow {
	switch {
	case c.Ready && c.Version == "":
		return planRow{name: "CloudNativePG", detail: "already running, version unknown; skipped"}
	case c.Ready && c.Version != kube.CNPGVersion:
		return planRow{name: "CloudNativePG", version: c.Version,
			detail: "already running; skipped (Burrow targets " + kube.CNPGVersion + ")"}
	case c.Ready:
		return planRow{name: "CloudNativePG", version: c.Version, detail: "already running; skipped"}
	case c.Present:
		return planRow{name: "CloudNativePG", version: kube.CNPGVersion, url: manifest,
			detail: "re-applied: its CustomResourceDefinitions are installed but no controller is running"}
	default:
		return planRow{name: "CloudNativePG", version: kube.CNPGVersion, url: manifest,
			detail: "runs and manages Postgres instances"}
	}
}

// pgBackRestPlanRow describes the backup plugin's line in the live plan. It leads with what the
// component DOES, because that is what a reader is deciding about, and keeps the product name in
// parentheses, because a second component installed with cluster-admin has to be auditable by the
// name it lands under — the install-phase status line names it the same way (issue #461).
func pgBackRestPlanRow(plugin controlplane.PgBackRestCapability, certs controlplane.CertManagerCapability) planRow {
	switch {
	case plugin.Ready:
		// The plugin's release artifact carries no version Burrow can read back, so a running one is
		// reported as running and nothing more (controlplane.PgBackRestCapability).
		return planRow{name: "Backup support", detail: "already running; skipped"}
	case !certs.Present:
		// The status line below states the whole reason; the plan says which way this run goes and
		// which command changes it, and leaves the paragraph to the component it belongs to.
		return planRow{name: "Backup support",
			detail: "skipped: needs cert-manager, which `burrow cluster ingress install` installs"}
	default:
		return planRow{name: "Backup support", version: kube.PgBackRestVersion,
			url:    kube.PgBackRestManifestURL(kube.PgBackRestVersion),
			detail: "archives write-ahead logs to object storage (pgBackRest plugin)"}
	}
}

// writeClusterPostgresDryRunPlan prints the plan without contacting the cluster, so the install stays
// conditional — there has been no live read to resolve it.
//
// It shows both manifest URLs unconditionally, where the live plan puts them behind --verbose. A dry
// run's whole purpose is reviewing what would be applied before applying it, so the artifacts are the
// point of the output rather than detail underneath it.
func writeClusterPostgresDryRunPlan(w io.Writer, manifest string) {
	fmt.Fprintln(w, "This would install, on your current kube context (dry run):")
	writePlanRows(w, true, []planRow{
		{name: "CloudNativePG", version: kube.CNPGVersion, url: manifest,
			detail: "runs and manages Postgres instances"},
		{name: "Backup support", version: kube.PgBackRestVersion,
			url:    kube.PgBackRestManifestURL(kube.PgBackRestVersion),
			detail: "archives write-ahead logs to object storage (pgBackRest plugin)"},
	})
	fmt.Fprintln(w, "\nEach is skipped if its controller is already running. Backup support also needs")
	fmt.Fprintln(w, "cert-manager, which `burrow cluster ingress install` provides.")
	fmt.Fprintln(w)
	writeClusterAdminNotice(w, true)
}

// clusterAdminNotice states the privilege this command needs and why the agent cannot run it
// (ADR-0066 Consequences). This string surfaces to users, so it stays plain (no em-dashes).
//
// It is where the manifest URLs are pointed at, because it is the one place in this output a reader is
// making a trust decision: applying a remote manifest with cluster-admin. Somebody who wants to read
// what will be fetched should not have to guess that a flag exists.
func writeClusterAdminNotice(w io.Writer, verbose bool) {
	fmt.Fprintln(w, note(w)+"this installs cluster-scoped CustomResourceDefinitions and cluster RBAC, so it needs")
	fmt.Fprintln(w, "  cluster-admin on this kube context. It is an operator step: the agent has no such access")
	if verbose {
		fmt.Fprintln(w, "  and cannot run it.")
		return
	}
	fmt.Fprintln(w, "  and cannot run it. Re-run with --verbose to see the manifests it applies.")
}

// writeClusterPostgresDone prints the closing block: what is ready, what to do next in the order it
// has to be done in, and the honest limit that a whole instance cannot yet be restored from the
// backups this sets up (ADR-0009).
//
// The ordering constraint sits INSIDE the next-steps block rather than below it as a separate note
// (issue #461). It is the reason the two commands are in that order, and printed as a note of its own
// it read as an unrelated caveat that somebody skimming the commands would never connect to them.
// That leaves one advisory here, on the one fact a reader most needs to have read.
func writeClusterPostgresDone(w io.Writer) {
	fmt.Fprintln(w, "\nDone. This cluster can now run Postgres instances.")
	fmt.Fprintln(w, "\nNext, in this order:")
	fmt.Fprintln(w, "  1. burrow config provider add --type s3 ...")
	fmt.Fprintln(w, "  2. burrow addon install postgres [--env <environment>]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  The order matters: an instance archives its write-ahead log and takes its base backups")
	fmt.Fprintln(w, "  only when an object-storage provider is registered BEFORE it is installed. Register one")
	fmt.Fprintln(w, "  first, or re-run addon install afterwards to wire an instance you already have.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, note(w)+"restoring a whole instance from these backups is not built yet. Per-app backup")
	fmt.Fprintln(w, "  and restore (burrow addon backup / restore) work today.")
	fmt.Fprintln(w, "\nCheck it anytime: burrow cluster")
}
