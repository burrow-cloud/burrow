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
	{string(controlplane.AddonPostgres), "cluster-shared PostgreSQL"},
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
			res, err := c.BackupAddon(ctx, args[0], args[1], o.env)
			if err != nil {
				return err
			}
			b := res.Backup
			human := fmt.Sprintf("backed up %q in environment %s (backup %s, status %s)\nstored at %s", b.App, b.Environment, b.ID, b.Status, b.Path)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
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
			backups, err := c.Backups(ctx, args[0], app, o.env)
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
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tAPP\tENV\tCREATED\tSTATUS\tSIZE")
			for _, b := range backups {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", b.ID, b.App, b.Environment, b.CreatedAt, b.Status, b.SizeBytes)
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newAddonRestoreCmd is `burrow addon restore postgres <app> --backup <id>`: restore an app's
// database from a recorded backup, overwriting its live contents (ADR-0032). It is destructive, so it
// is held for confirmation by the addon.restore guardrail by default. Restore is CLI-only — it is
// deliberately absent from the agent surface.
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
			if err := c.RestoreAddon(ctx, args[0], args[1], backup, o.env, confirm); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %q from backup %s\n", args[1], backup)
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&backup, "backup", "", "the backup id to restore (from `burrow addon backups postgres <app>`)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	_ = cmd.MarkFlagRequired("backup")
	return cmd
}

// newAddonAttachCmd is `burrow addon attach postgres <app> [--env]`: give an app its own database on
// the named environment's Postgres instance (ADR-0031/0067 §1). The caller supplies the add-on type,
// the app name, and the environment; burrowd generates the DATABASE_URL server-side and writes it
// into the app's Secret in that environment's namespace — no secret value is printed, returned, or
// carried over the agent control channel. Attach provisions and destroys nothing, so it is allowed
// by default.
func newAddonAttachCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "attach <addon> <app>",
		Short: "Attach an app to an add-on (e.g. give it a Postgres database)",
		Long: "attach gives an app its own database on an environment's Postgres instance: burrowd\n" +
			"provisions an isolated database and login role, generates the connection string server-side,\n" +
			"writes it into the app's Secret as DATABASE_URL, and restarts the app. No secret value is\n" +
			"printed or sent over the agent control channel; only the key name is reported. Re-attaching\n" +
			"rotates the password. Each environment has its own instance, so --env decides which server the\n" +
			"app is given a database on; with several environments registered, naming one is required.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.AttachAddon(ctx, args[0], args[1], o.env)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("attached %q to the %s add-on in environment %s\nwrote the connection string into %s's Secret under key %q (the value is never shown)",
				res.App, res.Addon, res.Environment, res.App, res.SecretKey)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newAddonDetachCmd is `burrow addon detach postgres <app> [--env]`: drop an app's database and role
// from the named environment's instance and remove its DATABASE_URL there. It is destructive (it
// destroys the app's data), so it is held for confirmation by the addon.detach guardrail by default.
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
			if err := c.DetachAddon(ctx, args[0], args[1], o.env, confirm); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "detached %q from the %s add-on\n", args[1], args[0])
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
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
			"writes it into the burrow-credentials Secret; it never travels over the agent control\n" +
			"channel and is never logged.",
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
			// never crosses the agent control channel and is never logged.
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
			a, err := c.InstallAddon(ctx, name, o.env, confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("installed the %s add-on %q (%s)\nin-cluster endpoint: %s\nprovides: %s",
				a.Type, a.Name, a.Image, a.Endpoint, strings.Join(a.Capabilities, ", "))
			return emit(out, o.json, a, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
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

// newAddonListCmd lists the add-ons registered on the cluster AND the volumes an earlier removal
// left behind (ADR-0064 §6). The second half is not decoration: removal keeps the data volume by
// default, so without a listing the only record of a retained claim is the removal output that
// created it — and a bill is a worse way to find out than a listing.
func newAddonListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed add-ons, their capabilities, and volumes kept by an earlier removal",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			listing, err := c.AddonList(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, listing, "")
			}
			if len(listing.Addons) == 0 {
				fmt.Fprintln(out, "No add-ons installed. Install one with `burrow addon install logs`.")
			} else {
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tTYPE\tMODE\tENDPOINT\tCAPABILITIES")
				for _, a := range listing.Addons {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Name, a.Type, a.Mode, a.Endpoint, strings.Join(a.Capabilities, ","))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			return printRetainedVolumes(out, listing.RetainedVolumes)
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// printRetainedVolumes renders the retained-volume section of `addon list`. It is a SEPARATE table
// under its own heading rather than extra rows in the add-on table, because a retained claim is not
// an add-on: it has no workload, no endpoint, and nothing is serving from it. Each row says which
// add-on it belonged to, what it holds, and how big it is, and the section closes with both ways out
// — reinstall to get the data back, or delete the claim to get the storage back (ADR-0064 §1/§6).
//
// Size, not cost. The claim knows its capacity; the price per GiB belongs to the provider, and a
// confident wrong number about money is worse than an honest one about bytes.
func printRetainedVolumes(out io.Writer, vols []client.RetainedVolume) error {
	if len(vols) == 0 {
		return nil
	}
	fmt.Fprintf(out, "\nRetained volumes (%d) — kept by an earlier `addon remove`, still allocated and still billed:\n\n", len(vols))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CLAIM\tADD-ON\tHOLDS\tSIZE\tNAMESPACE")
	adoptable := false
	for _, v := range vols {
		size := v.Size
		if size == "" {
			size = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, v.Addon, v.Role, size, v.Namespace)
		if v.ReinstallAdopts {
			adoptable = true
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	if adoptable {
		fmt.Fprintln(out, "Reinstalling the add-on (`burrow addon install <add-on>`) reuses its data volume and the data\n"+
			"  comes back — for postgres, the databases, roles, and role passwords with it.")
	}
	fmt.Fprintln(out, "To reclaim the storage instead: kubectl -n <namespace> delete pvc <claim>")
	fmt.Fprintln(out, "Nothing deletes these for you.")
	return nil
}

// newAddonRemoveCmd removes an installed add-on. It tears down the add-on's workload and KEEPS its
// data volume; --delete-data is the separate, explicit ask that destroys the data too. The default
// had to be this way round: for `postgres` the data volume holds every attached app's database
// (ADR-0031), and "remove it so I can reinstall it cleanly" is a normal thing to want.
//
// --delete-data lives on the operator CLI only. Destroying application data is a human decision, the
// same line `addon detach` and `addon restore` already sit on — burrow-agent can stop an add-on but
// cannot ask for its data to be destroyed (ADR-0049).
//
// On top of the addon.remove guardrail, --delete-data carries its own human gate (ADR-0064 §2): on a
// terminal the add-on's name has to be typed back after a notice naming what goes, and off a terminal
// it refuses unless --acknowledge-data-loss is passed. --confirm satisfies a guardrail; it is not that
// acknowledgement, because a flag people already reach for reflexively is exactly the habit the gate
// exists to interrupt.
func newAddonRemoveCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm, deleteData, ackDataLoss bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed add-on (keeps its data volume)",
		Long: "remove tears down an add-on's workload — its Deployment, Service, and collectors — and\n" +
			"KEEPS its data volume, so reinstalling the add-on picks the data back up. Pass --delete-data\n" +
			"to destroy the volume as well; for the postgres add-on that destroys every attached app's\n" +
			"database. Recorded backups are on a separate volume and survive either way.\n\n" +
			"--delete-data prints what it would destroy and asks for the add-on's name to be typed back.\n" +
			"With no terminal to type into it refuses rather than proceeding; --acknowledge-data-loss is\n" +
			"how a script says it means it.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			// The off-terminal refusal runs BEFORE anything is contacted, so a script that never
			// said it destroys databases cannot reach the removal call at all (ADR-0064 §2).
			if deleteData && !ackDataLoss && !stdinIsTerminal(cmd.InOrStdin()) {
				return errDeleteDataNeedsTerminal(name)
			}
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			// The typed-name gate goes to stderr so a --json run keeps a clean stdout.
			if deleteData && !ackDataLoss {
				if err := confirmDeleteData(ctx, c, name, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			res, err := c.RemoveAddon(ctx, name, deleteData, confirm)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, res, removeAddonSummary(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	cmd.Flags().BoolVar(&deleteData, "delete-data", false, "also DESTROY the add-on's data volume (for postgres: every attached app's database)")
	cmd.Flags().BoolVar(&ackDataLoss, dataLossAckFlag, false, "acknowledge that --delete-data destroys the add-on's data, so it proceeds without the typed-name prompt (this is how a script asks for it). No shorthand: destroying every attached app's database should not be a single keystroke.")
	return cmd
}

// dataLossAckFlag is the non-interactive acknowledgement --delete-data requires when there is no
// terminal to type the add-on's name into (ADR-0064 §2): "a script that destroys databases should
// have had to say so in the script." It is named for what it acknowledges rather than reusing
// --confirm (which satisfies a guardrail hold), --approve (which approves a billable cloud
// resource), or a generic --yes, because the record's whole point is that a reflexive
// blanket-agreement flag defeats the gate — a reader of the script has to see the words "data loss".
// Like --approve it takes no shorthand.
const dataLossAckFlag = "acknowledge-data-loss"

// errDeleteDataNeedsTerminal is the refusal --delete-data returns off a terminal without the
// acknowledgement flag (ADR-0064 §2). It refuses rather than proceeding, and names both ways out:
// acknowledge the data loss explicitly, or drop the flag and keep the volume.
func errDeleteDataNeedsTerminal(name string) error {
	return fmt.Errorf("--delete-data destroys the add-on's data volume %q and asks for the add-on's name "+
		"to be typed back, which needs an interactive terminal; re-run with --%s to say so explicitly "+
		"in a script, or drop --delete-data to keep the volume", name, dataLossAckFlag)
}

// confirmDeleteData is --delete-data's human gate: it prints what the removal would destroy and
// requires the add-on's name to be typed back before proceeding (ADR-0064 §2). The flag says what
// the operator intends; the typed name says they read what it would do — which a yes/no prompt no
// longer establishes once `-y` is muscle memory. Anything else typed, an empty line, or EOF aborts
// and nothing is removed.
//
// THIS PROMPT IS FOR HUMANS AND IS NOT A SECURITY CONTROL. Anything with a shell can type a word,
// so it adds nothing against an agent: what keeps an agent away from this is that `addon remove`
// is not compiled into burrow-agent at all (ADR-0064 §2, ADR-0049 layer (a), ADR-0065 §2 tier 1).
// That structural absence must never be relaxed on the grounds that "there's a confirmation anyway"
// — the confirmation is a legibility device, not the boundary.
func confirmDeleteData(ctx context.Context, c *client.Client, name string, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, warning(out)+deleteDataConsequence(ctx, c, name))
	fmt.Fprintln(out)
	typed, err := readLine(in, out, fmt.Sprintf("Type the add-on's name (%s) to proceed, or anything else to abort: ", name))
	if err != nil {
		return err
	}
	if typed != name {
		return fmt.Errorf("aborted: %q is not the add-on's name (%s); nothing was removed", typed, name)
	}
	return nil
}

// deleteDataConsequence renders what --delete-data is about to destroy, in the same terms the
// addon.remove guardrail's held confirmation uses (ADR-0064 §3) — the volume by name, and for
// postgres the apps whose databases live in it — so the operator reads one account of the
// consequence rather than two differently-worded ones. "This is destructive" is not consent; "this
// destroys the databases of api and web" is.
//
// Every lookup here is BEST-EFFORT and never blocking (ADR-0064 §3): an add-on is often removed
// precisely because it is wedged, and a control plane that will not answer must degrade to the
// volume-concrete message rather than make a broken add-on unremovable. The volume name is known
// without asking anyone — a stateful add-on's claim carries the add-on's own name.
func deleteDataConsequence(ctx context.Context, c *client.Client, name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--delete-data DESTROYS the data volume %q in namespace %s. This cannot be undone.",
		name, connect.DefaultAddonNamespace)
	if addonTypeOf(ctx, c, name) != string(controlplane.AddonPostgres) {
		return b.String()
	}
	b.WriteString("\n  For the postgres add-on that volume holds every attached app's database")
	if apps := attachedApps(ctx, c); len(apps) > 0 {
		fmt.Fprintf(&b, ": %s", pluralApps(apps))
	}
	fmt.Fprintf(&b, ".\n  The backup volume %q is kept — recorded backups outlive the database they came from.",
		controlplane.PostgresBackupVolume)
	return b.String()
}

// addonTypeOf looks up the registered type of an installed add-on by name, returning "" when the
// listing cannot be read or the name is not registered. Best-effort by contract: an unanswerable
// lookup costs the notice its per-app detail, never the removal.
func addonTypeOf(ctx context.Context, c *client.Client, name string) string {
	addons, err := c.Addons(ctx)
	if err != nil {
		return ""
	}
	for _, a := range addons {
		if a.Name == name {
			return a.Type
		}
	}
	return ""
}

// attachedApps enumerates, best-effort, the apps holding a Burrow-provisioned database on the
// Postgres add-on — the concrete blast radius of a data-deleting removal. Attachment leaves no
// registry row (ADR-0064 §Context): it exists as a database on the instance and as the DATABASE_URL
// KEY in the app's Secret, and the key is the half a CLI can read. Only key NAMES are listed, never
// values (ADR-0028). Any app or listing that will not answer is skipped rather than failing the
// notice.
func attachedApps(ctx context.Context, c *client.Client) []string {
	apps, err := c.Apps(ctx, "")
	if err != nil {
		return nil
	}
	var attached []string
	for _, a := range apps {
		keys, err := c.Secrets(ctx, a.App, "")
		if err != nil {
			continue
		}
		for _, k := range keys {
			if k == databaseURLKey {
				attached = append(attached, a.App)
				break
			}
		}
	}
	sort.Strings(attached)
	return attached
}

// databaseURLKey is the key `addon attach` writes an app's generated connection string under
// (ADR-0031). Only the key name is ever read here; the value stays in the app's Secret.
const databaseURLKey = "DATABASE_URL"

// pluralApps renders an app list as "2 attached apps (api, web)" / "1 attached app (web)", matching
// the phrasing the guardrail's held confirmation uses so the two read as one message.
func pluralApps(apps []string) string {
	noun := "attached apps"
	if len(apps) == 1 {
		noun = "attached app"
	}
	return fmt.Sprintf("%d %s (%s)", len(apps), noun, strings.Join(apps, ", "))
}

// removeAddonSummary renders the human outcome of a removal. It always says what happened to the
// data, because "removed add-on X" alone leaves the one question that matters unanswered — and names
// the retained volumes so the operator can reclaim them deliberately rather than discovering the
// storage later.
func removeAddonSummary(res client.RemoveAddonResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "removed add-on %q\n", res.Name)
	switch {
	case res.DataDeleted:
		b.WriteString("its data volume was DESTROYED\n")
	case res.RetainedDataVolume != "":
		// `burrow addon list` is named here because it is where this volume shows up from now on:
		// the removal output must not be the only record of it (ADR-0064 §1/§6).
		fmt.Fprintf(&b, "kept the data volume %q in namespace %s — reinstalling the add-on reuses it,\n"+
			"  with the data (and, for postgres, the app roles and passwords) intact.\n"+
			"  To reclaim the storage instead: kubectl -n %s delete pvc %s\n"+
			"  `burrow addon list` reports it until then.\n",
			res.RetainedDataVolume, res.Namespace, res.Namespace, res.RetainedDataVolume)
	}
	if res.RetainedBackupVolume != "" {
		fmt.Fprintf(&b, "kept the backup volume %q — recorded backups outlive the database and\n"+
			"  `burrow addon backups postgres` still lists them.\n", res.RetainedBackupVolume)
	}
	if len(res.AttachedApps) > 0 {
		verb := "still hold a DATABASE_URL for this instance and cannot reach a database until it is reinstalled"
		if res.DataDeleted {
			verb = "lost their database; their DATABASE_URL now points at nothing"
		}
		fmt.Fprintf(&b, "%d attached app(s) (%s) %s\n", len(res.AttachedApps), strings.Join(res.AttachedApps, ", "), verb)
	}
	return strings.TrimRight(b.String(), "\n")
}
