// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// scanProbeFn probes a single kube context for an installed burrowd in the given control-plane
// namespace, returning the burrowd image or an error: an IsNotFound when nothing is installed, a
// dial/timeout error when the cluster is unreachable. It is a package var so a test can substitute
// a fake probe and exercise `burrow env scan` without a real cluster, the way `burrow version`
// classifies a cluster from the burrowd Deployment image (ADR-0036).
var scanProbeFn = func(ctx context.Context, kubeconfig, kubeContext, namespace string) (string, error) {
	cs, err := clientsetForContext(kubeconfig, kubeContext)
	if err != nil {
		return "", err
	}
	return burrowdImage(ctx, cs, namespace)
}

// newEnvScanCmd walks every kubeconfig context, probes each for an installed Burrow, prints what it
// finds, and registers a local handle for each installed context that has none yet (ADR-0036). It
// reads clusters but mutates only ~/.burrow/config; it never changes a cluster.
func newEnvScanCmd() *cobra.Command {
	var kubeconfig, namespace string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Probe every kube context for an installed Burrow and register the ones it finds",
		Long: "scan walks every context in your kubeconfig and probes each cluster for an installed\n" +
			"Burrow control plane (in the control-plane namespace, default \"burrow\"). It prints a table\n" +
			"of what it finds, then registers a local handle for each installed context that\n" +
			"does not have one yet.\n\n" +
			"It reads clusters but changes only ~/.burrow/config (override with $BURROW_CONFIG); it never\n" +
			"modifies a cluster. To install Burrow into a cluster that has none, use `burrow install`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEnvScan(cmd.Context(), kubeconfig, namespace, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig to scan (default: ambient)")
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "control-plane namespace to probe for burrowd")
	return cmd
}

// scanRow is one context's probe outcome, rendered as a table row. installed drives both the
// VERSION cell and whether scan registers a handle for the context.
type scanRow struct {
	context   string
	cluster   string
	status    string // installed / not installed / unreachable
	version   string // the image tag when installed, the failure reason when unreachable, else "-"
	installed bool
}

// classifyProbe maps a burrowd probe result to the three-way status that both `burrow env scan` and
// the `burrow install` context listing report, the same way `burrow version` classifies a cluster
// (ADR-0036): a clean read is an installed control plane (version carries its image tag), an
// IsNotFound means none is installed, and any other error is an unreachable cluster (version carries
// the failure reason). Factoring it here keeps the two call sites from drifting.
func classifyProbe(img string, perr error) (status, version string, installed bool) {
	switch {
	case perr == nil:
		return "installed", imageTag(img), true
	case apierrors.IsNotFound(perr):
		return "not installed", "-", false
	default:
		return "unreachable", connect.FailureReason(perr), false
	}
}

func runEnvScan(ctx context.Context, kubeconfig, namespace string, w io.Writer) error {
	contexts, err := connect.Contexts(kubeconfig)
	if err != nil {
		return err
	}
	if len(contexts) == 0 {
		fmt.Fprintln(w, "No kube contexts found. Point Burrow at a cluster: set $KUBECONFIG or create ~/.kube/config.")
		return nil
	}

	rows := make([]scanRow, 0, len(contexts))
	for _, c := range contexts {
		probeCtx, cancel := context.WithTimeout(ctx, connect.ProbeTimeout)
		img, perr := scanProbeFn(probeCtx, kubeconfig, c.Name, namespace)
		cancel()
		row := scanRow{context: c.Name, cluster: c.Cluster}
		row.status, row.version, row.installed = classifyProbe(img, perr)
		rows = append(rows, row)
	}
	writeScanTable(w, rows)

	// Register a handle for each installed context that has none yet. Idempotent: a context that
	// already has a handle is left untouched, so re-running scan adds only what is new.
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	installed := 0
	for _, row := range rows {
		if row.installed {
			installed++
		}
	}
	var added []string
	for _, row := range rows {
		if !row.installed || hasHandleForContext(cfg, row.context) {
			continue
		}
		name := uniqueHandleName(cfg, row.context)
		env := localconfig.Environment{
			Name:                  name,
			Context:               row.context,
			ControlPlaneNamespace: namespace,
		}
		// Backfill the scoped agent credential for this handle (ADR-0038 §4): read the existing
		// burrow-agent credential and write the local scoped kubeconfig. Best-effort — a pre-Phase-1
		// install carries none and a joining user may lack read access, so register the handle
		// without a scoped cred rather than fail the scan; the operator can `burrow upgrade` to mint
		// it. Idempotent by construction: a context that already has a handle is skipped above, so a
		// re-scan neither rewrites the kubeconfig nor duplicates the handle.
		if path, agentCtx, jerr := joinAgentCredentialFn(ctx, kubeconfig, row.context, namespace, name); jerr == nil {
			env.AgentKubeconfig = path
			env.AgentContext = agentCtx
		}
		if err := cfg.Add(env); err != nil {
			return err
		}
		added = append(added, name)
	}

	// Close with an outcome-aware message rather than a flat "nothing to register", which reads as a
	// non-sequitur when every context simply has no Burrow installed yet.
	switch {
	case len(added) > 0:
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(w, "\nRegistered %d environment handle(s): %s\n", len(added), strings.Join(added, ", "))
		fmt.Fprintln(w, "See `burrow env list`.")
	case installed > 0:
		fmt.Fprintln(w, "\nAll installed environments are already registered. See `burrow env list`.")
	default:
		fmt.Fprintf(w, "\nNo Burrow control plane found in any context. Install one with `burrow install <context>`,\n"+
			"then re-run scan.\n")
		fmt.Fprintf(w, "(scan probed the %q control-plane namespace; pass --namespace if yours differs.)\n", namespace)
	}
	return nil
}

// writeScanTable prints the probe outcomes as an aligned CONTEXT/CLUSTER/BURROWD/VERSION table.
func writeScanTable(w io.Writer, rows []scanRow) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONTEXT\tCLUSTER\tBURROWD\tVERSION")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.context, r.cluster, r.status, r.version)
	}
	_ = tw.Flush()
}

// hasHandleForContext reports whether the config already has a handle pointing at the kube context,
// so scan does not register a duplicate.
func hasHandleForContext(cfg *localconfig.Config, context string) bool {
	for _, e := range cfg.Environments {
		if e.Context == context {
			return true
		}
	}
	return false
}

// uniqueHandleName returns base if no handle by that name exists, otherwise base-2, base-3, ... so
// scan never collides with an existing handle name (e.g. one a different context already claimed).
func uniqueHandleName(cfg *localconfig.Config, base string) string {
	if _, ok := cfg.Lookup(base); !ok {
		return base
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s-%d", base, i)
		if _, ok := cfg.Lookup(name); !ok {
			return name
		}
	}
}
