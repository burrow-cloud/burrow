// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/connect"
)

// newContextCmd groups read-only commands over the kubeconfig contexts. A kubeconfig context is a
// cluster, and each cluster runs its own burrowd, so the contexts are the environments a developer
// can target with the global --context flag (ADR-0035 phase 1). It is named `context` rather than
// `env` to avoid colliding with `app env` (application environment variables).
func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "List the kubeconfig contexts (clusters) you can target",
		Long: "context lists the kubeconfig contexts, each a cluster running its own burrowd. Target a\n" +
			"specific one with the global --context flag, e.g. `burrow --context prod-cluster app status\n" +
			"web`. It is read-only and changes nothing.",
	}
	cmd.AddCommand(newContextListCmd())
	return cmd
}

// newContextListCmd lists the kubeconfig context names and marks the current one. It honors
// --kubeconfig but needs no control-plane connection, so it does not use commonOpts.
func newContextListCmd() *cobra.Command {
	var kubeconfig string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the kubeconfig contexts and mark the current one",
		Long: "list reads your kubeconfig and prints its contexts, marking the current one with a *.\n" +
			"Each context is a cluster you can operate by passing its name to the global --context flag.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			contexts, err := connect.Contexts(kubeconfig)
			if err != nil {
				return err
			}
			writeContextList(cmd.OutOrStdout(), contexts)
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	return cmd
}

// writeContextList prints the contexts (already sorted by connect.Contexts), marking the current
// one with a *.
func writeContextList(w io.Writer, contexts []connect.Context) {
	if len(contexts) == 0 {
		fmt.Fprintln(w, "No contexts found in the kubeconfig.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tNAME\tCLUSTER")
	for _, c := range contexts {
		marker := ""
		if c.Current {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", marker, c.Name, c.Cluster)
	}
	_ = tw.Flush()
}
