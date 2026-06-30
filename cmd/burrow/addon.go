// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/localconfig"
)

// installableAddon is one row of the `addon install` listing: an add-on name and a one-line
// description.
type installableAddon struct {
	name        string
	description string
}

// installableAddons is the static, compiled-in set of add-ons `burrow addon install <name>` can
// install, in install order. It is intentionally static: the CLI and burrowd ship in lockstep, so
// listing what is installable stays useful even before connecting to a cluster. The descriptions
// mirror the control-plane catalog summaries where they fit on one line.
var installableAddons = []installableAddon{
	{string(controlplane.AddonLogs), "log aggregation (VictoriaLogs)"},
	{string(controlplane.AddonMetrics), "metrics (VictoriaMetrics + a vmagent scraper)"},
	{string(controlplane.AddonCache), "in-memory cache (ValKey)"},
	{string(controlplane.AddonPostgres), "shared PostgreSQL"},
}

// metricsRBACManifest is the per-add-on metrics RBAC template, embedded like the install manifests
// (cmd/burrow/install.go). The CLI applies it kubeconfig-side at install time so burrowd never needs
// RBAC-creation powers (least privilege).
//
//go:embed manifests/addon-metrics-rbac.yaml.tmpl
var metricsRBACManifest string

// metricsRBACTemplate parses the embedded metrics RBAC manifest once at startup.
var metricsRBACTemplate = template.Must(template.New("addon-metrics-rbac").Parse(metricsRBACManifest))

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
	cmd.AddCommand(newAddonInstallCmd(), newAddonConnectCmd(), newAddonAttachCmd(), newAddonDetachCmd(), newAddonBackupCmd(), newAddonBackupsCmd(), newAddonRestoreCmd(), newAddonListCmd(), newAddonLogsCmd(), newAddonMetricsCmd(), newAddonRemoveCmd())
	return cmd
}

// newAddonBackupCmd is `burrow addon backup postgres <app>`: back up an app's database on the
// installed Postgres add-on (ADR-0032). burrowd runs an in-cluster Job that pg_dumps the database to
// the backup PVC and records the backup; no secret value crosses the API. Backup destroys nothing,
// so it is allowed by default.
func newAddonBackupCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "backup <addon> <app>",
		Short: "Back up an app's database (e.g. on the Postgres add-on)",
		Long: "backup runs an in-cluster Job that pg_dumps an app's database on the installed Postgres\n" +
			"add-on to a backup volume and records the backup in the control plane. No secret value crosses\n" +
			"the API — the Job reads the superuser password from the add-on's Secret in-cluster.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.BackupAddon(ctx, args[0], args[1])
			if err != nil {
				return err
			}
			b := res.Backup
			human := fmt.Sprintf("backed up %q (backup %s, status %s)\nstored at %s", b.App, b.ID, b.Status, b.Path)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// newAddonBackupsCmd is `burrow addon backups postgres [<app>]`: list recorded backups, newest
// first. With no app it lists every app's backups. Read-only.
func newAddonBackupsCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "backups <addon> [<app>]",
		Short: "List recorded database backups (id, app, time, size)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := ""
			if len(args) == 2 {
				app = args[1]
			}
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			backups, err := c.Backups(ctx, args[0], app)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, backups, "")
			}
			if len(backups) == 0 {
				fmt.Fprintln(out, "No backups recorded. Create one with `burrow addon backup postgres <app>`.")
				return nil
			}
			fmt.Fprintf(out, "%-26s%-16s%-24s%-12s%s\n", "ID", "APP", "CREATED", "STATUS", "SIZE")
			for _, b := range backups {
				fmt.Fprintf(out, "%-26s%-16s%-24s%-12s%d\n", b.ID, b.App, b.CreatedAt, b.Status, b.SizeBytes)
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// newAddonRestoreCmd is `burrow addon restore postgres <app> --backup <id>`: restore an app's
// database from a recorded backup, overwriting its live contents (ADR-0032). It is destructive, so it
// is held for confirmation by the addon.restore guardrail by default. Restore is CLI-only — there is
// no MCP tool for it.
func newAddonRestoreCmd() *cobra.Command {
	o := &commonOpts{}
	var backup string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "restore <addon> <app> --backup <id>",
		Short: "Restore an app's database from a backup, overwriting its live contents",
		Long: "restore runs an in-cluster Job that pg_restores a recorded backup into an app's database,\n" +
			"replacing its current contents. It is destructive, so it is held for confirmation by the\n" +
			"addon.restore guardrail by default; pass --confirm to proceed.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if backup == "" {
				return errors.New("a backup id is required (--backup <id>)")
			}
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.RestoreAddon(ctx, args[0], args[1], backup, confirm); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %q from backup %s\n", args[1], backup)
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&backup, "backup", "", "the backup id to restore (from `burrow addon backups postgres <app>`)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	_ = cmd.MarkFlagRequired("backup")
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
// confirmation by the addon.detach guardrail by default.
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
		Use:   "install [<name>]",
		Short: "Install a vetted backing service (logs, metrics, cache, postgres)",
		Long: "install deploys the vetted, permissively-licensed default for an add-on (logs is\n" +
			"VictoriaLogs, metrics is VictoriaMetrics) and registers it so your agent can use it. The\n" +
			"install is gated by the addon.install guardrail.\n\n" +
			"Run `burrow addon install` with no name to list the add-ons you can install and which are\n" +
			"already installed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				return listInstallableAddons(ctx, o, out)
			}
			name := args[0]

			// The CLI holds the kubeconfig, so it stages the add-on's per-add-on RBAC kubeconfig-side
			// BEFORE the install API call: burrowd is forbidden from creating RBAC (least privilege),
			// so the grant the add-on needs cannot be minted server-side. Most add-ons need none and
			// this is a no-op.
			kubeContext, controlPlaneNamespace, appNamespace := o.resolveAddonNamespaces()
			if err := ensureAddonRBAC(ctx, name, o.kubeconfig, kubeContext, controlPlaneNamespace, appNamespace, out); err != nil {
				return err
			}

			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			a, err := c.InstallAddon(ctx, name, confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("installed the %s add-on %q (%s)\nin-cluster endpoint: %s\nprovides: %s",
				a.Type, a.Name, a.Image, a.Endpoint, strings.Join(a.Capabilities, ", "))
			return emit(out, o.json, a, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

// listInstallableAddons prints the static list of installable add-ons and, when a cluster is
// reachable, which are already installed. It never fails the listing when Burrow is not installed or
// unreachable: the installable set is compiled in, so it stays useful offline (the INSTALLED column
// blanks to "-" and a hint points at `burrow install`).
func listInstallableAddons(ctx context.Context, o *commonOpts, out io.Writer) error {
	installed := map[string]bool{}
	connected := false
	if c, err := o.client(ctx); err == nil {
		if addons, err := c.Addons(ctx); err == nil {
			connected = true
			for _, a := range addons {
				installed[a.Type] = true
			}
		}
	}

	fmt.Fprintln(out, "Available add-ons:")
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tINSTALLED\tDESCRIPTION")
	for _, ia := range installableAddons {
		mark := "-"
		if connected {
			mark = "no"
			if installed[ia.name] {
				mark = "yes"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", ia.name, mark, ia.description)
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	if !connected {
		fmt.Fprintln(out, "Connect to a cluster to see which are installed (run `burrow install <context>` first).")
	}
	fmt.Fprintln(out, "Install one with `burrow addon install <name>`.")
	return nil
}

// resolveAddonNamespaces resolves the namespaces the per-add-on RBAC targets, reusing the same
// active-environment resolution the API connection uses (localconfig plus the --context/--namespace
// overrides). It returns the kube context to apply against (empty means the kubeconfig's current
// context, exactly as the API connection resolves it), the control-plane namespace, and the app
// namespace where an add-on's app-namespace RBAC (the metrics vmagent's pod-discovery Role) belongs.
func (o *commonOpts) resolveAddonNamespaces() (kubeContext, controlPlaneNamespace, appNamespace string) {
	kubeContext = o.context
	controlPlaneNamespace = o.namespace
	appNamespace = connect.DefaultAppNamespace
	cfg, err := localconfig.Load()
	if err != nil {
		return
	}
	resolved, err := localconfig.Resolve(cfg, o.kubeconfig)
	if err != nil {
		return
	}
	if kubeContext == "" {
		kubeContext = resolved.Context
	}
	if o.namespace == "" || o.namespace == connect.DefaultNamespace {
		controlPlaneNamespace = resolved.ControlPlaneNamespace
	}
	if resolved.Namespace != "" {
		appNamespace = resolved.Namespace
	}
	return
}

// ensureAddonRBAC stages an add-on's per-add-on RBAC kubeconfig-side before the install API call.
// burrowd cannot create RBAC (least privilege), but the CLI has the kubeconfig, so it applies the
// add-on's grant here. Add-ons without per-add-on RBAC (logs, cache, postgres) are a no-op. The
// metrics add-on's vmagent scraper needs a pre-provisioned ServiceAccount plus a pod-discovery
// Role/RoleBinding: render them and server-side-apply through the shared applyFn seam. Apply is
// idempotent, so re-applying an already-present grant is fine and needs no separate presence probe.
func ensureAddonRBAC(ctx context.Context, name, kubeconfig, kubeContext, controlPlaneNamespace, appNamespace string, out io.Writer) error {
	if name != string(controlplane.AddonMetrics) {
		return nil
	}
	var sb strings.Builder
	if err := metricsRBACTemplate.Execute(&sb, struct {
		AddonNamespace        string
		AppNamespace          string
		ControlPlaneNamespace string
	}{
		AddonNamespace:        connect.DefaultAddonNamespace,
		AppNamespace:          appNamespace,
		ControlPlaneNamespace: controlPlaneNamespace,
	}); err != nil {
		return fmt.Errorf("rendering metrics RBAC: %w", err)
	}
	fmt.Fprintln(out, "Preparing metrics RBAC (vmagent scraper)...")
	return applyFn(ctx, kubeconfig, kubeContext, sb.String(), false, out, out)
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
