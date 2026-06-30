// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// burrowdStatus is the outcome of probing one kubeconfig context for burrowd: whether the
// control plane is installed there, absent, or the cluster could not be reached.
type burrowdStatus int

const (
	statusInstalled burrowdStatus = iota
	statusNotInstalled
	statusUnreachable
)

// scanProbe is what a single context probe reports: the install status, the control-plane
// version (the burrowd image tag) when installed, and a short reason when unreachable.
type scanProbe struct {
	status  burrowdStatus
	version string
	detail  string
}

// probeBurrowdFn is the seam tests replace to drive `burrow env scan` without a cluster. The
// real implementation reaches each context's API server best-effort and reads the burrowd
// Deployment image, reusing the same classification as `burrow version`.
var probeBurrowdFn = probeBurrowd

// probeBurrowd reads the burrowd Deployment image in namespace through the given kubeconfig
// context, classifying the result the way connect does: a NotFound means burrowd is not
// installed, a dial/DNS/timeout error means the cluster is unreachable, and a found image
// yields the installed version (its tag). It is best effort and never returns an error: a
// scan over many contexts must tolerate some being down.
func probeBurrowd(ctx context.Context, kubeconfig, kubeContext, namespace string) scanProbe {
	cctx, cancel := context.WithTimeout(ctx, connect.ProbeTimeout)
	defer cancel()
	cs, err := clientsetForContext(kubeconfig, kubeContext)
	if err != nil {
		return scanProbe{status: statusUnreachable, detail: connect.FailureReason(err)}
	}
	img, err := burrowdImage(cctx, cs, namespace)
	switch {
	case err == nil:
		return scanProbe{status: statusInstalled, version: imageTag(img)}
	case apierrors.IsNotFound(err):
		return scanProbe{status: statusNotInstalled}
	default:
		return scanProbe{status: statusUnreachable, detail: connect.FailureReason(err)}
	}
}

// newEnvScanCmd walks every kubeconfig context, probes each for burrowd, prints what it finds,
// and registers a local handle for each context that has burrowd installed but no handle yet
// (ADR-0036). It reads clusters but mutates only the client-side config. It lives under `env`
// alongside the other selector verbs rather than as a separate top-level `scan`.
func newEnvScanCmd() *cobra.Command {
	var kubeconfig, namespace string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan kubeconfig contexts for burrowd and register a handle for each install",
		Long: "scan walks every kubeconfig context, probes each for an installed Burrow control plane\n" +
			"(burrowd in the control-plane namespace), and prints whether burrowd is installed, not\n" +
			"installed, or unreachable, with its version when installed. It then registers a local\n" +
			"environment handle for each context that has burrowd but no handle yet (ADR-0036). It\n" +
			"reads your clusters but only ever writes the client-side config; it is safe to re-run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEnvScan(cmd.Context(), kubeconfig, namespace, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().StringVar(&namespace, "control-plane-namespace", localconfig.DefaultControlPlaneNamespace, "control-plane namespace burrowd runs in")
	return cmd
}

func runEnvScan(ctx context.Context, kubeconfig, namespace string, w io.Writer) error {
	contexts, err := connect.Contexts(kubeconfig)
	if err != nil {
		return err
	}
	if len(contexts) == 0 {
		fmt.Fprintln(w, "No contexts found in the kubeconfig.")
		return nil
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONTEXT\tCLUSTER\tBURROWD\tVERSION")
	var installed []connect.Context
	for _, c := range contexts {
		p := probeBurrowdFn(ctx, kubeconfig, c.Name, namespace)
		version := "-"
		if p.version != "" {
			version = p.version
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, c.Cluster, statusLabel(p), version)
		if p.status == statusInstalled {
			installed = append(installed, c)
		}
	}
	_ = tw.Flush()

	// Register a handle, named for the context, for each installed context that has none yet.
	// Idempotent: a context already covered by a handle is left alone, and a name already taken
	// by an unrelated handle is skipped rather than clobbered.
	var added []string
	for _, c := range installed {
		if handleForContext(cfg, c.Name) {
			continue
		}
		if _, taken := cfg.Lookup(c.Name); taken {
			continue
		}
		env := localconfig.Environment{Name: c.Name, Context: c.Name}
		if namespace != localconfig.DefaultControlPlaneNamespace {
			env.ControlPlaneNamespace = namespace
		}
		if err := cfg.Add(env); err != nil {
			return err
		}
		added = append(added, c.Name)
	}

	fmt.Fprintln(w)
	if len(added) == 0 {
		fmt.Fprintln(w, "No new environments to register.")
		return nil
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	for _, name := range added {
		fmt.Fprintf(w, "Registered environment %q for context %q.\n", name, name)
	}
	return nil
}

// statusLabel renders a probe outcome for the BURROWD column.
func statusLabel(p scanProbe) string {
	switch p.status {
	case statusInstalled:
		return "installed"
	case statusNotInstalled:
		return "not installed"
	default:
		if p.detail != "" {
			return "unreachable (" + p.detail + ")"
		}
		return "unreachable"
	}
}

// handleForContext reports whether the config already has a handle pointing at the context.
func handleForContext(cfg *localconfig.Config, context string) bool {
	for _, e := range cfg.Environments {
		if e.Context == context {
			return true
		}
	}
	return false
}
