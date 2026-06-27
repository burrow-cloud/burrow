// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
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
	cmd.AddCommand(newAddonInstallCmd(), newAddonConnectCmd(), newAddonListCmd(), newAddonLogsCmd(), newAddonMetricsCmd(), newAddonRemoveCmd())
	return cmd
}

func newAddonConnectCmd() *cobra.Command {
	o := &commonOpts{}
	var endpoint string
	var auth bool
	cmd := &cobra.Command{
		Use:   "connect <backend>",
		Short: "Register an existing backend you already run (e.g. loki) as a queryable capability",
		Long: "connect registers an adapter to an existing backend you already run (logs → Loki) so\n" +
			"your agent can query it — Burrow deploys nothing and the license bar does not apply, since\n" +
			"it connects rather than distributes. Pass the in-cluster endpoint with --endpoint.\n\n" +
			"For an authenticated backend, pass --auth: you are prompted for a bearer token with the\n" +
			"input hidden, which is written into the burrow-credentials Secret with your kubeconfig and\n" +
			"read by the control plane at query time. The token never travels over the control-plane API\n" +
			"— only the Secret key does.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			backend := args[0]

			// Without --auth the backend is unauthenticated: no Secret is written and only the
			// (empty) key crosses the API.
			if !auth {
				c, err := o.client(ctx)
				if err != nil {
					return err
				}
				a, err := c.ConnectAddon(ctx, backend, endpoint, "")
				if err != nil {
					return err
				}
				return emit(cmd.OutOrStdout(), o.json, a, connectHuman(a, ""))
			}

			// --auth: prompt for the token, write it into burrow-credentials with the developer's
			// kubeconfig (ADR-0017/0023), then record the registry entry naming only the key. If the
			// API call fails after the write, roll the Secret back so a rejected token is not left.
			token, err := readToken(cmd.InOrStdin(), cmd.OutOrStdout(), fmt.Sprintf("Enter the %s bearer token: ", backend))
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("no token provided")
			}
			key := "addon-" + backend

			cs, err := clientset(o.kubeconfig)
			if err != nil {
				return err
			}
			prior, existed, err := readCredential(ctx, cs, o.namespace, key)
			if err != nil {
				return err
			}
			if err := writeCredential(ctx, cs, o.namespace, key, token); err != nil {
				return err
			}
			c, err := o.client(ctx)
			if err != nil {
				restoreCredential(ctx, cs, o.namespace, key, prior, existed)
				return err
			}
			a, err := c.ConnectAddon(ctx, backend, endpoint, key)
			if err != nil {
				restoreCredential(ctx, cs, o.namespace, key, prior, existed)
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, a, connectHuman(a, key))
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "in-cluster host:port of the existing backend (required)")
	cmd.Flags().BoolVar(&auth, "auth", false, "the backend requires a bearer token; prompt for it and store it in the burrow-credentials Secret")
	_ = cmd.MarkFlagRequired("endpoint")
	return cmd
}

// connectHuman is the human-readable confirmation for a connected add-on, noting where an
// authenticated backend's token was stored when a key was used.
func connectHuman(a client.Addon, key string) string {
	human := fmt.Sprintf("connected the %s add-on %q (mode: %s)\nin-cluster endpoint: %s — capabilities: %s",
		a.Type, a.Name, a.Mode, a.Endpoint, strings.Join(a.Capabilities, ", "))
	if key != "" {
		human += fmt.Sprintf("\nbearer token stored in %s under key %q", credentialsSecretName, key)
	}
	return human
}

func newAddonLogsCmd() *cobra.Command {
	o := &commonOpts{}
	var limit int
	var backend string
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
			entries, err := c.QueryLogs(ctx, query, limit, backend)
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
	cmd.Flags().StringVar(&backend, "backend", "", "query a specific backend when more than one serves this capability (e.g. loki, victorialogs, prometheus)")
	return cmd
}

func newAddonMetricsCmd() *cobra.Command {
	o := &commonOpts{}
	var backend string
	cmd := &cobra.Command{
		Use:   "metrics <query>",
		Short: "Query the connected metrics add-on with an instant PromQL query",
		Long: "metrics runs an instant PromQL query against the connected metrics store (Prometheus or\n" +
			"VictoriaMetrics) — e.g. `up`, `rate(http_requests_total[5m])`. Connect one first with\n" +
			"`burrow addon connect prometheus --endpoint <host:port>`.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			samples, err := c.QueryMetrics(ctx, args[0], backend)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, samples, "")
			}
			if len(samples) == 0 {
				fmt.Fprintln(out, "no matching samples")
				return nil
			}
			for _, s := range samples {
				if len(s.Labels) > 0 {
					fmt.Fprintf(out, "%s  %s\n", metricLabels(s.Labels), s.Value)
				} else {
					fmt.Fprintln(out, s.Value)
				}
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&backend, "backend", "", "query a specific backend when more than one serves this capability (e.g. loki, victorialogs, prometheus)")
	return cmd
}

// metricLabels renders a sample's labels in a stable {k="v",...} form for the human-readable listing.
func metricLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
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
