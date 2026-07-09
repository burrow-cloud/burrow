// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// This file adds the remaining agent-exposed mutating verbs (ADR-0049 Phase 2b): the routing verbs
// (expose/unexpose, domain add/remove), the add-on operations (install/remove/attach/backup), the
// non-secret config writes (config set/unset), the secret-key removal (secret unset), and the guarded
// destructive delete. Every one funnels through the confirm-flow spine in mutate.go and prints the same
// outcome envelope, so a held or denied operation is surfaced identically to the Phase 2a compute
// verbs. Deliberately ABSENT: there is no `secret set` — a secret VALUE never routes through the agent
// channel (ADR-0029); the human sets secrets with the `burrow` CLI.

// newExposeCmd makes a deployed app reachable from outside the cluster at a hostname (a Service and an
// Ingress). Public exposure trips the app.expose_public guardrail, held for confirmation by default.
func newExposeCmd() *cobra.Command {
	o := &connOpts{}
	var host, issuer string
	var port int
	var tls, confirm bool
	cmd := &cobra.Command{
		Use:   "expose <app> --host <host> --port <port>",
		Short: "Make a deployed app reachable from outside the cluster at a hostname",
		Long: "Make a deployed application reachable from outside the cluster at a hostname, by creating a\n" +
			"Service and an Ingress. Reachability also needs an ingress controller and DNS pointing the host\n" +
			"at the cluster (use domain add and reachability). Requesting TLS needs cert-manager.\n\n" +
			"Public exposure trips the app.expose_public guardrail, held for confirmation by default. When\n" +
			"held, the outcome says so — relay it and re-run with --confirm ONLY after the human approves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if host == "" {
				return errors.New("--host is required")
			}
			if port == 0 {
				return errors.New("--port is required")
			}
			return o.mutate(cmd, "expose", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Expose(ctx, args[0], env, host, int32(port), tls, issuer, confirm)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&host, "host", "", "external hostname to route to the app (required)")
	cmd.Flags().IntVar(&port, "port", 0, "the app's container port to forward to (required)")
	cmd.Flags().BoolVar(&tls, "tls", false, "request an HTTPS certificate for the host via cert-manager")
	cmd.Flags().StringVar(&issuer, "tls-issuer", "letsencrypt", "cert-manager ClusterIssuer to request the certificate from when --tls is set")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a public exposure a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newUnexposeCmd removes an app's exposure (its Service and Ingress). It does not affect the running
// workload and is not guarded, but still prints the outcome envelope for a uniform agent contract.
func newUnexposeCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "unexpose <app>",
		Short: "Remove an app's exposure (its Service and Ingress); the workload keeps running",
		Long: "Remove an application's exposure — its Service and Ingress — so it is no longer served at its\n" +
			"hostname. This does not affect the running workload; it stays deployed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "unexpose", func(ctx context.Context, c *client.Client, env string) (any, error) {
				if err := c.Unexpose(ctx, args[0], env); err != nil {
					return nil, err
				}
				return map[string]any{"app": args[0], "exposed": false}, nil
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newDomainCmd groups the DNS-record verbs (add/remove). Domains are a cluster-level concern, not
// per-environment, so the subcommands bind only the connection flags, not --env.
func newDomainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domain",
		Short: "Manage public DNS records at a configured provider (add, remove)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newDomainAddCmd(), newDomainRemoveCmd())
	return cmd
}

// newDomainAddCmd points a hostname at the cluster by creating or updating a DNS record at a
// configured provider. Public DNS writes trip the dns.write guardrail, held for confirmation by default.
func newDomainAddCmd() *cobra.Command {
	o := &connOpts{}
	var provider, address, app string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "add <host>",
		Short: "Point a hostname at the cluster by creating or updating a DNS record",
		Long: "Point a hostname at the cluster by creating or updating a DNS record at a configured provider\n" +
			"(e.g. DigitalOcean or Cloudflare). Give the cluster's external address with --address (an IPv4\n" +
			"address becomes an A record, a hostname a CNAME), or name an exposed app with --app to read its\n" +
			"external address from its ingress. The provider must already be configured by the operator.\n\n" +
			"Public DNS writes trip the dns.write guardrail, held for confirmation by default. When held, the\n" +
			"outcome says so — relay it and re-run with --confirm ONLY after the human approves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "domain_add", func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.AddDomain(ctx, args[0], provider, address, app, confirm)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	cmd.Flags().StringVar(&provider, "provider", "", "configured DNS provider to write the record at (default: the only one configured)")
	cmd.Flags().StringVar(&address, "address", "", "the cluster's external IP or hostname to point at (or use --app)")
	cmd.Flags().StringVar(&app, "app", "", "an exposed app whose external address to point at (instead of --address)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a public DNS write a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newDomainRemoveCmd removes the DNS record a configured provider holds for a hostname. Deleting a
// public DNS record trips the dns.delete guardrail, held for confirmation by default.
func newDomainRemoveCmd() *cobra.Command {
	o := &connOpts{}
	var provider string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "remove <host>",
		Short: "Remove the DNS record a configured provider holds for a hostname",
		Long: "Remove the DNS record a configured provider holds for a hostname. Deleting a public DNS record\n" +
			"trips the dns.delete guardrail, held for confirmation by default. When held, the outcome says so\n" +
			"— relay it and re-run with --confirm ONLY after the human approves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "domain_remove", func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.RemoveDomain(ctx, args[0], provider, confirm)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	cmd.Flags().StringVar(&provider, "provider", "", "configured DNS provider holding the record (default: the only one configured)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a public DNS delete a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newAddonCmd groups the add-on operations exposed to the agent (install/remove/attach/backup). Add-ons
// are a cluster-level concern, so the subcommands bind only the connection flags, not --env.
func newAddonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addon",
		Short: "Operate the cluster's backing-service add-ons (install, remove, attach, backup)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newAddonInstallCmd(), newAddonRemoveCmd(), newAddonAttachCmd(), newAddonBackupCmd())
	return cmd
}

// newAddonInstallCmd installs a vetted, self-hostable backing service for a capability (e.g. logs →
// VictoriaLogs) and registers it as queryable. Guarded by addon.install, held for confirmation by default.
func newAddonInstallCmd() *cobra.Command {
	o := &connOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "install <capability>",
		Short: "Install a vetted backing service for a capability (e.g. logs, metrics) and register it",
		Long: "Install a vetted, self-hostable backing service for a capability (logs → VictoriaLogs,\n" +
			"metrics → VictoriaMetrics) and register it as queryable, in one step.\n\n" +
			"Guarded by the addon.install guardrail, held for confirmation by default. When held, the outcome\n" +
			"says so — relay it and re-run with --confirm ONLY after the human approves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "addon_install", func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.InstallAddon(ctx, args[0], confirm)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an add-on install a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newAddonRemoveCmd removes an installed add-on by name. Guarded by addon.remove (removing a backing
// service can break dependent apps), held for confirmation by default.
func newAddonRemoveCmd() *cobra.Command {
	o := &connOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed add-on by name",
		Long: "Remove an installed add-on instance by name. Guarded by the addon.remove guardrail, held for\n" +
			"confirmation by default (removing a backing service can break dependent apps). When held, the\n" +
			"outcome says so — relay it and re-run with --confirm ONLY after the human approves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "addon_remove", func(ctx context.Context, c *client.Client, _ string) (any, error) {
				if err := c.RemoveAddon(ctx, args[0], confirm); err != nil {
					return nil, err
				}
				return map[string]any{"removed": args[0]}, nil
			})
		},
	}
	bindConn(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an add-on removal a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newAddonAttachCmd gives an app its own database on the installed Postgres add-on and wires it in
// (ADR-0031). No secret crosses this channel: burrowd generates the connection string server-side and
// writes it into the app's Secret; the result carries only the KEY name (DATABASE_URL), never the value.
// Attach provisions — it destroys nothing — so it is not guarded.
func newAddonAttachCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "attach <addon> <app>",
		Short: "Give an app its own database on the installed Postgres add-on and wire it in",
		Long: "Give an application its own database on the installed Postgres add-on and wire it in. You supply\n" +
			"only the add-on type (\"postgres\") and the app name — NO secret. Burrow generates the database,\n" +
			"role, and connection string server-side and writes it into the app's Secret as DATABASE_URL; the\n" +
			"value is never returned or shown. Re-attaching rotates the password. The result carries only the\n" +
			"app, the add-on, and the KEY name — never the value. Attach is not guarded (it destroys nothing).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "addon_attach", func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.AttachAddon(ctx, args[0], args[1])
			})
		},
	}
	bindConn(cmd.Flags(), o)
	return cmd
}

// newAddonBackupCmd backs up an app's database on the installed Postgres add-on (ADR-0032). No secret
// crosses this channel; the result is the recorded backup row (id, app, path, size, status), never a
// credential. Backup destroys nothing, so it is not guarded.
func newAddonBackupCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "backup <addon> <app>",
		Short: "Back up an app's database on the installed Postgres add-on",
		Long: "Back up an application's database on the installed Postgres add-on. You supply only the add-on\n" +
			"type (\"postgres\") and the app name — NO secret. Burrow runs an in-cluster Job that dumps the\n" +
			"database to a backup volume and records the backup; no credential crosses this channel or appears\n" +
			"in the result. The result is the recorded backup (id, app, path, size, status). Backup destroys\n" +
			"nothing, so it is not guarded. To RESTORE a backup (which overwrites live data), the human runs\n" +
			"`burrow addon restore postgres <app> --backup <id>` — restore is CLI-only.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "addon_backup", func(ctx context.Context, c *client.Client, _ string) (any, error) {
				return c.BackupAddon(ctx, args[0], args[1])
			})
		},
	}
	bindConn(cmd.Flags(), o)
	return cmd
}

// newConfigSetCmd sets (upserts) one NON-SECRET config var for an app. Not guarded. For secret values
// there is deliberately no agent verb — a secret value never routes through the agent (ADR-0029).
func newConfigSetCmd() *cobra.Command {
	o := &connOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "set <app> KEY=VALUE",
		Short: "Set (upsert) a non-secret config var for an app",
		Long: "Set (upsert) a NON-SECRET config var for an app, sourced into the workload at deploy time. By\n" +
			"default the running app is rolled so it picks the change up; pass --no-restart to only persist it\n" +
			"and let it land on the next deploy. For SECRETS, do not use config — config vars are non-secret,\n" +
			"and a secret value never routes through this channel (the human sets secrets with the burrow CLI).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value, found := strings.Cut(args[1], "=")
			if !found || key == "" {
				return fmt.Errorf("expected KEY=VALUE, got %q", args[1])
			}
			return o.mutate(cmd, "config_set", func(ctx context.Context, c *client.Client, env string) (any, error) {
				if err := c.SetConfig(ctx, args[0], env, key, value, noRestart); err != nil {
					return nil, err
				}
				return map[string]any{"app": args[0], "key": key}, nil
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the change without rolling the running workload; it lands on the next deploy")
	return cmd
}

// newConfigUnsetCmd removes one NON-SECRET config var from an app. Not guarded.
func newConfigUnsetCmd() *cobra.Command {
	o := &connOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "unset <app> KEY",
		Short: "Remove a non-secret config var from an app",
		Long: "Remove a NON-SECRET config var from an app. By default the running app is rolled so it drops\n" +
			"the value; pass --no-restart to only persist the removal and let it land on the next deploy.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "config_unset", func(ctx context.Context, c *client.Client, env string) (any, error) {
				if err := c.UnsetConfig(ctx, args[0], env, args[1], noRestart); err != nil {
					return nil, err
				}
				return map[string]any{"app": args[0], "key": args[1]}, nil
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the removal without rolling the running workload; it lands on the next deploy")
	return cmd
}

// newSecretUnsetCmd removes one secret key from an app's per-app Secret. Removing a key carries NO
// value, so it is allowed over the agent channel — unlike SETTING a secret, which has no agent verb
// (a secret value never routes through the agent; the human sets secrets with the burrow CLI, ADR-0029).
func newSecretUnsetCmd() *cobra.Command {
	o := &connOpts{}
	var noRestart bool
	cmd := &cobra.Command{
		Use:   "unset <app> KEY",
		Short: "Remove a secret from an app by KEY (no value crosses the agent channel)",
		Long: "Remove a secret environment variable from an app by KEY. Removing a key carries no value, so it\n" +
			"is allowed here. By default the running app is rolled so it drops the value; pass --no-restart to\n" +
			"only persist the removal and let it land on the next deploy.\n\n" +
			"There is deliberately no `secret set`: a secret VALUE never routes through the agent channel. To\n" +
			"set a secret, the human runs `burrow app secret set <app> KEY=VALUE` at their own terminal.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "secret_unset", func(ctx context.Context, c *client.Client, env string) (any, error) {
				if err := c.UnsetSecret(ctx, args[0], env, args[1], noRestart); err != nil {
					return nil, err
				}
				return map[string]any{"app": args[0], "key": args[1]}, nil
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "persist the removal without rolling the running workload; it lands on the next deploy")
	return cmd
}

// newDeleteCmd deletes an app entirely — its workload, routing, and release history. It is destructive
// but guarded by app.delete, held for confirmation by default, so it flows through the confirm envelope
// like any other held op and a human approves every deletion.
func newDeleteCmd() *cobra.Command {
	o := &connOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "delete <app>",
		Short: "Delete an app entirely (its workload, routing, and release history)",
		Long: "Delete an application entirely: its workload, its routing (Service and Ingress), and its\n" +
			"recorded release history, so it disappears from the apps listing and from status. This is\n" +
			"destructive and irreversible.\n\n" +
			"Guarded by the app.delete guardrail, held for confirmation by default. When held, the outcome\n" +
			"says so — relay it and re-run with --confirm ONLY after the human explicitly approves. Never\n" +
			"self-confirm a deletion.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "delete", func(ctx context.Context, c *client.Client, env string) (any, error) {
				if err := c.DeleteApp(ctx, args[0], env, confirm); err != nil {
					return nil, err
				}
				return map[string]any{"deleted": args[0]}, nil
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm the delete the guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}
