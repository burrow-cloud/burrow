// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newAddonCmd groups the building-block backing services Burrow installs and operates — vetted,
// self-hostable add-ons like logs (ADR-0025/0026). `install` deploys a vetted default and
// registers it as a capability the agent can query; `connect` (later) adapts an existing backend.
func newAddonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addon",
		Short: "Install and manage backing services (logs, metrics, …)",
		Long: "addon installs and operates vetted, self-hostable backing services on your cluster —\n" +
			"`addon install logs` stands up log aggregation and registers it as a capability your\n" +
			"agent can query. Every install/remove is gated by a guardrail.",
	}
	cmd.AddCommand(newAddonInstallCmd(), newAddonListCmd(), newAddonLogsCmd(), newAddonRemoveCmd())
	return cmd
}

func newAddonLogsCmd() *cobra.Command {
	o := &commonOpts{}
	var limit int
	cmd := &cobra.Command{
		Use:   "logs [query]",
		Short: "Query the installed logs add-on (LogsQL; empty matches everything)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			entries, err := c.QueryLogs(ctx, query, limit)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, entries, "")
			}
			if len(entries) == 0 {
				fmt.Fprintln(out, "no matching log records")
				return nil
			}
			for _, e := range entries {
				if e.Pod != "" {
					fmt.Fprintf(out, "%s  %s  %s\n", e.Time, e.Pod, e.Message)
				} else {
					fmt.Fprintf(out, "%s  %s\n", e.Time, e.Message)
				}
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum records to return (default 200)")
	return cmd
}

func newAddonInstallCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "install <capability>",
		Short: "Install a vetted backing service for a capability (e.g. logs)",
		Long: "install deploys the vetted, permissively-licensed default for a capability (logs →\n" +
			"VictoriaLogs) and registers it as a capability your agent can query — install and\n" +
			"connect in one step. Held for confirmation by the addon_install guardrail.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			a, err := c.InstallAddon(ctx, args[0], confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("installed the %s add-on %q (%s)\nin-cluster endpoint: %s — capabilities: %s",
				a.Type, a.Name, a.Image, a.Endpoint, strings.Join(a.Capabilities, ", "))
			return emit(cmd.OutOrStdout(), o.json, a, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

func newAddonListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed add-ons and their capabilities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			addons, err := c.Addons(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, addons, "")
			}
			if len(addons) == 0 {
				fmt.Fprintln(out, "No add-ons installed. Install one with `burrow addon install logs`.")
				return nil
			}
			fmt.Fprintf(out, "%-16s%-10s%-12s%-30s%s\n", "NAME", "TYPE", "MODE", "ENDPOINT", "CAPABILITIES")
			for _, a := range addons {
				fmt.Fprintf(out, "%-16s%-10s%-12s%-30s%s\n", a.Name, a.Type, a.Mode, a.Endpoint, strings.Join(a.Capabilities, ","))
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

func newAddonRemoveCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed add-on",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.RemoveAddon(ctx, args[0], confirm); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed add-on %q\n", args[0])
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}
