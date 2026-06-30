// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
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
			cfg, err := loadRawKubeconfig(kubeconfig)
			if err != nil {
				return err
			}
			writeContextList(cmd.OutOrStdout(), cfg)
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	return cmd
}

// loadRawKubeconfig loads the merged kubeconfig as a raw api.Config (contexts, clusters, current
// context), honoring an explicit path and otherwise the ambient KUBECONFIG / ~/.kube/config.
func loadRawKubeconfig(path string) (*api.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return cfg, nil
}

// writeContextList prints the contexts, sorted by name, marking the current one with a *.
func writeContextList(w io.Writer, cfg *api.Config) {
	if len(cfg.Contexts) == 0 {
		fmt.Fprintln(w, "No contexts found in the kubeconfig.")
		return
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tNAME\tCLUSTER")
	for _, name := range names {
		marker := ""
		if name == cfg.CurrentContext {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", marker, name, cfg.Contexts[name].Cluster)
	}
	_ = tw.Flush()
}
