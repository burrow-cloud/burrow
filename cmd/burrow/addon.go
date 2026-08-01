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
	"time"

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
	{string(controlplane.AddonPostgres), "PostgreSQL on CloudNativePG (needs `burrow cluster postgres install` first)"},
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
		Use: "addon",
		// `burrow addons` reads naturally when listing them, so accept the plural rather than
		// answering it with a "did you mean" suggestion. The canonical name stays singular (ADR-0024).
		Aliases: []string{"addons"},
		Short:   "Install and manage backing services (logs, metrics, …)",
		Long: "addon installs and operates vetted, self-hostable backing services on your cluster —\n" +
			"`addon install logs` stands up log aggregation and registers it as a capability your\n" +
			"agent can query. Every install/remove is gated by a guardrail.",
	}
	cmd.AddCommand(newAddonInstallCmd(), newAddonConnectCmd(), newAddonAttachCmd(), newAddonDetachCmd(), newAddonBackupCmd(), newAddonBackupInstanceCmd(), newAddonBackupsCmd(), newAddonBackupHealthCmd(), newAddonRestoreCmd(), newAddonRestoreInstanceCmd(), newAddonListCmd(), newAddonLogsCmd(), newAddonMetricsCmd(), newAddonRemoveCmd())
	return cmd
}

// newAddonBackupCmd is `burrow addon backup postgres <app>`: back up an app's database on the
// installed Postgres add-on (ADR-0032, ADR-0063 §7). burrowd runs an in-cluster Job that pg_dumps
// the database to the backup PVC and, when an object-storage destination is registered, writes it on
// to the store and reads it back before the backup is recorded as completed; no secret value crosses
// the API. Backup destroys nothing, so it is allowed by default.
func newAddonBackupCmd() *cobra.Command {
	o := &commonOpts{}
	var destination string
	cmd := &cobra.Command{
		Use:   "backup <addon> <app>",
		Short: "Back up an app's database (e.g. on the Postgres add-on)",
		Long: "backup runs an in-cluster Job that pg_dumps an app's database on the installed Postgres\n" +
			"add-on to a backup volume and records the backup in the control plane. No secret value crosses\n" +
			"the API — the Job reads the superuser password from the add-on's Secret in-cluster.\n\n" +
			"With an object-storage provider registered, the same Job writes the dump to that store and\n" +
			"reads it back before the backup is recorded as completed, so a completed backup is one whose\n" +
			"bytes left the cluster. With none registered the dump stays on the in-cluster volume, which\n" +
			"shares a failure domain with the database it came from — the recorded backup says which.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.BackupAddon(ctx, args[0], args[1], o.env, destination)
			if err != nil {
				return err
			}
			b := res.Backup
			human := fmt.Sprintf("backed up %q in environment %s (backup %s, status %s)\n%s",
				b.App, b.Environment, b.ID, b.Status, backupWhere(b))
			return o.emitChange(cmd.OutOrStdout(), res, human)
		},
	}
	cmd.Flags().StringVar(&destination, "destination", "",
		"the object-storage `provider` to write this backup to (only needed when more than one is registered)")
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newAddonBackupInstanceCmd is `burrow addon backup-instance postgres`: a PHYSICAL base backup of one
// environment's whole Postgres instance (ADR-0066 §2). burrowd creates a `Backup` custom resource,
// CloudNativePG hands it to the pgBackRest plugin, and burrowd reads the result back out of the
// object store before recording it as completed. Burrow runs no backup tool on this path.
//
// It is a separate command from `addon backup` rather than a flag on it, and the naming is doing
// work ADR-0066 §4 asks it to do: the two answer different questions and reaching for the wrong one
// under pressure is the failure the record names. `backup <app>` is "this app's data is wrong";
// `backup-instance` is "the instance is gone".
func newAddonBackupInstanceCmd() *cobra.Command {
	o := &commonOpts{}
	var destination string
	cmd := &cobra.Command{
		Use:   "backup-instance <addon>",
		Short: "Take a physical base backup of an environment's whole Postgres instance",
		Long: "backup-instance asks CloudNativePG for a base backup of the WHOLE instance in one\n" +
			"environment: every app's database on it, written to the pgBackRest repository in your\n" +
			"object store, with the write-ahead log archived continuously behind it.\n\n" +
			"It is not the same operation as `burrow addon backup <addon> <app>`. That dumps one app's\n" +
			"database and restores into that app alone. This one covers the whole instance and can only\n" +
			"be restored as the whole instance, so it answers \"the instance is gone\" rather than \"this\n" +
			"app's data is wrong\". Both are worth having and neither replaces the other.\n\n" +
			"It requires an object-storage provider to be registered and the instance to have been wired\n" +
			"to it: there is no in-cluster tier for a physical backup.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.BackupInstance(ctx, args[0], o.env, destination)
			if err != nil {
				return err
			}
			b := res.Backup
			human := fmt.Sprintf("backed up the whole %s instance in environment %s (backup %s, status %s)\n%s",
				args[0], b.Environment, b.ID, b.Status, backupWhere(b))
			return o.emitChange(cmd.OutOrStdout(), res, human)
		},
	}
	cmd.Flags().StringVar(&destination, "destination", "",
		"the object-storage `provider` holding this instance's repository (only needed when more than one is registered)")
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// backupWhere says where a backup actually went, in one line. An in-cluster backup says so plainly:
// "stored at /backups/..." reads like an address rather than a warning, and the fact worth surfacing
// is that this dump does not survive losing the cluster.
func backupWhere(b client.Backup) string {
	if b.Destination == "object-store" && b.ObjectKey != "" {
		return fmt.Sprintf("written to the %s object store at %s", b.Provider, b.ObjectKey)
	}
	return fmt.Sprintf("stored at %s, on a volume in this cluster — register an object-storage provider\n"+
		"  with `burrow config provider add --type s3` to keep backups outside it", b.Path)
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
			// WHERE is a column rather than a footnote: the difference between a backup that survives
			// losing this cluster and one that does not is the most important thing on the row, and a
			// listing that hid it would let a set of in-cluster dumps read as a backup strategy.
			fmt.Fprintln(tw, "ID\tAPP\tENV\tCREATED\tSTATUS\tWHERE\tSIZE")
			for _, b := range backups {
				where := b.Destination
				if where == "" {
					where = "cluster"
				}
				// A physical backup belongs to no app, and an empty column would read as a missing
				// value rather than as the fact that it covers all of them (ADR-0066 §4).
				app := b.App
				if app == "" {
					app = "(whole instance)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", b.ID, app, b.Environment, b.CreatedAt, b.Status, where, b.SizeBytes)
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newAddonBackupHealthCmd is `burrow addon backup-health postgres [<app>]`: ADR-0063 §7's status
// surface, which ADR-0066 §5 requires the backup-age signal to live on. It answers "are my backups
// working?" from what BURROW observed — its own recorded backups, plus a reachability probe of each
// registered destination run at the moment of the call.
//
// It is a separate command from `backups` because it answers a different question. `backups` lists
// what exists; this says whether what exists amounts to coverage — and in particular whether any of
// it left the cluster, which a listing can only show one row at a time.
func newAddonBackupHealthCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "backup-health <addon> [<app>]",
		Short: "Report backup coverage: last successful backup, last failure, destination reachability",
		Long: "backup-health reports what Burrow itself observed about an add-on's backups: how long ago\n" +
			"the last backup completed, how long ago the last one actually left the cluster, the most\n" +
			"recent failure and why, and whether each registered object-storage destination answers now.\n\n" +
			"The two ages are different questions. An in-cluster dump shares a failure domain with the\n" +
			"database it came from, so only a backup that reached an object store is one that survives\n" +
			"losing the cluster — and that is the age worth watching.\n\n" +
			"With no app it spans every app; with no --env, every environment. Read-only.",
		Args: cobra.RangeArgs(1, 2),
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
			health, err := c.BackupHealth(ctx, args[0], app, o.env)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, health, backupHealthHuman(health))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// backupHealthHuman renders the health report for a human. The summary leads, because it is the
// answer; the observations follow as the evidence for it. A destination that did not answer is
// listed with what Burrow could not do, never with the vendor's own words.
func backupHealthHuman(h client.BackupHealth) string {
	var b strings.Builder
	b.WriteString(h.Summary)
	if h.LastSuccess != nil {
		fmt.Fprintf(&b, "\n\nlast successful backup:  %s (%s, %s ago, %s)",
			h.LastSuccess.ID, h.LastSuccess.App, backupAge(h.LastSuccess.AgeSeconds), backupWhereShort(*h.LastSuccess))
	}
	if h.LastDurableSuccess != nil && (h.LastSuccess == nil || h.LastDurableSuccess.ID != h.LastSuccess.ID) {
		fmt.Fprintf(&b, "\nlast off-cluster backup: %s (%s, %s ago, %s)",
			h.LastDurableSuccess.ID, h.LastDurableSuccess.App, backupAge(h.LastDurableSuccess.AgeSeconds), backupWhereShort(*h.LastDurableSuccess))
	}
	if h.LastFailure != nil {
		fmt.Fprintf(&b, "\nlast failure:            %s (%s, %s ago, %s)",
			h.LastFailure.ID, h.LastFailure.App, backupAge(h.LastFailure.AgeSeconds), h.LastFailure.Reason)
		if h.LastFailure.Detail != "" {
			fmt.Fprintf(&b, "\n                         %s", h.LastFailure.Detail)
		}
	}
	if h.Pending > 0 {
		fmt.Fprintf(&b, "\npending:                 %d (a pending backup is never counted as a successful one)", h.Pending)
	}
	if len(h.Destinations) > 0 {
		b.WriteString("\n\ndestinations:")
		for _, d := range h.Destinations {
			mark := "unreachable"
			if d.Reachable {
				mark = "reachable"
			}
			fmt.Fprintf(&b, "\n  %s  %s (%s) — %s", d.Provider, mark, d.Bucket, d.Detail)
		}
	}
	return b.String()
}

// backupWhereShort names where one backup's bytes went, in a few words.
func backupWhereShort(o client.BackupObservation) string {
	if o.Destination == "object-store" && o.Provider != "" {
		return "the " + o.Provider + " object store"
	}
	return "a volume in this cluster"
}

// backupAge renders an age in seconds the way the server's own summary does, so the two lines of one
// report do not describe the same duration differently.
func backupAge(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dd", seconds/86400)
	}
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
			human := fmt.Sprintf("restored %q from backup %s", args[1], backup)
			return o.emitChange(cmd.OutOrStdout(), map[string]string{"addon": args[0], "app": args[1], "backup": backup}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&backup, "backup", "", "the backup id to restore (from `burrow addon backups postgres <app>`)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	_ = cmd.MarkFlagRequired("backup")
	return cmd
}

// newAddonRestoreInstanceCmd is `burrow addon restore-instance postgres`: rewind one environment's
// WHOLE Postgres instance to a point in its object-storage repository (ADR-0066 §4).
//
// IT IS A SEPARATE VERB FROM `addon restore`, NOT A FLAG ON IT, and that is the naming ADR-0066 §4
// asks for. `addon restore <addon> <app>` acts on one app's database and refuses a physical backup by
// name; this acts on the instance and refuses a logical dump by name. The two point at each other, so
// whichever one is reached for under pressure says which one the situation actually calls for.
//
// It is on the operator CLI only. `burrow-agent` does not carry it — not gated, not permitted:
// the verb is absent from the binary (ADR-0065 §2 tier 1), because rewinding the database every app
// in an environment shares is not an agent operation in any configuration. `guard` reports it as
// absent so the agent relays what it is and who can run it rather than failing with an unknown
// command.
//
// On top of the addon.restore_instance guardrail it carries the typed confirmation ADR-0064 §2
// established for `--delete-data`, because the blast radius is comparable: on a terminal the
// instance's name is typed back after a notice naming every app that goes with it, and off a terminal
// it refuses unless --acknowledge-data-loss is passed.
func newAddonRestoreInstanceCmd() *cobra.Command {
	o := &commonOpts{}
	var backup, toTime, destination string
	var latest, skipSafety, ackDataLoss, confirm bool
	cmd := &cobra.Command{
		Use:   "restore-instance <addon>",
		Short: "Rewind an environment's whole Postgres instance to a point in its object-storage repository",
		Long: "restore-instance replaces an environment's Postgres instance with one recovered from the\n" +
			"pgBackRest repository in your object store, at a point you name. EVERY database on that\n" +
			"instance goes back to that point together, so every app attached to it is affected — this is\n" +
			"an instance operation and it cannot be scoped to one app.\n\n" +
			"To restore ONE app's database, use `burrow addon restore <addon> <app> --backup <id>`, which\n" +
			"replays that app's own dump and touches nothing else.\n\n" +
			"Name exactly one recovery target: --backup for a recorded base backup, --to-time for any\n" +
			"instant inside the write-ahead-log window, or --latest for the newest state the repository\n" +
			"holds. Nothing is assumed.\n\n" +
			"Burrow takes a physical backup of the instance's CURRENT state first and stops without\n" +
			"changing anything if it does not reach the object store, so a rewind to the wrong point can\n" +
			"itself be rewound. --skip-safety-backup goes ahead without one, for an instance too broken\n" +
			"to back up.\n\n" +
			"The instance's data volume is destroyed and rebuilt from the repository, and each attached\n" +
			"app's DATABASE_URL is rewritten for the recovered instance and the app restarted: a recovered\n" +
			"instance holds the login roles as they were at the recovery point, so the credential an app\n" +
			"is holding may no longer work until it is reissued.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			addon := args[0]
			// Everything that can be refused WITHOUT contacting the control plane is refused before
			// the prompt. Asking somebody to type a database's name back and then telling them the
			// add-on was wrong, or the timestamp unparseable, is a bad way to spend the one moment
			// they are reading carefully.
			if addon != string(controlplane.AddonPostgres) {
				return fmt.Errorf("only the postgres add-on has physical restore; %q has no object-storage repository to recover from", addon)
			}
			if err := checkOneRecoveryTarget(backup, toTime, latest); err != nil {
				return err
			}
			if toTime != "" {
				if _, err := time.Parse(time.RFC3339, toTime); err != nil {
					return fmt.Errorf("--to-time %q is not an RFC3339 instant (for example 2026-08-01T14:30:00Z)", toTime)
				}
			}
			instance, err := controlplane.AddonInstanceName(controlplane.AddonType(addon), restoreEnvironment(o.env))
			if err != nil {
				return err
			}
			// The off-terminal refusal runs BEFORE anything is contacted, so a script that never said
			// it destroys the current state cannot reach the restore call at all (ADR-0064 §2).
			if !ackDataLoss && !stdinIsTerminal(cmd.InOrStdin()) {
				return errRestoreInstanceNeedsTerminal(instance)
			}
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			// The typed-name gate goes to stderr so a --json run keeps a clean stdout.
			if !ackDataLoss {
				if err := confirmRestoreInstance(ctx, c, instance, o.env, recoveryTargetLabel(backup, toTime), skipSafety, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			res, err := c.RestoreInstance(ctx, addon, o.env, client.RestoreInstanceOptions{
				Backup:           backup,
				ToTime:           toTime,
				Latest:           latest,
				SkipSafetyBackup: skipSafety,
				Destination:      destination,
				Confirm:          confirm,
			})
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, res, restoreInstanceSummary(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&backup, "backup", "", "recover exactly this recorded physical `backup` (from `burrow addon backups postgres`)")
	cmd.Flags().StringVar(&toTime, "to-time", "", "recover to this RFC3339 `instant` inside the write-ahead-log window (e.g. 2026-08-01T14:30:00Z)")
	cmd.Flags().BoolVar(&latest, "latest", false, "recover to the newest state the repository holds (the furthest point the archived write-ahead log reaches)")
	cmd.Flags().StringVar(&destination, "destination", "",
		"the object-storage `provider` holding this instance's repository (only needed when more than one is registered)")
	cmd.Flags().BoolVar(&skipSafety, "skip-safety-backup", false, "rewind WITHOUT first backing up the state that is there. For an instance too broken to back up — the output says plainly that no copy was taken.")
	cmd.Flags().BoolVar(&ackDataLoss, dataLossAckFlag, false, "acknowledge that this destroys the instance's current state, so it proceeds without the typed-name prompt (this is how a script asks for it). No shorthand: rewinding every app's database should not be a single keystroke.")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

// checkOneRecoveryTarget refuses zero targets and more than one, in the CLI as well as on the server.
// It is checked here so the typed-name prompt is never printed for an invocation that was going to be
// refused anyway — asking someone to type a database's name and then telling them the flags were
// wrong is a bad way to spend the one moment they are reading carefully.
func checkOneRecoveryTarget(backup, toTime string, latest bool) error {
	named := 0
	for _, on := range []bool{backup != "", toTime != "", latest} {
		if on {
			named++
		}
	}
	switch named {
	case 1:
		return nil
	case 0:
		return errors.New("name where to rewind to: --backup <id> for a recorded backup, --to-time <RFC3339> for an instant in the write-ahead-log window, or --latest for the newest state the repository holds")
	default:
		return errors.New("name exactly one recovery target: --backup, --to-time, or --latest")
	}
}

// recoveryTargetLabel renders the target for the notice, in the same words the server uses in the
// guardrail message and the result, so an operator reads one description of the point rather than
// three.
func recoveryTargetLabel(backup, toTime string) string {
	switch {
	case backup != "":
		return "backup " + backup
	case toTime != "":
		return "the state at " + toTime
	default:
		return "the newest state in the repository"
	}
}

// restoreEnvironment resolves the environment flag for the purpose of NAMING the instance in the
// notice. An unnamed environment is the default one, which is what the server resolves it to when
// exactly one environment is registered; with several the server refuses, so the name printed here
// is never the one a restore silently went to.
func restoreEnvironment(env string) string {
	if env == "" {
		return controlplane.DefaultEnvironment
	}
	return env
}

// errRestoreInstanceNeedsTerminal is the refusal a physical restore returns off a terminal without
// the acknowledgement flag (ADR-0064 §2). It refuses rather than proceeding, and names both ways out.
func errRestoreInstanceNeedsTerminal(instance string) error {
	return fmt.Errorf("restore-instance rewinds every database on the postgres instance %q and asks for the "+
		"instance's name to be typed back, which needs an interactive terminal; re-run with --%s to say so "+
		"explicitly in a script. To restore a single app instead, use `burrow addon restore <addon> <app> "+
		"--backup <id>`", instance, dataLossAckFlag)
}

// confirmRestoreInstance is the physical restore's human gate: it prints what the rewind would do and
// requires the instance's name to be typed back before proceeding (ADR-0064 §2).
//
// THE INSTANCE'S NAME IS WHAT IS TYPED, not the add-on type on the command line. `postgres` is a word
// somebody types without reading; `burrow-postgres-staging` is the environment's instance, and typing
// it is the operator saying which environment they know they are rewinding. That is the difference
// between a gate and a formality on a command whose argument does not name the thing it acts on.
//
// THIS PROMPT IS FOR HUMANS AND IS NOT A SECURITY CONTROL. Anything with a shell can type a word, so
// it adds nothing against an agent: what keeps an agent away from this is that `addon restore-instance`
// is not compiled into burrow-agent at all (ADR-0065 §2 tier 1). That structural absence must never be
// relaxed on the grounds that "there's a confirmation anyway".
func confirmRestoreInstance(ctx context.Context, c *client.Client, instance, env, target string, skipSafety bool, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, warning(out)+restoreInstanceConsequence(ctx, c, instance, env, target, skipSafety))
	fmt.Fprintln(out)
	typed, err := readLine(in, out, fmt.Sprintf("Type the instance's name (%s) to proceed, or anything else to abort: ", instance))
	if err != nil {
		return err
	}
	if typed != instance {
		return fmt.Errorf("aborted: %q is not the instance's name (%s); nothing was restored", typed, instance)
	}
	return nil
}

// restoreInstanceConsequence renders what the rewind is about to do, in the same terms the
// addon.restore_instance guardrail's held confirmation uses — the instance by name, the point it goes
// back to, and EVERY app whose database goes with it, listed rather than counted.
//
// A COUNT IS NOT CONSENT. "3 apps are affected" is a number to nod at; "api, web and worker all go
// back to that point" is the sentence that makes somebody stop when one of those names should not be
// on the list. That is the whole reason this reads the apps rather than reporting how many there are.
//
// It enumerates from the APPS' own Secrets rather than by asking the instance, which is the opposite
// of what a removal's notice does and is right for the opposite reason: a restore is reached for when
// the instance is the broken thing, so an enumeration that needed the instance would be least
// reliable exactly here. Only key NAMES are read, never values (ADR-0028). Best-effort like every
// other lookup behind a notice — an unreadable listing costs the sentence, and the server's own
// guardrail message carries the authoritative list either way.
func restoreInstanceConsequence(ctx context.Context, c *client.Client, instance, env, target string, skipSafety bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "restore-instance REWINDS the whole postgres instance %q in environment %s to %s.\n"+
		"  Every database on it goes back to that point and everything written since is gone.",
		instance, restoreEnvironment(env), target)
	if apps := attachedAppsIn(ctx, c, env); len(apps) > 0 {
		fmt.Fprintf(&b, "\n  On this instance: %s. All of them are rewound together, their DATABASE_URL is\n"+
			"  reissued afterwards, and each app is restarted.", pluralApps(apps))
	}
	b.WriteString("\n  The instance's current data volume is DESTROYED and rebuilt from the repository.")
	if skipSafety {
		b.WriteString("\n  --skip-safety-backup: NO backup of the current state is taken first. This is the last\n" +
			"  moment that state exists.")
	} else {
		b.WriteString("\n  A physical backup of the current state is written to the object store FIRST; if it does\n" +
			"  not get there, nothing is changed. That backup is the way back from a rewind to the wrong point.")
	}
	return b.String()
}

// restoreInstanceSummary renders the human outcome of a physical restore. It always says what
// happened to the state that was there and which apps came back onto the instance, because "restored
// the instance" alone leaves both questions a person actually has unanswered.
func restoreInstanceSummary(res client.RestoreInstanceResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rewound the %s instance %q in environment %s to %s\n", res.Addon, res.Instance, res.Environment, res.Target)
	// What happened to the copy comes immediately after what happened to the original, because they
	// are one fact read together (ADR-0064 §5's posture). A skipped backup is stated as plainly as a
	// taken one.
	if res.SafetyBackup != "" {
		fmt.Fprintf(&b, "backup of the pre-restore state taken first: %s — `burrow addon restore-instance %s --backup %s` puts it back\n",
			res.SafetyBackup, res.Addon, res.SafetyBackup)
	}
	if res.SafetyBackupNote != "" {
		fmt.Fprintf(&b, "%s\n", res.SafetyBackupNote)
	}
	if len(res.Reconnected) > 0 {
		fmt.Fprintf(&b, "%d app(s) (%s) were reissued a DATABASE_URL for the recovered instance and restarted\n",
			len(res.Reconnected), strings.Join(res.Reconnected, ", "))
	}
	for _, s := range res.Stranded {
		fmt.Fprintf(&b, "%q was NOT reconnected: %s\n", s.App, s.Reason)
	}
	if len(res.Apps) == 0 {
		b.WriteString("no app was attached to this instance\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// attachedAppsIn is attachedApps scoped to ONE environment (ADR-0067 §1). A physical restore rewinds
// one environment's instance, so a notice listing another environment's apps would name apps this
// operation does not touch — and understate or overstate the blast radius in the one message whose
// job is to state it exactly.
func attachedAppsIn(ctx context.Context, c *client.Client, env string) []string {
	apps, err := c.Apps(ctx, env)
	if err != nil {
		return nil
	}
	var attached []string
	for _, a := range apps {
		keys, err := c.Secrets(ctx, a.App, env)
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
			return o.emitChange(cmd.OutOrStdout(), res, human)
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
			human := fmt.Sprintf("detached %q from the %s add-on", args[1], args[0])
			return o.emitChange(cmd.OutOrStdout(), map[string]string{"addon": args[0], "app": args[1]}, human)
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
				return o.emitChange(cmd.OutOrStdout(), a, connectHuman(a, ""))
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
			return o.emitChange(cmd.OutOrStdout(), a, connectHuman(a, key))
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
	var archiveDestination string
	cmd := &cobra.Command{
		Use:   "install [<name>]",
		Short: "Install a vetted backing service (logs, metrics, cache, postgres)",
		Long: "install deploys the vetted, permissively-licensed default for an add-on (logs is\n" +
			"VictoriaLogs, metrics is VictoriaMetrics) and registers it so your agent can use it. The\n" +
			"install is gated by the addon.install guardrail.\n\n" +
			"Run `burrow addon install` with no name to list the add-ons you can install and which are\n" +
			"already installed.\n\n" +
			"postgres runs on CloudNativePG, so the operator has to be on the cluster first:\n" +
			"`burrow cluster postgres install`. That is a one-time operator step — it installs\n" +
			"cluster-scoped CustomResourceDefinitions and needs cluster-admin.",
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
			a, err := c.InstallAddon(ctx, name, o.env, client.InstallAddonOptions{
				Confirm:            confirm,
				ArchiveDestination: archiveDestination,
			})
			if err != nil {
				return err
			}
			human := fmt.Sprintf("installed the %s add-on %q (%s)\nin-cluster endpoint: %s\nprovides: %s",
				a.Type, a.Name, a.Image, a.Endpoint, strings.Join(a.Capabilities, ", "))
			// The warning goes on the human output as its own line rather than being folded into the
			// summary: it says what this instance does NOT do, and an install that quietly took no
			// backups is exactly the thing nobody should have to read carefully to notice.
			if a.Warning != "" {
				human += "\nnote: " + a.Warning
			}
			return o.emitChange(out, a, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	cmd.Flags().StringVar(&archiveDestination, "archive-destination", "",
		"the object-storage `provider` a postgres instance archives to (only needed when more than one is registered)")
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
	fmt.Fprintln(tw, "CLAIM\tADD-ON\tENV\tHOLDS\tSIZE\tNAMESPACE")
	adoptable := false
	for _, v := range vols {
		size := v.Size
		if size == "" {
			size = "-"
		}
		// The environment is a column rather than something to read off the claim name, because a
		// claim per environment (ADR-0067 §1) means several rows differ only by it.
		env := v.Environment
		if env == "" {
			env = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.Addon, env, v.Role, size, v.Namespace)
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
//
// With an object-storage provider registered, burrowd takes a final backup of every attached database
// before it destroys anything and abandons the removal if it does not reach the store (ADR-0064 §5).
// --skip-final-backup is the way past that, and it exists because an add-on is often removed BECAUSE
// it is wedged: without an override a broken add-on would be undeletable.
func newAddonRemoveCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm, deleteData, ackDataLoss, skipFinalBackup bool
	var backupDestination string
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed add-on (keeps its data volume)",
		Long: "remove tears down an add-on's workload — for postgres its CloudNativePG Cluster, for\n" +
			"every other add-on its Deployment, Service, and collectors — and\n" +
			"KEEPS its data volume, so reinstalling the add-on picks the data back up. Pass --delete-data\n" +
			"to destroy the volume as well; for the postgres add-on that destroys every attached app's\n" +
			"database. Recorded backups are on a separate volume and survive either way.\n\n" +
			"--delete-data prints what it would destroy and asks for the add-on's name to be typed back.\n" +
			"With no terminal to type into it refuses rather than proceeding; --acknowledge-data-loss is\n" +
			"how a script says it means it.\n\n" +
			"With an object-storage provider registered, --delete-data takes a final backup of every\n" +
			"attached database to that store first and removes NOTHING if it fails. --skip-final-backup\n" +
			"destroys the data without one — which is how a wedged instance that cannot be dumped is\n" +
			"still removable.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			// A flag that skips a step this invocation was never going to take is a misunderstanding
			// worth surfacing rather than accepting quietly — the likely reading of a lone
			// --skip-final-backup is "and destroy the data", which is not what it says.
			if skipFinalBackup && !deleteData {
				return fmt.Errorf("--skip-final-backup only applies to --delete-data: without it the data volume is kept and no backup is taken or needed")
			}
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
				if err := confirmDeleteData(ctx, c, name, skipFinalBackup, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			res, err := c.RemoveAddon(ctx, name, client.RemoveAddonOptions{
				DeleteData:        deleteData,
				SkipFinalBackup:   skipFinalBackup,
				BackupDestination: backupDestination,
				Confirm:           confirm,
			})
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, removeAddonSummary(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	cmd.Flags().BoolVar(&deleteData, "delete-data", false, "also DESTROY the add-on's data volume (for postgres: every attached app's database)")
	cmd.Flags().BoolVar(&ackDataLoss, dataLossAckFlag, false, "acknowledge that --delete-data destroys the add-on's data, so it proceeds without the typed-name prompt (this is how a script asks for it). No shorthand: destroying every attached app's database should not be a single keystroke.")
	cmd.Flags().BoolVar(&skipFinalBackup, "skip-final-backup", false, "destroy the data WITHOUT the final backup --delete-data otherwise takes first. For an instance too broken to dump — the removal output says plainly that no backup was taken.")
	cmd.Flags().StringVar(&backupDestination, "backup-destination", "",
		"the object-storage `provider` the final backup goes to (only needed when more than one is registered)")
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
	// It names the ADD-ON rather than the claim, because this refusal deliberately runs before the
	// control plane is contacted at all — so which type this instance is, and therefore what its claim
	// is called, is not a question that has been asked yet.
	return fmt.Errorf("--delete-data destroys the data volume of the add-on %q and asks for the add-on's name "+
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
func confirmDeleteData(ctx context.Context, c *client.Client, name string, skipFinalBackup bool, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, warning(out)+deleteDataConsequence(ctx, c, name, skipFinalBackup))
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
// volume-concrete message rather than make a broken add-on unremovable. The claim's name comes from
// the instance and its TYPE (ADR-0066 §1) — for postgres it is CloudNativePG's `<instance>-1`, not
// the instance's own name — and a listing that will not answer degrades to the instance name rather
// than dropping the sentence.
func deleteDataConsequence(ctx context.Context, c *client.Client, name string, skipFinalBackup bool) string {
	var b strings.Builder
	addonType, addonEnv := addonInstanceOf(ctx, c, name)
	fmt.Fprintf(&b, "--delete-data DESTROYS the data volume %q in namespace %s. This cannot be undone.",
		controlplane.AddonDataVolumeName(controlplane.AddonType(addonType), name), connect.DefaultAddonNamespace)
	if addonType != string(controlplane.AddonPostgres) {
		return b.String()
	}
	b.WriteString("\n  For the postgres add-on that volume holds every attached app's database")
	if apps := attachedApps(ctx, c); len(apps) > 0 {
		fmt.Fprintf(&b, ": %s", pluralApps(apps))
	}
	// This instance's OWN environment names the claim that survives: backups are held one claim per
	// environment (ADR-0067 §1), and naming another environment's would describe a volume this
	// removal is not touching. Best-effort like the rest of the notice — an unreadable listing costs
	// the sentence, never the removal.
	if claim, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, addonEnv); err == nil {
		fmt.Fprintf(&b, ".\n  The backup volume %q is kept — recorded backups outlive the database they came from.", claim)
	} else {
		b.WriteString(".\n  This environment's backup volume is kept — recorded backups outlive the database they came from.")
	}
	b.WriteString("\n  " + finalBackupNotice(ctx, c, skipFinalBackup))
	return b.String()
}

// finalBackupNotice is the line about the final backup in the typed-name notice (ADR-0064 §5). It is
// stated BEFORE the name is typed rather than only in the output afterwards, because it changes what
// the operator is agreeing to: "a copy is written off-cluster first and this is abandoned if that
// fails" and "this is the last moment the data exists" are different decisions.
//
// Best-effort, like everything else behind this notice: a provider listing that will not answer
// degrades to naming the uncertainty rather than asserting either answer. It is deliberately not the
// authority — burrowd decides and enforces §5 — so being unable to read the providers costs the
// notice a sentence, never the removal.
func finalBackupNotice(ctx context.Context, c *client.Client, skipFinalBackup bool) string {
	if skipFinalBackup {
		return "--skip-final-backup: NO final backup will be taken. The data is destroyed with no off-cluster copy."
	}
	stores, err := c.Providers(ctx)
	if err != nil {
		return "Whether a final backup is taken first depends on an object-storage provider being registered;\n" +
			"  the provider listing could not be read, so burrowd decides. It removes nothing if the backup fails."
	}
	var names []string
	for _, p := range stores {
		for _, cap := range p.Capabilities {
			if cap == string(controlplane.CapabilityObjectStorage) {
				names = append(names, p.Name)
				break
			}
		}
	}
	if len(names) == 0 {
		return "No object-storage provider is registered, so NO off-cluster backup is taken first —\n" +
			"  register one (`burrow config provider add --type s3`) if you want a copy to survive this."
	}
	sort.Strings(names)
	return fmt.Sprintf("A final backup of each attached database is written to the %s object store FIRST;\n"+
		"  if it does not get there, nothing is removed.", strings.Join(names, " or "))
}

// addonInstanceOf looks up the registered type and environment of an installed add-on by name,
// returning empty strings when the listing cannot be read or the name is not registered. Both are
// needed together: with an instance and a backup claim per environment (ADR-0067 §1), the type says
// what the removal destroys and what its claim is CALLED — for postgres that is CloudNativePG's
// `<instance>-1` rather than the instance's own name (ADR-0066 §1) — and the environment says which
// backup claim survives it. Best-effort by contract: an unanswerable lookup costs the notice its
// detail, never the removal.
func addonInstanceOf(ctx context.Context, c *client.Client, name string) (addonType, env string) {
	addons, err := c.Addons(ctx)
	if err != nil {
		return "", ""
	}
	for _, a := range addons {
		if a.Name == name {
			// A row written before add-ons were per-environment carries no environment and is the
			// default one's, since that is the only instance that could have existed (ADR-0067 §3).
			if a.Environment == "" {
				return a.Type, controlplane.DefaultEnvironment
			}
			return a.Type, a.Environment
		}
	}
	return "", ""
}

// attachedApps enumerates, best-effort, the apps holding a Burrow-provisioned database on the
// Postgres add-on — the concrete blast radius of a data-deleting removal. Attachment leaves no
// registry row (ADR-0064 §Context): it exists as a database on the instance and as the DATABASE_URL
// KEY in the app's Secret, and the key is the half a CLI can read. Only key NAMES are listed, never
// values (ADR-0028). Any app or listing that will not answer is skipped rather than failing the
// notice.
func attachedApps(ctx context.Context, c *client.Client) []string {
	return attachedAppsIn(ctx, c, "")
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
		// What happened to the copy comes immediately after what happened to the original, because
		// they are one fact read together (ADR-0064 §5). A skipped backup is stated as plainly as a
		// taken one: an operator who destroyed a database believing a copy exists is worse off than
		// one who knows none does.
		for _, bk := range res.FinalBackups {
			fmt.Fprintf(&b, "final backup of %q taken first: %s (%s)\n", bk.App, bk.ID, backupWhere(bk))
		}
		if res.FinalBackupSkipped {
			fmt.Fprintf(&b, "NO final backup was taken — %s\n", res.FinalBackupNote)
		}
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
