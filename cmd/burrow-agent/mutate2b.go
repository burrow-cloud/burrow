// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// This file adds the remaining agent-exposed mutating verbs (ADR-0049 Phase 2b): the routing verbs
// (publish/unpublish, domain add/remove), the add-on operations (install/remove/attach/backup), the
// non-secret config writes (config set/unset), the secret-key removal (secret unset), and the guarded
// destructive delete. Every one funnels through the confirm-flow spine in mutate.go and prints the same
// outcome envelope, so a held or denied operation is surfaced identically to the Phase 2a compute
// verbs. Deliberately ABSENT: there is no `secret set` — a secret VALUE never routes through the agent
// channel (ADR-0029); the human sets secrets with the `burrow` CLI.

// newPublishCmd makes a deployed app reachable at a hostname over HTTPS in ONE operation: the
// Service and Ingress, the DNS record when a provider is configured, the pre-flight that proves the
// host reaches this cluster, the certificate, and the wait for it (ADR-0041 §3).
//
// It carries the alias `expose`, and that is deliberate rather than tidiness. `expose` used to be
// this surface's only routing verb and it did strictly less — a Service and an Ingress on port 80,
// reported as executed with an `http://` URL. On an HSTS-preloaded domain such as `.dev` that URL
// does not open in any browser, so an agent relayed a success for something unusable (issue #476).
// Retiring the name outright would answer an agent that had been told to use it with
// `unknown command`, the dead end ADR-0065 §5 says pushes an agent to route around the control
// channel; keeping it pointed at the whole operation means an agent that knows the old verb gets
// the complete result instead of the partial one.
//
// Public exposure trips the app.expose_public guardrail and the DNS write trips dns.write, each
// held for confirmation by default; the operation performs no guardrail evaluation of its own.
func newPublishCmd() *cobra.Command {
	o := &connOpts{}
	var host, issuer, provider string
	var port int
	var tls, noDNS, confirm bool
	cmd := &cobra.Command{
		Use:     "publish <app> --host <host> --port <port>",
		Aliases: []string{"expose"},
		Short:   "Make a deployed app reachable at a hostname over HTTPS (routing, DNS, and the certificate)",
		Long: "Make a deployed application reachable at a hostname, as ONE operation: create its Service and\n" +
			"Ingress, write the DNS record when a provider is configured, confirm the host actually reaches\n" +
			"this cluster, request the HTTPS certificate, and wait for it to issue.\n\n" +
			"TLS IS ON BY DEFAULT. --tls=false publishes plain HTTP and is REFUSED for a host on an\n" +
			"HSTS-preloaded domain such as .dev, where a browser refuses http:// before it sends a request —\n" +
			"an http:// URL there is not a working result to report.\n\n" +
			"The certificate is requested only after the host is confirmed to resolve to this cluster and to\n" +
			"answer on port 80, so a hostname that is not pointed yet does not consume the certificate\n" +
			"authority's rate limit on an order that cannot complete.\n\n" +
			"READ THE RESULT BEFORE REPORTING SUCCESS. `reachable` is the verdict; when it is false the app\n" +
			"is NOT live yet, `blocked_on` names the link it is waiting on and `next` names the action.\n\n" +
			"`expose` is an alias for this command and does the same whole operation.\n\n" +
			"Public exposure trips the app.expose_public guardrail and the DNS write trips dns.write, both\n" +
			"held for confirmation by default. When held, the outcome says so — relay it and re-run with\n" +
			"--confirm ONLY after the human approves.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if host == "" {
				return errors.New("--host is required")
			}
			if port == 0 {
				return errors.New("--port is required")
			}
			return o.mutate(cmd, "publish", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Publish(ctx, args[0], client.PublishRequest{
					Env: env, Host: host, Port: int32(port), NoTLS: !tls, Issuer: issuer,
					SkipDNS: noDNS, Provider: provider, Confirm: confirm,
				})
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&host, "host", "", "external hostname to route to the app (required)")
	cmd.Flags().IntVar(&port, "port", 0, "the app's container port to forward to (required)")
	cmd.Flags().BoolVar(&tls, "tls", true, "request an HTTPS certificate for the host via cert-manager (--tls=false publishes plain HTTP)")
	cmd.Flags().StringVar(&issuer, "tls-issuer", "letsencrypt", "cert-manager ClusterIssuer to request the certificate from")
	cmd.Flags().BoolVar(&noDNS, "no-dns", false, "leave DNS alone; publish only routes the host and waits for what already points at the cluster")
	cmd.Flags().StringVar(&provider, "provider", "", "configured DNS provider to write the record at (default: the only one configured)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a public exposure or DNS write a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newUnpublishCmd removes an app's routing (its Service and Ingress). It does not affect the running
// workload and is not guarded, but still prints the outcome envelope for a uniform agent contract.
// It carries the alias `unexpose` for the same reason publish carries `expose`, and it leaves any
// DNS record alone — removing one is `domain remove`, guarded separately.
func newUnpublishCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:     "unpublish <app>",
		Aliases: []string{"unexpose"},
		Short:   "Stop serving an app at its hostname (removes its Service and Ingress); the workload keeps running",
		Long: "Stop serving an application at its hostname by removing its Service and Ingress. This does not\n" +
			"affect the running workload; it stays deployed. Any DNS record for the host is left alone —\n" +
			"remove it with `domain remove`, which is guarded separately.\n\n" +
			"`unexpose` is an alias for this command.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "unpublish", func(ctx context.Context, c *client.Client, env string) (any, error) {
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
// public DNS record trips the dns.delete guardrail, DENIED by default (ADR-0065 §3) — the verb stays
// on this surface so the refusal is legible, and an operator who wants it relaxes the guardrail.
func newDomainRemoveCmd() *cobra.Command {
	o := &connOpts{}
	var provider string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "remove <host>",
		Short: "Remove the DNS record a configured provider holds for a hostname",
		Long: "Remove the DNS record a configured provider holds for a hostname. Deleting a public DNS record\n" +
			"trips the dns.delete guardrail, DENIED by default: removing the record takes an application off\n" +
			"the internet, and the record may not be one Burrow created. A denied outcome is final — no\n" +
			"--confirm opens it. Relay it; only the human, at the operator CLI, can relax the guardrail.\n" +
			"If they have set it to confirm, the outcome says held instead — re-run with --confirm ONLY\n" +
			"after they approve.",
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

// newAddonCmd groups the add-on operations exposed to the agent (install/attach/backup). Add-ons are
// per ENVIRONMENT rather than per cluster (ADR-0067 §1), so the subcommands bind --env as well as the
// connection flags: which environment an add-on operation targets is which server it reaches, and a
// call that names none while several environments are registered is refused rather than guessed
// (ADR-0047 §1).
//
// There is deliberately NO `remove` here, and it is not an oversight to be corrected for symmetry
// with `install`. Every app in an environment has its database on that environment's one Postgres
// instance (ADR-0031/0067 §1), so removal is not "remove an add-on", it is "remove THE add-on for
// this environment", taking every attached app in it down at once. That fails ADR-0065 §1's scope test unconditionally (no configuration makes the
// blast radius small) and no agent workflow legitimately needs it, which is what puts it in ADR-0065
// §2's tier 1: absent from this binary rather than merely guarded. `detach` and `restore` are absent
// for the same reason. Removing an add-on is `burrow addon remove` at the operator's own terminal.
// TestAddonRemoveStructurallyAbsent and the closed surface guard assert the absence, so re-adding it
// here fails the build's tests rather than passing quietly.
//
// The group carries a RunE for the bare invocation so that NoArgs is reached at all: cobra returns
// help (and exit 0) from a group with no RunE before it validates args, which would make
// `addon remove` look like it quietly worked. With one, `addon <unknown>` is rejected by name — the
// legible refusal ADR-0065 §5 asks an absent verb to produce, rather than a silent dead end. `guard`
// already fails `guard set` this way.
func newAddonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addon",
		Short: "Operate the cluster's backing-service add-ons (install, attach, backup)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newAddonInstallCmd(), newAddonAttachCmd(), newAddonBackupCmd(), newAddonBackupHealthCmd(), newAddonSQLCmd())
	return cmd
}

// newAddonSQLCmd runs ONE statement against ONE app's database and returns columns and rows
// (ADR-0087). It is the first verb that reads the application's own data rather than the platform's
// state, which is why it is here at all and why it is closed by default.
//
// IT IS ON THIS SURFACE AND DENIED, and both halves are the decision (ADR-0087 §5, ADR-0065 §3 tier
// 2). On the surface, because an agent that can see the verb exists and is denied asks the human for
// it, while an agent that meets `unknown command` reaches for `kubectl` or a shell — the failure
// ADR-0021 says Burrow cannot close from the inside. Denied rather than held, because there is no
// upper bound on what a statement does: a human reading a hundred-line statement is not meaningfully
// approving it, and where a confirmation cannot be an informed one, holding for confirmation is
// theatre. `--confirm` is here for the operator who has moved the disposition to confirm for an
// environment; it does nothing to a deny.
//
// Burrow does not tell a read from a write, and the agent should not report one either: a `SELECT`
// can delete, a function call is whatever the function is, and a gate labelled safe that is not is
// worse than no gate (ADR-0087 §6).
func newAddonSQLCmd() *cobra.Command {
	o := &connOpts{}
	var statement string
	var instance string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "sql <addon> <app>",
		Short: "Run a statement against one app's database and get columns and rows back",
		Long: "Run ONE statement against one application's database on the named environment's Postgres\n" +
			"instance. Supply it with -c, or pipe it on stdin. The result is structured — column names, rows,\n" +
			"a row count, and whether it was truncated — so you can compose on it rather than parse a table.\n\n" +
			"The add-on type (\"postgres\") and the app together name the database, the same pair attach\n" +
			"takes. There is no form of this that reaches the instance, template1, or another app's\n" +
			"database: burrowd connects as that app's OWN role with the credential it already minted, so the\n" +
			"statement can touch exactly what the application can touch.\n\n" +
			"It runs independently of the application, so a database whose app is crash-looping is still\n" +
			"queryable — this is the tool for that case, not `run`.\n\n" +
			"A statement returning no rows comes back with its command tag and rows_affected. A statement\n" +
			"the database REFUSES comes back as an executed outcome carrying the error and its SQLSTATE: the\n" +
			"call worked, the statement did not, and 42P01 means the table is not there whatever the message\n" +
			"says. Read `truncated` before concluding you have seen every row.\n\n" +
			"GUARDED BY addon.sql AND DENIED BY DEFAULT. A denial is not something to work around: relay it,\n" +
			"and tell the human it is opened per environment with\n" +
			"`burrow guard set --env <env> addon.sql allow`. Burrow does not classify a statement as a read\n" +
			"or a write and neither should you — a SELECT can delete. The statement text is recorded in the\n" +
			"audit log, so do not put a secret in a literal.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			stmt, err := agentStatement(cmd, statement)
			if err != nil {
				return err
			}
			return o.mutate(cmd, "addon_sql", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.AddonSQL(ctx, args[0], args[1], env, instance, stmt, confirm)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVarP(&statement, "statement", "c", "", "the `SQL` to run (or pipe it on stdin)")
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` holding the database (default: the environment's own instance)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a statement a guardrail holds for confirmation (the default disposition is deny, which this does not open)")
	return cmd
}

// agentStatement resolves the statement from -c or from stdin. There is no --file on this surface:
// an agent composes the statement it wants to run, so a path would only add a way for the text that
// is audited to differ from the text somebody read.
func agentStatement(cmd *cobra.Command, statement string) (string, error) {
	if strings.TrimSpace(statement) != "" {
		return statement, nil
	}
	b, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("reading the statement from stdin: %w", err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("a statement is required: pass it with -c 'select …' or pipe it on stdin")
	}
	return string(b), nil
}

// newAddonBackupHealthCmd reports what Burrow observed about an add-on's backups (ADR-0063 §7,
// ADR-0066 §5). It is a READ: it changes nothing, so it fails neither of ADR-0065 §1's tests —
// scope, because it reads records Burrow already holds and probes a destination Burrow already has a
// credential for, and reversibility, because there is nothing to reverse. It is therefore on the
// surface unguarded, and it is named in the capability catalogue with that reasoning (ADR-0065 §6).
//
// It belongs here rather than being CLI-only because the agent is the reader most likely to need it
// BEFORE it does something else: "how old is the last backup that left the cluster" is the question
// worth asking ahead of a migration, a schema change, or relaying a human's request to remove an
// add-on. Answering it from Burrow's own rows also means the agent never has to read a backup
// engine's status fields, which can report stale values rather than absent ones.
func newAddonBackupHealthCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "backup-health <addon> [<app>]",
		Short: "Report backup coverage: last successful backup, last off-cluster backup, last failure, destination reachability",
		Long: "Report what Burrow itself observed about an add-on's backups: how long ago the last backup\n" +
			"completed, how long ago the last one actually LEFT THE CLUSTER, the most recent failure and its\n" +
			"machine-readable reason, how many are still pending, and whether each registered object-storage\n" +
			"destination answers right now.\n\n" +
			"The two ages answer different questions. A dump on an in-cluster volume shares a failure domain\n" +
			"with the database it came from, so only a backup that reached an object store survives losing\n" +
			"the cluster — treat that age as the real one. Read-only and not guarded; it carries names,\n" +
			"times and sizes, never a credential.\n\n" +
			"With no app it spans every app; with no --env, every environment.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := ""
			if len(args) == 2 {
				app = args[1]
			}
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.BackupHealth(ctx, args[0], app, env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
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
			return o.mutate(cmd, "addon_install", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.InstallAddon(ctx, args[0], env, client.InstallAddonOptions{Confirm: confirm})
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an add-on install a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newAddonAttachCmd gives an app its own database on the installed Postgres add-on and wires it in
// (ADR-0031). No secret crosses this channel: burrowd generates the connection string server-side and
// writes it into the app's Secret; the result carries only the KEY name, never the value.
//
// IT IS ADR-0065 TIER 3 — present on this surface and held for confirmation by the addon.attach
// guardrail (ADR-0095 §1). It passes the scope test, since the reach beyond the app is a database and
// a role on a server that already holds one per attached app, and passes reversibility for the case
// that dominates, since a detach undoes it and keeps the data (ADR-0090). It is not tier 3 for being
// harmless: it restarts the app, and on an app that is already attached it rotates a password nothing
// can restore.
//
// --as IS ON THIS SURFACE, and ADR-0065 §1 is why. It fails neither test. SCOPE: it changes which key
// of the app's OWN Secret is written, in the environment the attach already targets — the same app the
// agent was asked about, and nothing beyond it. REVERSIBILITY: the one irreversible thing it could do
// is overwrite a value nobody can read back, and the control plane refuses a name the app's config or
// Secret already holds rather than writing over it (issue #462), so what remains is a variable a
// re-attach can rename again. It is also the agent that knows which variable the app it is deploying
// actually reads; withholding the name would leave the agent writing a start-up wrapper to copy one
// variable to another, which is the workaround the flag exists to remove.
func newAddonAttachCmd() *cobra.Command {
	o := &connOpts{}
	var as string
	var instance string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "attach <addon> <app>",
		Short: "Give an app its own database on the installed Postgres add-on and wire it in",
		Long: "Give an application its own database on the installed Postgres add-on and wire it in. You supply\n" +
			"only the add-on type (\"postgres\"), the app name, and optionally the variable name — NO secret.\n" +
			"Burrow generates the database, role, and connection string server-side and writes it into the\n" +
			"app's Secret; the value is never returned or shown. Re-attaching rotates the password. The result\n" +
			"carries only the app, the add-on, the environment, and the KEY name — never the value. Each\n" +
			"environment has its OWN database instance, so --env decides which server the app is given a\n" +
			"database on; with several environments registered, naming one is required.\n\n" +
			"Attaching is HELD FOR CONFIRMATION by the addon.attach guardrail by default: it puts a database\n" +
			"on a server every other app in the environment shares, restarts the app, and on an app that is\n" +
			"already attached rotates its password, which nothing can undo. A held attach provisions nothing\n" +
			"and comes back naming what it would do — surface that to a human, and re-run with --confirm\n" +
			"once they approve. Do not pass --confirm on your own account. `guard` reports the disposition\n" +
			"ahead of time, and a person can relax it for one environment with\n" +
			"`burrow guard set --env <env> addon.attach allow`.\n\n" +
			"The variable is DATABASE_URL unless --as names another, so an app that reads DB_URL or PG_DSN\n" +
			"can be wired to it directly instead of copying one variable to another at start-up. --as on an\n" +
			"already-attached app MOVES the variable — the result reports the removed name in\n" +
			"previous_secret_key. A name the app's config or Secret already holds is REFUSED, naming what\n" +
			"holds it, rather than overwriting a value that cannot be read back.\n\n" +
			"An environment may hold more than one database instance. Without --name the app is attached to\n" +
			"the environment's own; --name attaches it to another one that already exists, and an app may hold\n" +
			"several attachments at once. A SECOND attachment must name its own variable with --as, because\n" +
			"DATABASE_URL belongs to the first — Burrow refuses rather than inventing a name the application\n" +
			"was never told to read. Creating an instance is a person's job: if the one you want is not there,\n" +
			"say that a human can add it with `burrow addon install postgres --name <name>`.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "addon_attach", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.AttachAddon(ctx, args[0], args[1], env, client.AttachAddonOptions{Instance: instance, EnvKey: as, Confirm: confirm})
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&as, "as", "", "environment variable to write the connection string into (default DATABASE_URL, or the name this attachment already uses)")
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` to attach to (default: the environment's own instance)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an attach a guardrail holds for confirmation (supply only after the human approves)")
	return cmd
}

// newAddonBackupCmd backs up an app's database on the installed Postgres add-on (ADR-0032,
// ADR-0063 §7). No secret crosses this channel; the result is the recorded backup row (id, app,
// path, destination, size, status), never a credential. Backup destroys nothing, so it is not
// guarded.
//
// The result's `destination` and `status` are the two fields worth reading together: a completed
// backup whose destination is the object store is one whose bytes left the cluster, and a completed
// backup whose destination is the cluster is a dump sharing a failure domain with the database. A
// failed one carries a reason from a closed set, so the agent can tell a transient outage from a
// credential that will never work without parsing prose (ADR-0074 §5).
func newAddonBackupCmd() *cobra.Command {
	o := &connOpts{}
	var destination string
	var instance string
	cmd := &cobra.Command{
		Use:   "backup <addon> <app>",
		Short: "Back up an app's database on the installed Postgres add-on",
		Long: "Back up an application's database on the installed Postgres add-on. You supply only the add-on\n" +
			"type (\"postgres\") and the app name — NO secret. Burrow runs an in-cluster Job that dumps the\n" +
			"database to a backup volume and, when an object-storage provider is registered, writes that dump\n" +
			"to the store and reads it back before the backup is recorded as completed; no credential crosses\n" +
			"this channel or appears in the result. The result is the recorded backup (id, app, path,\n" +
			"destination, size, status); a failure carries a machine-readable reason. Backup destroys nothing,\n" +
			"so it is not guarded. To RESTORE a backup (which overwrites live data), the human runs\n" +
			"`burrow addon restore postgres <app> --backup <id>` — restore is CLI-only.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "addon_backup", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.BackupAddon(ctx, args[0], args[1], env, instance, destination)
			})
		},
	}
	cmd.Flags().StringVar(&destination, "destination", "",
		"the object-storage `provider` to write this backup to (only needed when more than one is registered)")
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` holding the database (default: the environment's own instance)")
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
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
			"set a secret, the human runs `burrow app secret set <app> KEY` at their own terminal and types the\n" +
			"value at its hidden prompt. Never ask for the value here: anything in this conversation is\n" +
			"retained and re-sent.",
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

// newSecretMountCmd projects one of an app's secret keys into a file (ADR-0089 §1). Like
// `secret unset` it names a KEY and carries no value, so it is on this surface — and it is the one
// verb here that makes a credential SAFER, by taking it out of an environment every child process
// inherits. Not guarded (§7): config and secret mutation are ungated today.
func newSecretMountCmd() *cobra.Command {
	o := &connOpts{}
	var filename, dir string
	var noEnv bool
	cmd := &cobra.Command{
		Use:   "mount <app> KEY",
		Short: "Project a secret key into a file the app reads from disk (no value crosses the agent channel)",
		Long: "Project one of an app's secret keys into a file under a directory Burrow owns, /run/secrets\n" +
			"by default. Reach for this when the credential is file-shaped — a kubeconfig, a PEM private\n" +
			"key, a service-account JSON — or when it should stay out of the environment: a variable is\n" +
			"readable at /proc/<pid>/environ and is inherited by every child process.\n\n" +
			"This names a KEY and never a value. The key must already be SET, by the human at their own\n" +
			"terminal; mounting one that is not set is refused, because it would produce an app that\n" +
			"starts and only fails when it opens the file. Never ask for the value here.\n\n" +
			"The app reads the directory from BURROW_SECRETS_DIR. If it hardcodes a path variable of its\n" +
			"own (GOOGLE_APPLICATION_CREDENTIALS, KUBECONFIG), point it at the file with `config set`.\n\n" +
			"--dir moves the directory for the whole app; there is no per-key path, because one can\n" +
			"shadow a file in the app's image and it stops the file being updated in place on rotation.\n\n" +
			"Mounting on its own does NOT remove the environment variable, and unmounting does not unset\n" +
			"the key. --no-env is what removes it, and only reach for it once the deployed code reads\n" +
			"the file: taking the variable away from code that still reads it breaks the app. It also\n" +
			"switches this app to naming each remaining secret key in its pod template, after which a\n" +
			"`secret set` re-applies the app rather than restarting it. Re-mount with --no-env=false to\n" +
			"put the variable back; leaving the flag off leaves the key however it already was.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "secret_mount", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.MountSecret(ctx, args[0], env, args[1], filename, dir, askedNoEnv(cmd, noEnv))
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&filename, "filename", "", "the `name` the key lands under (default: the key itself)")
	cmd.Flags().StringVar(&dir, "dir", "", "the `directory` this app's mounted keys land in (default: /run/secrets) — per app, never per key")
	cmd.Flags().BoolVar(&noEnv, "no-env", false, "read this key from its file ONLY, and keep it out of the app's environment; --no-env=false puts the variable back")
	return cmd
}

// askedNoEnv turns the --no-env flag into the tri-state the API takes: nil when the caller did not
// mention it, so a mount that renames a file leaves the app's environment exactly as it found it
// (ADR-0089 §4). An agent re-running a mount it is not sure took must not thereby return a
// credential to /proc/self/environ.
func askedNoEnv(cmd *cobra.Command, noEnv bool) *bool {
	if !cmd.Flags().Changed("no-env") {
		return nil
	}
	return &noEnv
}

// newSecretUnmountCmd stops projecting one key as a file. It removes a FILE and never a value: the
// key stays set and stays in the app's environment, which is what makes it reversible.
func newSecretUnmountCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "unmount <app> KEY",
		Short: "Stop projecting a secret key into a file (the value is untouched)",
		Long: "Stop projecting a secret key into a file. The value is untouched: the key stays set, and it is\n" +
			"back in the app's environment even if it was mounted --no-env, so this cannot lose a\n" +
			"credential — it is `secret mount` undone.\n\n" +
			"The running app is rolled so the file leaves its pods.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "secret_unmount", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.UnmountSecret(ctx, args[0], env, args[1])
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newSecretMountsCmd lists which of an app's keys are read as files, and where. A read of key names
// and paths — the same class as the key listing it hangs beside.
func newSecretMountsCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "mounts <app>",
		Short: "List the secret keys an app reads as files, and where they land",
		Long: "List which of an app's secret keys are projected into files, and the path each one lands at.\n" +
			"Keys and paths only — a value never crosses this channel.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.SecretMounts(ctx, args[0], env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newDeleteCmd deletes an app entirely — its workload, routing, and release history. It is guarded by
// app.delete, DENIED by default (ADR-0065 §3) because destroying the release history leaves nothing to
// roll back to. The verb stays on this surface rather than leaving the binary: a denial the agent can
// read and relay beats an `unknown command` it might route around (ADR-0065 §5). An operator who wants
// the agent tidying up after itself relaxes the guardrail, ideally per environment.
func newDeleteCmd() *cobra.Command {
	o := &connOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "delete <app>",
		Short: "Delete an app entirely (its workload, routing, and release history)",
		Long: "Delete an application entirely: its workload, its routing (Service and Ingress), and its\n" +
			"recorded release history, so it disappears from the apps listing and from status. This is\n" +
			"destructive and irreversible.\n\n" +
			"Guarded by the app.delete guardrail, DENIED by default. A denied outcome is final — no --confirm\n" +
			"opens it. Relay it; only the human, at the operator CLI, can relax the guardrail, and the message\n" +
			"names the per-environment way to do it. If they have set it to confirm, the outcome says held\n" +
			"instead — re-run with --confirm ONLY after they explicitly approve. Never self-confirm a deletion.",
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
