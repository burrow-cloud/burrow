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
	cmd.AddCommand(newAddonInstallCmd(), newAddonConnectCmd(), newAddonAttachCmd(), newAddonDetachCmd(), newAddonListCmd(), newAddonLogsCmd(), newAddonMetricsCmd(), newAddonRemoveCmd())
	return cmd
}

// newAddonAttachCmd is `burrow addon attach postgres <app>`: give an app its own database on the
// installed Postgres add-on (ADR-0031). The agent supplies only the add-on type and app name;
// burrowd generates the DATABASE_URL server-side and writes it into the app's Secret — no secret
// value is printed, returned, or carried over MCP. Attach provisions and destroys nothing, so it is
// allowed by default.
func newAddonAttachCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "attach <addon> <app>",
		Short: "Attach an app to an add-on (e.g. give it a Postgres database)",
		Long: "attach gives an app its own database on the installed Postgres add-on: burrowd provisions\n" +
			"an isolated database and login role, generates the connection string server-side, writes it\n" +
			"into the app's Secret as DATABASE_URL, and restarts the app. No secret value is printed or\n" +
			"sent over MCP — only the key name is reported. Re-attaching rotates the password.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.AttachAddon(ctx, args[0], args[1])
			if err != nil {
				return err
			}
			human := fmt.Sprintf("attached %q to the %s add-on\nwrote the connection string into %s's Secret under key %q (the value is never shown)",
				res.App, res.Addon, res.App, res.SecretKey)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// newAddonDetachCmd is `burrow addon detach postgres <app>`: drop an app's database and role and
// remove its DATABASE_URL. It is destructive (it destroys the app's data), so it is held for
// confirmation by the addon_detach guardrail by default.
func newAddonDetachCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "detach <addon> <app>",
		Short: "Detach an app from an add-on, destroying its data (e.g. drop its Postgres database)",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.DetachAddon(ctx, args[0], args[1], confirm); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "detached %q from the %s add-on\n", args[1], args[0])
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
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
			"input hidden. The token travels over burrowd's authenticated control-plane API (TLS), which\n" +
			"writes it into the burrow-credentials Secret (ADR-0030); it never travels over MCP and is\n" +
			"never logged.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			backend := args[0]

			// Without --auth the backend is unauthenticated: no token and no key cross the API.
			if !auth {
				c, err := o.client(ctx)
				if err != nil {
					return err
				}
				a, err := c.ConnectAddon(ctx, backend, endpoint, "", "")
				if err != nil {
					return err
				}
				return emit(cmd.OutOrStdout(), o.json, a, connectHuman(a, ""))
			}

			// --auth: prompt for the token and send it to burrowd over its authenticated
			// control-plane API (TLS). burrowd writes it into burrow-credentials under the key and
			// records the registry entry (ADR-0030). The token travels only in the request body; it
			// never crosses MCP and is never logged.
			token, err := readToken(cmd.InOrStdin(), cmd.OutOrStdout(), fmt.Sprintf("Enter the %s bearer token: ", backend))
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("no token provided")
			}
			key := "addon-" + backend

			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			a, err := c.ConnectAddon(ctx, backend, endpoint, key, token)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, a, connectHuman(a, key))
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "in-cluster host:port of the existing backend (required)")
	cmd.Flags().BoolVar(&auth, "auth", false, "the backend requires a bearer token; prompt for it and send it over the control-plane API to be stored in the burrow-credentials Secret")
	_ = cmd.MarkFlagRequired("endpoint")
	return cmd
}

// connectHuman is the human-readable confirmation for a connected add-on, noting where an
// authenticated backend's token was stored when a key was used.
func connectHuman(a client.Addon, key string) string {
	human := fmt.Sprintf("connected the %s add-on %q (mode: %s)\nin-cluster endpoint: %s — capabilities: %s",
		a.Type, a.Name, a.Mode, a.Endpoint, strings.Join(a.Capabilities, ", "))
	if key != "" {
		human += fmt.Sprintf("\nbearer token stored in burrow-credentials under key %q", key)
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
