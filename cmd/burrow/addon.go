// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
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
	cmd.AddCommand(newAddonInstallCmd(), newAddonConfigCmd(), newAddonConnectCmd(), newAddonAttachCmd(), newAddonDetachCmd(), newAddonBackupCmd(), newAddonBackupInstanceCmd(), newAddonBackupsCmd(), newAddonBackupHealthCmd(), newAddonRestoreCmd(), newAddonRestoreInstanceCmd(), newAddonListCmd(), newAddonLogsCmd(), newAddonMetricsCmd(), newAddonSQLCmd(), newAddonRemoveCmd())
	return cmd
}

// newAddonBackupCmd is `burrow addon backup postgres <app>`: back up an app's database on the
// installed Postgres add-on (ADR-0032, ADR-0063 §7). burrowd runs an in-cluster Job that pg_dumps
// the database to the backup PVC and, when an object-storage destination is registered, writes it on
// to the store and reads it back before the backup is recorded as completed; no secret value crosses
// the API. Backup destroys nothing, so it is allowed by default.
func newAddonBackupCmd() *cobra.Command {
	o := &commonOpts{}
	var destination, instance string
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
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.BackupAddon(ctx, args[0], args[1], o.env, instance, destination)
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
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` to act on (default: the environment's own instance)")
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
	var destination, instance string
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
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.BackupInstance(ctx, args[0], o.env, instance, destination)
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
			c, err := o.client(ctx, cmd.ErrOrStderr())
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
			c, err := o.client(ctx, cmd.ErrOrStderr())
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
	var backup, instance string
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
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := c.RestoreAddon(ctx, args[0], args[1], backup, o.env, instance, confirm); err != nil {
				return err
			}
			human := fmt.Sprintf("restored %q from backup %s", args[1], backup)
			return o.emitChange(cmd.OutOrStdout(), map[string]string{"addon": args[0], "app": args[1], "backup": backup}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&backup, "backup", "", "the backup id to restore (from `burrow addon backups postgres <app>`)")
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` to restore into (default: the environment's own instance)")
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
	var backup, toTime, destination, instance string
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
			// THE NAME IN THE PROMPT IS THE LABEL THE OPERATOR TYPES, and it is settled without
			// contacting anything: --name when one was given, and otherwise the label of the
			// environment's FIRST instance, which is its own derived name and the one instance whose
			// label a client can know (ADR-0091 §2). Resolving it from the registry would mean
			// contacting the control plane before the off-terminal refusal below, and that refusal
			// exists precisely so a script that never said it destroys the current state cannot get
			// that far (ADR-0064 §2).
			label := instance
			if label == "" {
				derived, err := controlplane.AddonInstanceName(controlplane.AddonType(addon), restoreEnvironment(o.env))
				if err != nil {
					return err
				}
				label = derived
			}
			// The off-terminal refusal runs BEFORE anything is contacted, so a script that never said
			// it destroys the current state cannot reach the restore call at all (ADR-0064 §2).
			if !ackDataLoss && !stdinIsTerminal(cmd.InOrStdin()) {
				return errRestoreInstanceNeedsTerminal(label)
			}
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			// The typed-name gate goes to stderr so a --json run keeps a clean stdout.
			if !ackDataLoss {
				if err := confirmRestoreInstance(ctx, c, label, o.env, recoveryTargetLabel(backup, toTime), skipSafety, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			res, err := c.RestoreInstance(ctx, addon, o.env, client.RestoreInstanceOptions{
				Instance:         instance,
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
			// emitChange, not emit: this changes something on a target, so the result names the
			// target it changed (ADR-0078 §4) — and of everything the CLI can do, a rewind of a whole
			// database instance is the one where "which control plane was that?" must not be a
			// question asked afterwards.
			return o.emitChange(cmd.OutOrStdout(), res, restoreInstanceSummary(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` to rewind (default: the environment's own instance)")
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
		fmt.Fprintf(&b, "\n  On this instance: %s. All of them are rewound together, their connection string is\n"+
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
	fmt.Fprintf(&b, "rewound the %s instance %q in environment %s to %s\n", res.Addon, res.Instance, res.Environment, res.RecoveryTarget)
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
	// What the recovered instance actually holds, before who was pointed at it — a `Cluster` that came
	// up is not a restore, and this is the line that says whether one happened. It is printed on every
	// outcome, including the one where nothing could be established: an unproven restore that reads
	// like a proven one is the failure this check exists to remove.
	switch {
	case res.Verification.Status == string(controlplane.RestoreVerificationConfirmed):
		fmt.Fprintf(&b, "the recovered instance holds %d database(s) (%s), checked before any app was reconnected\n",
			len(res.Verification.Databases), strings.Join(res.Verification.Databases, ", "))
		if len(res.Verification.Missing) > 0 {
			fmt.Fprintf(&b, "no database came back for: %s — %s\n",
				strings.Join(res.Verification.Missing, ", "), res.Verification.Note)
		}
	case res.Verification.Note != "":
		fmt.Fprintf(&b, "NOT VERIFIED: %s\n", res.Verification.Note)
	default:
		// A control plane older than this check reports no verification at all. Silence would read
		// as a confirmation, so the absence is what gets said.
		b.WriteString("NOT VERIFIED: this control plane does not report whether the recovered instance holds the data\n")
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
//
// It looks for the DEFAULT variable name, which is as far as a client can get: an app may be attached
// under a name of its own (issue #462) and only the control plane holds the record of which. So an app
// attached under another name is missing from THIS sentence, and it is missing from a notice rather
// than from the operation — the server's held guardrail message enumerates the same apps from the
// recorded names and is the authoritative list the operator confirms against. Closing the gap here
// needs the attachment listing this file does not add.
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

// newAddonAttachCmd is `burrow addon attach postgres <app> [--as NAME] [--env]`: give an app its own
// database on the named environment's Postgres instance (ADR-0031/0067 §1). The caller supplies the
// add-on type, the app name, the environment, and optionally the variable to write the connection
// string into; burrowd generates it server-side and writes it into the app's Secret in that
// environment's namespace — no secret value is printed, returned, or carried over the agent control
// channel. It is held for confirmation by the addon.attach guardrail by default (ADR-0095 §1): the
// held message names the instance, the variable, and — on an app that is already attached — that the
// password is about to be rotated.
//
// It connects through changeClient, so it acts on a Burrow Cloud target as well as a cluster: giving
// an app a database is what the managed product does for a tenant, over the route every attach on the
// platform already goes through (cloud issue #215). The hold above is what the two have to say to
// each other — reaching the managed product is about which credential may ask, and the guardrail is
// about whether the ask is carried out, so it binds a tenant there exactly as it binds anyone here.
func newAddonAttachCmd() *cobra.Command {
	o := &commonOpts{}
	var as, instance string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "attach <addon> <app>",
		Short: "Attach an app to an add-on (e.g. give it a Postgres database)",
		Long: "attach gives an app its own database on an environment's Postgres instance: burrowd\n" +
			"provisions an isolated database and login role, generates the connection string server-side,\n" +
			"writes it into the app's Secret, and restarts the app. No secret value is printed or sent over\n" +
			"the agent control channel; only the key name is reported. Re-attaching rotates the password.\n" +
			"Each environment has its own instance, so --env decides which server the app is given a\n" +
			"database on; with several environments registered, naming one is required.\n\n" +
			"The variable is DATABASE_URL unless --as names another (DB_URL, PG_DSN, anything the app\n" +
			"already reads), so an app that does not follow the convention needs no wrapper copying one\n" +
			"variable to another at start-up. --as on an already-attached app MOVES the variable: the new\n" +
			"name is written and the old one removed, because the attach has just rotated the password and\n" +
			"the old name would otherwise hold a connection string that no longer works. A name something\n" +
			"else in the app's environment already answers to is refused rather than overwritten.\n\n" +
			"Attaching is held for confirmation by the addon.attach guardrail: it puts a database on a\n" +
			"server every other app in the environment shares, restarts the app, and on an app that is\n" +
			"already attached rotates its password, which nothing can undo. Re-run with --confirm to\n" +
			"proceed. An operator who wants it unattended in one environment can set\n" +
			"`burrow guard set --env <env> addon.attach allow`, which leaves the others held.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// changeClient, not client: the managed control plane serves this route, and it is how a
			// tenant's database is provisioned there (cloud issue #215).
			c, err := o.changeClient(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.AttachAddon(ctx, args[0], args[1], o.env, client.AttachAddonOptions{Instance: instance, EnvKey: as, Confirm: confirm})
			if err != nil {
				return err
			}
			human := fmt.Sprintf("attached %q to the %s add-on in environment %s\nwrote the connection string into %s's Secret under key %q (the value is never shown)",
				res.App, res.Addon, res.Environment, res.App, res.SecretKey)
			// The read address only appears where it works, so saying it was written is also saying
			// the instance has a standby (ADR-0081 §2) — which is the fact a developer needs before
			// pointing any read at it.
			if res.ReadSecretKey != "" {
				human += fmt.Sprintf("\nthis instance has a standby, so a read-only connection string was written into %q as well", res.ReadSecretKey)
			}
			if res.PreviousSecretKey != "" {
				human += fmt.Sprintf("\nremoved the previous key %q: the password was rotated, so it no longer connected", res.PreviousSecretKey)
			}
			return o.emitChange(cmd.OutOrStdout(), res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&as, "as", "", "environment variable to write the connection string into (default DATABASE_URL, or the name this attachment already uses)")
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` to attach to (default: the environment's own instance)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an attach the addon.attach guardrail holds for confirmation")
	return cmd
}

// newAddonDetachCmd is `burrow addon detach postgres <app> [--env]`: remove the variable the app's
// connection string was written under and drop the app's login role on the named environment's
// instance, KEEPING the database (ADR-0090 §1). It is held for confirmation by the addon.detach
// guardrail by default — what that guards is the app losing access to its data, which is disruptive,
// rather than the data being destroyed, which the verb no longer does.
//
// --delete-data is the separate, explicit ask that destroys the database too, and it lives on the
// operator CLI only: destroying application data is a human decision, the same line `addon remove
// --delete-data` already sits on (ADR-0090 §2, ADR-0065 §2). On top of the guardrail it carries its
// own human gate (ADR-0064 §2): on a terminal the app's name has to be typed back after a notice
// naming what goes, and off a terminal it refuses unless --acknowledge-data-loss is passed.
// --confirm satisfies a guardrail; it is not that acknowledgement, because a flag people already
// reach for reflexively is exactly the habit the gate exists to interrupt.
func newAddonDetachCmd() *cobra.Command {
	o := &commonOpts{}
	var instance string
	var confirm, deleteData, ackDataLoss bool
	cmd := &cobra.Command{
		Use:   "detach <addon> <app>",
		Short: "Detach an app from an add-on, keeping its data (e.g. drop its Postgres login role)",
		Long: "detach removes the variable the attachment was written under from the app's Secret and\n" +
			"drops the app's login role, so nothing that was issued to it still authenticates. The\n" +
			"DATABASE IS KEPT, with its rows: attaching the same app again adopts the database that is\n" +
			"there and hands the data back, which is what makes detach the inverse of attach rather than\n" +
			"a one-way door. Moving an app between instances, or taking it out of service for a while, is\n" +
			"a detach and an attach.\n\n" +
			"A kept database occupies storage nobody is using until someone removes it. `burrow addon sql`\n" +
			"can list what an instance holds.\n\n" +
			"Pass --delete-data to destroy the database as well. That prints what it would destroy and\n" +
			"asks for the app's name to be typed back; with no terminal to type into it refuses rather\n" +
			"than proceeding, and --acknowledge-data-loss is how a script says it means it.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			addon, app := args[0], args[1]
			// The off-terminal refusal runs BEFORE anything is contacted, so a script that never said
			// it destroys a database cannot reach the detach call at all (ADR-0064 §2).
			if deleteData && !ackDataLoss && !stdinIsTerminal(cmd.InOrStdin()) {
				return errDetachDeleteDataNeedsTerminal(app)
			}
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			// The typed-name gate goes to stderr so a --json run keeps a clean stdout.
			if deleteData && !ackDataLoss {
				if err := confirmDetachDeleteData(app, o.env, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			if err := c.DetachAddon(ctx, addon, app, o.env, client.DetachAddonOptions{Instance: instance, DeleteData: deleteData, Confirm: confirm}); err != nil {
				return err
			}
			human := fmt.Sprintf("detached %q from the %s add-on\nits database was destroyed, with everything in it", app, addon)
			if !deleteData {
				human = fmt.Sprintf("detached %q from the %s add-on\nits database is kept — attaching %q again picks the data back up", app, addon, app)
			}
			return o.emitChange(cmd.OutOrStdout(), map[string]any{"addon": addon, "app": app, "data_deleted": deleteData}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	cmd.Flags().BoolVar(&deleteData, "delete-data", false, "also DESTROY the app's database, with every row in it")
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` to detach from (default: the environment's own instance)")
	cmd.Flags().BoolVar(&ackDataLoss, dataLossAckFlag, false, "acknowledge that --delete-data destroys the app's database, so it proceeds without the typed-name prompt (this is how a script asks for it). No shorthand: destroying an app's data should not be a single keystroke.")
	return cmd
}

// errDetachDeleteDataNeedsTerminal is the refusal `detach --delete-data` returns off a terminal
// without the acknowledgement flag (ADR-0064 §2, ADR-0090 §2). It refuses rather than proceeding, and
// names both ways out: acknowledge the data loss explicitly, or drop the flag and keep the database.
func errDetachDeleteDataNeedsTerminal(app string) error {
	return fmt.Errorf("--delete-data destroys %q's database and asks for the app's name to be typed back, "+
		"which needs an interactive terminal; re-run with --%s to say so explicitly in a script, or drop "+
		"--delete-data to keep the database", app, dataLossAckFlag)
}

// confirmDetachDeleteData is `detach --delete-data`'s human gate: it prints what the detach would
// destroy and requires the app's name to be typed back (ADR-0064 §2). The flag says what the operator
// intends; the typed name says they read what it would do. Anything else typed, an empty line, or EOF
// aborts and nothing is detached.
//
// THIS PROMPT IS FOR HUMANS AND IS NOT A SECURITY CONTROL. Anything with a shell can type a word, so
// it adds nothing against an agent: what keeps an agent away from this is that the flag is not
// compiled into burrow-agent at all (ADR-0090 §2, ADR-0065 §2 tier 1). That structural absence must
// never be relaxed on the grounds that "there's a confirmation anyway".
//
// It names the database and the environment and nothing else, because unlike a removal there is
// nothing to look up: the blast radius of this flag is one app's database on one instance, and it is
// already named in the command that was typed.
func confirmDetachDeleteData(app, env string, in io.Reader, out io.Writer) error {
	where := "the default environment's instance"
	if env != "" {
		where = fmt.Sprintf("environment %s's instance", env)
	}
	fmt.Fprintf(out, "%s--delete-data DESTROYS %q's database on %s, with every row in it. This cannot be undone.\n", warning(out), app, where)
	fmt.Fprintln(out, "  Without it the detach takes the app's access away and keeps the database, so attaching")
	fmt.Fprintln(out, "  the app again picks the data back up.")
	fmt.Fprintln(out)
	typed, err := readLine(in, out, fmt.Sprintf("Type the app's name (%s) to proceed, or anything else to abort: ", app))
	if err != nil {
		return err
	}
	if typed != app {
		return fmt.Errorf("aborted: %q is not the app's name (%s); nothing was detached", typed, app)
	}
	return nil
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
				c, err := o.client(ctx, cmd.ErrOrStderr())
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

			c, err := o.client(ctx, cmd.ErrOrStderr())
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
			// readClient, not client: a logs query is a read the control plane answers wherever it
			// runs, and on the managed product it is the tenant's OWN logs — the platform registers a
			// logs backend for every tenant and scopes each query to that tenant's namespaces, which
			// is the surface the product is built around (cloud issue #202, cloud ADR-0013).
			c, err := o.readClient(ctx, cmd.ErrOrStderr())
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
			// readClient, for the reason `addon logs` above uses it: the query is a read, and on the
			// managed product the samples it returns are the tenant's own, scoped to their namespaces
			// by the platform-registered backend (cloud issue #202, cloud ADR-0013).
			c, err := o.readClient(ctx, cmd.ErrOrStderr())
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

// newAddonSQLCmd is `burrow addon sql <addon> <app>`: run one statement against that app's database
// on the named environment's relational add-on instance and get columns and rows back (ADR-0087).
//
// burrowd runs the statement, connecting as the APP'S OWN ROLE with the credential it already
// minted. Nothing opens a connection from this machine: there is no port-forward and no proxy, so
// this CLI never becomes a path from a laptop to tenant data — and the statement runs independently
// of the application, so a database whose app is crash-looping is still queryable, which is the case
// `burrow app run web -- psql` cannot serve.
//
// The addon.sql guardrail is DENIED by default. That is not a mistake to be worked around: there is
// no upper bound on what a statement does, and where a confirmation cannot be an informed one,
// holding for confirmation is theatre (ADR-0087 §5). An operator opens it per environment with
// `burrow guard set --env <env> addon.sql allow`.
func newAddonSQLCmd() *cobra.Command {
	o := &commonOpts{}
	var statement, file, instance string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "sql <addon> <app>",
		Short: "Run a statement against an app's database and get rows back",
		Long: "sql runs one statement against an app's database on the named environment's Postgres\n" +
			"instance and returns columns and rows: a table here, and the rows themselves under --json.\n\n" +
			"Supply the statement with -c, with --file, or on stdin. The add-on and the app together name\n" +
			"the database, the same pair attach and detach take: it targets one app's database and there is\n" +
			"no form of it that reaches the instance, another app's database, or template1.\n\n" +
			"burrowd runs it, connecting as the app's own role with the credential it already minted, so\n" +
			"the statement can touch exactly what the application can touch and nothing more. No connection\n" +
			"to the database is opened from this machine.\n\n" +
			"Burrow does NOT tell a read from a write, and does not try: a SELECT can delete, and a gate\n" +
			"labelled safe that is not is worse than no gate. One guardrail, addon.sql, gates whether the\n" +
			"statement runs at all, and it is denied by default; an operator opens it per environment with\n" +
			"`burrow guard set --env <env> addon.sql allow`.\n\n" +
			"Every run is bounded by a statement timeout and a row cap (`burrow cluster config list`), and\n" +
			"a result cut short says so rather than looking complete. The statement text is written to the\n" +
			"audit log, so a literal in a WHERE clause is recorded.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			stmt, err := readStatement(cmd, statement, file)
			if err != nil {
				return err
			}
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.AddonSQL(ctx, args[0], args[1], o.env, instance, stmt, confirm)
			if err != nil {
				return err
			}
			// emitChange, not emit, and Burrow's refusal to classify the statement is why. A `SELECT`
			// can delete, so every statement is treated as one that changed something and names the
			// target it ran against (ADR-0078 §4) — the honest reading of a verb whose effect Burrow
			// deliberately does not inspect (ADR-0087 §6).
			return o.emitChange(cmd.OutOrStdout(), res, formatSQLResult(args[1], res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVarP(&statement, "statement", "c", "", "the `SQL` to run (or use --file, or pipe it on stdin)")
	cmd.Flags().StringVar(&file, "file", "", "read the statement from a `path` instead of -c or stdin")
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` holding the database (default: the environment's own instance)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a statement a guardrail holds for confirmation (the default disposition is deny, which this does not open)")
	return cmd
}

// readStatement resolves the statement from exactly one of -c, --file, or stdin. Naming two is
// refused rather than resolved by precedence: which one won would be invisible in the output, and the
// output is a statement someone ran against a live database.
//
// With none of the three and a terminal on stdin it refuses instead of waiting, because a command
// that silently blocks reads as a hung control plane.
func readStatement(cmd *cobra.Command, statement, file string) (string, error) {
	if statement != "" && file != "" {
		return "", errors.New("pass the statement with -c or with --file, not both")
	}
	if statement != "" {
		return statement, nil
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading the statement from %s: %w", file, err)
		}
		if strings.TrimSpace(string(b)) == "" {
			return "", fmt.Errorf("%s holds no statement", file)
		}
		return string(b), nil
	}
	if stdinIsTerminal(cmd.InOrStdin()) {
		return "", errors.New("a statement is required: pass it with -c 'select …', with --file <path>, or pipe it on stdin")
	}
	b, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("reading the statement from stdin: %w", err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("a statement is required: pass it with -c 'select …', with --file <path>, or pipe it on stdin")
	}
	return string(b), nil
}

// formatSQLResult renders a statement's outcome for a human: a HEADLINE saying what ran and what
// came back, then the body — a table of rows, or the database's own error where it raised one.
//
// The headline is first because the target clause is appended to the first line (withTargetClause),
// and "which database did that statement run against" is the sentence the target belongs to.
//
// A truncated result says so and names the limit to raise, because a short answer nobody was told
// about is the failure this whole shape exists to avoid. No em-dashes: it is user-facing CLI output.
func formatSQLResult(app string, res client.SQLResult) string {
	var b strings.Builder
	if res.Error != nil {
		// The database's own words, unmodified, with the SQLSTATE that makes it identifiable.
		fmt.Fprintf(&b, "ran a statement against %s's database: the database refused it", app)
		if res.Error.SQLState != "" {
			fmt.Fprintf(&b, " (SQLSTATE %s)", res.Error.SQLState)
		}
		fmt.Fprintf(&b, "\n%s", res.Error.Message)
		if res.Error.Detail != "" {
			fmt.Fprintf(&b, "\ndetail: %s", res.Error.Detail)
		}
		if res.Error.Hint != "" {
			fmt.Fprintf(&b, "\nhint: %s", res.Error.Hint)
		}
		return b.String()
	}
	if len(res.Columns) == 0 {
		// No row set: the command tag and what it changed is the whole answer.
		fmt.Fprintf(&b, "ran a statement against %s's database: %s row(s) affected", app, strconv.FormatInt(res.RowsAffected, 10))
		if res.Command != "" {
			fmt.Fprintf(&b, " (%s)", res.Command)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "ran a statement against %s's database: %d row(s)", app, res.RowCount)
	if res.Truncated {
		fmt.Fprintf(&b, ", cut off at the row cap of %d, so there are more than these", res.RowLimit)
	}
	b.WriteString("\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(res.Columns, "\t"))
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				// A NULL and an empty string are different answers, so the table says which.
				cells[i] = "NULL"
				continue
			}
			cells[i] = *v
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
	if res.Truncated {
		fmt.Fprintf(&b, "Narrow the statement, or raise the cap with `burrow cluster config set --env %s addon.sql_rows <n>`.",
			envOrDefaultName(res.Environment))
	}
	return strings.TrimRight(b.String(), "\n")
}

// envOrDefaultName is the environment to name in a hint, falling back to a placeholder when the
// result carried none, so the printed command is never `--env ` with nothing after it.
func envOrDefaultName(env string) string {
	if env == "" {
		return "<env>"
	}
	return env
}

func newAddonInstallCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm bool
	var archiveDestination, instance string
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
			"cluster-scoped CustomResourceDefinitions and needs cluster-admin.\n\n" +
			"The result says whether the instance archives to object storage and where, and whether the\n" +
			"repository holds a base backup — archived write-ahead log cannot be restored without one. A\n" +
			"postgres instance installed before an object-storage provider was registered archives\n" +
			"nowhere: register one, then re-run this command to wire the instance to it. Re-running is\n" +
			"safe and leaves the data untouched. An instance whose repository still holds no base backup\n" +
			"is reported as such; take one with `burrow addon backup-instance postgres`.\n\n" +
			"An environment gets ONE instance by default and that is the right default — an instance is a\n" +
			"pod and a volume, and most apps do not need their own. --name stands a SECOND one up beside\n" +
			"it, for a service that wants its own blast radius, its own resource ceiling, or its own\n" +
			"upgrade schedule. The name is what you type from then on: `addon attach --name`,\n" +
			"`addon config --name`, `addon remove <name>`. Installing a name that already exists\n" +
			"re-applies that instance rather than creating a second one.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			// Checked here rather than left to the connection below, because the RBAC staging that comes
			// first writes to a cluster: by the time client() refused, a grant would already be on
			// whatever cluster the kubeconfig pointed at. The no-argument listing is refused too — it is
			// a menu of things this target cannot install, and offering it would be the misdirection
			// this refusal exists to remove.
			if err := o.requireCluster(); err != nil {
				return err
			}
			if len(args) == 0 {
				return listInstallableAddons(ctx, o, out, cmd.ErrOrStderr())
			}
			name := args[0]

			// The CLI holds the kubeconfig, so it stages the add-on's per-add-on RBAC kubeconfig-side
			// BEFORE the install API call: burrowd is forbidden from creating RBAC (least privilege),
			// so the grant the add-on needs cannot be minted server-side. Most add-ons need none and
			// this is a no-op.
			//
			// The RBAC and the API call resolve the cluster the same way, through clusterContext, and
			// that is the point rather than an implementation detail: they used to resolve it
			// differently, so on a machine where the two answers differed one add-on install wrote a
			// grant to one cluster and registered the add-on on another.
			kubeContext, controlPlaneNamespace, appNamespace, err := o.resolveAddonNamespaces(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := ensureAddonRBAC(ctx, name, o.kubeconfig, kubeContext, controlPlaneNamespace, appNamespace, out); err != nil {
				return err
			}

			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			a, err := c.InstallAddon(ctx, name, o.env, client.InstallAddonOptions{
				Name:               instance,
				Confirm:            confirm,
				ArchiveDestination: archiveDestination,
			})
			if err != nil {
				return err
			}
			return o.emitChange(out, a, addonInstallSummary(out, a))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	cmd.Flags().StringVar(&archiveDestination, "archive-destination", "",
		"the object-storage `provider` a postgres instance archives to (only needed when more than one is registered)")
	cmd.Flags().StringVar(&instance, "name", "",
		"stand a SECOND `instance` of this add-on up beside the environment's own, addressed by this name from then on. A separate pod and a separate volume, both billed.")
	return cmd
}

// addonInstallSummary renders what an install did, in the order a reader needs it (issue #466).
//
// THE BACKUP LINE IS THE REASON THIS FUNCTION EXISTS. An install either archives to object storage or
// it does not, that is decided at install time, and the output used to say neither — so an install
// with no provider registered produced the same success message as one that archives, and the
// difference surfaced months later when somebody went looking for a backup nobody had ever taken.
//
// The image moves BELOW the endpoint. It was the longest thing on the first line and the least useful
// one: nobody chose it, and it pushed the facts a reader is scanning for off the edge of a terminal.
// It stays, because an operator auditing what is running needs to be able to read it.
//
// The first line is the headline on purpose: emitChange appends the target clause to it, so what the
// install did and where it landed stay in the same breath.
func addonInstallSummary(w io.Writer, a client.Addon) string {
	lines := []string{
		fmt.Sprintf("%s installed the %s add-on %q", okMark(w), a.Type, addonLabel(a)),
		fmt.Sprintf("%s endpoint: %s", okMark(w), a.Endpoint),
	}
	lines = append(lines, addonBackupLines(w, a)...)
	lines = append(lines, "  provides: "+strings.Join(a.Capabilities, ", "))
	if a.Image != "" {
		lines = append(lines, "  image: "+a.Image)
	}
	return strings.Join(lines, "\n")
}

// addonBackupLines states what the instance does about backups, from what Burrow read back off it
// rather than from what the install meant to do.
//
// An add-on that CANNOT archive and one that merely does not are marked differently, and the
// difference is not cosmetic: a cache holding rebuildable data is working as designed, while a
// Postgres instance archiving nowhere is a database whose only copy shares a failure domain with the
// cluster. The second gets the warning label and the command that fixes it; the first gets a plain
// statement of fact.
//
// A server that reports nothing at all (an older burrowd, or a path that does not fill this in) falls
// back to the install's own note, so the output never gets quieter than it was.
func addonBackupLines(w io.Writer, a client.Addon) []string {
	b := a.Backups
	if b == nil {
		if a.Warning != "" {
			return []string{warning(w) + a.Warning}
		}
		return nil
	}
	loud := a.Type == string(controlplane.AddonPostgres)
	switch b.State {
	case client.AddonBackupsArchiving:
		out := []string{fmt.Sprintf("%s backups: archiving to %s", okMark(w), archiveRepositoryPhrase(b))}
		if line := baseBackupLine(w, b); line != "" {
			out = append(out, line)
		}
		return out
	case client.AddonBackupsUnknown:
		return []string{warning(w) + "backups: not confirmed — " + b.Detail}
	default:
		if loud {
			return []string{warning(w) + "backups: none — " + b.Detail}
		}
		return []string{"  backups: none — " + b.Detail}
	}
}

// archiveRepositoryPhrase names where an archiving instance writes, and how long what it writes is
// kept. Every part is omitted when it was not read rather than filled in with a plausible value.
func archiveRepositoryPhrase(b *client.AddonBackups) string {
	phrase := "object storage"
	if b.Provider != "" {
		phrase = fmt.Sprintf("the %q object store", b.Provider)
	}
	var detail []string
	if b.Bucket != "" {
		detail = append(detail, "bucket "+b.Bucket)
	}
	if b.RetentionDays > 0 {
		detail = append(detail, fmt.Sprintf("%d-day retention", b.RetentionDays))
	}
	if len(detail) > 0 {
		phrase += " (" + strings.Join(detail, ", ") + ")"
	}
	if s := humanBackupSchedule(b.Schedule); s != "" {
		phrase += ", base backup " + s
	}
	return phrase
}

// baseBackupLine says whether there is anything for the archived write-ahead log to be replayed onto.
// Archiving without a base backup is the failure worth a line of its own: every artifact of a working
// backup exists — a stanza, a schedule, segments arriving in the bucket — except the one that makes
// them restorable.
func baseBackupLine(w io.Writer, b *client.AddonBackups) string {
	switch b.BaseBackup {
	case client.AddonBaseBackupPresent:
		return fmt.Sprintf("%s base backup: the repository holds one, so the archived write-ahead log can be replayed onto it", okMark(w))
	case client.AddonBaseBackupRequested:
		return "  base backup: requested — " + b.Detail
	case client.AddonBaseBackupNone:
		return warning(w) + "base backup: none — " + b.Detail
	case client.AddonBaseBackupUnknown:
		return "  base backup: not confirmed — " + b.Detail
	default:
		return ""
	}
}

// humanBackupSchedule renders CloudNativePG's six-field cron as the sentence an operator is actually
// asking for — how much a restore could lose. The leading field is SECONDS, which is the difference
// from crontab and the mistake worth not making, so a schedule this does not recognise is printed
// verbatim rather than guessed at.
func humanBackupSchedule(schedule string) string {
	fields := strings.Fields(schedule)
	if len(fields) != 6 {
		if schedule == "" {
			return ""
		}
		return "on the schedule " + schedule
	}
	sec, min, hour := fields[0], fields[1], fields[2]
	daily := fields[3] == "*" && fields[4] == "*" && fields[5] == "*"
	if !daily || strings.ContainsAny(sec+min+hour, "*/,-") {
		return "on the schedule " + schedule
	}
	h, errH := strconv.Atoi(hour)
	m, errM := strconv.Atoi(min)
	if errH != nil || errM != nil {
		return "on the schedule " + schedule
	}
	return fmt.Sprintf("daily at %02d:%02d", h, m)
}

// listInstallableAddons prints the static list of installable add-ons and, when a cluster is
// reachable, which are already installed. It never fails the listing when Burrow is not installed or
// unreachable: the installable set is compiled in, so it stays useful offline (the INSTALLED column
// blanks to "-" and a hint points at `burrow cluster install`).
func listInstallableAddons(ctx context.Context, o *commonOpts, out, stderr io.Writer) error {
	installed := map[string]bool{}
	connected := false
	if c, err := o.client(ctx, stderr); err == nil {
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
		fmt.Fprintln(out, "Connect to a cluster to see which are installed (run `burrow cluster install <context>` first).")
	}
	fmt.Fprintln(out, "Install one with `burrow addon install <name>`.")
	return nil
}

// resolveAddonNamespaces resolves what the per-add-on RBAC is applied against: the kube context, the
// control-plane namespace, and the app namespace where an add-on's app-namespace RBAC (the metrics
// vmagent's pod-discovery Role) belongs.
//
// The context comes from clusterContext, which is the SAME resolution the API connection uses, so
// the grant and the registration land on one cluster. It used to come from the environment
// resolution instead, which folds in a pinned handle — and a pin naming another cluster is exactly
// how an add-on install came to write a Role to one cluster and register the add-on on a second.
// An empty context means the kubeconfig's current context, which is how connect reads it too.
//
// The namespaces come from the environment resolution, because that is what they are: a handle's
// control-plane and app namespaces, not a cluster. They are taken ONLY when that resolution landed
// on the same cluster the grant is going to. A handle describing a different cluster describes
// different namespaces, and borrowing them would be the two-cluster bug again in a quieter form —
// the right context with somebody else's namespaces. That check is ADR-0036's own rule, that a pin
// for another cluster is not a narrowing of this one, applied on the path where no target is
// selected as well as the one where a target settles it.
func (o *commonOpts) resolveAddonNamespaces(stderr io.Writer) (kubeContext, controlPlaneNamespace, appNamespace string, err error) {
	kubeContext, err = o.clusterContext(stderr)
	if err != nil {
		return "", "", "", err
	}
	controlPlaneNamespace = o.namespace
	appNamespace = connect.DefaultAppNamespace

	// Best effort from here: the namespaces have working defaults, and a config problem is not worth
	// failing an install over when the connection is about to report it far better.
	cfg, cfgErr := localconfig.Load()
	if cfgErr != nil {
		return kubeContext, controlPlaneNamespace, appNamespace, nil
	}
	resolved, resolveErr := localconfig.Resolve(cfg, o.kubeconfig)
	if resolveErr != nil {
		return kubeContext, controlPlaneNamespace, appNamespace, nil
	}
	if !sameContext(o.kubeconfig, kubeContext, resolved.Context) {
		return kubeContext, controlPlaneNamespace, appNamespace, nil
	}
	if o.namespace == "" || o.namespace == connect.DefaultNamespace {
		controlPlaneNamespace = resolved.ControlPlaneNamespace
	}
	if resolved.Namespace != "" {
		appNamespace = resolved.Namespace
	}
	return kubeContext, controlPlaneNamespace, appNamespace, nil
}

// sameContext reports whether two kube context names denote the same context, resolving either
// empty value to the kubeconfig's current context first — "" and "do-nyc3-dev" are the same cluster
// when the latter is what is current, and comparing the raw strings would call them different.
// A kubeconfig that cannot be read answers false, which costs a defaulted namespace rather than a
// grant applied somewhere unintended.
func sameContext(kubeconfig, a, b string) bool {
	if a == b {
		return true
	}
	resolvedA, err := connect.TargetContextName(kubeconfig, a)
	if err != nil {
		return false
	}
	resolvedB, err := connect.TargetContextName(kubeconfig, b)
	if err != nil {
		return false
	}
	return resolvedA == resolvedB
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
//
// It reads through readClient, so it answers on either kind of target (cloud issue #202). What the
// managed control plane returns is the tenant's own registry — the backing services the platform
// runs FOR that tenant — which is exactly the question this command asks; the refusal it replaces
// was the client reaching for a kubeconfig, not the route being absent. What differs there is the
// second half and the empty state, both of which describe a cluster the tenant does not have: see
// addonListCloudTail.
func newAddonListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed add-ons, their capabilities, and volumes kept by an earlier removal",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, cloud := activeCloudTarget(o.kubeconfig)
			c, err := o.readClient(ctx, cmd.ErrOrStderr())
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
				fmt.Fprintln(out, addonListEmpty(cloud))
			} else {
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				// NAME is the LABEL, because that is the string every other command takes; CLUSTER is
				// what the instance is called in Kubernetes. The two are the same for an environment's
				// first instance and differ for every later one, and this listing is where somebody
				// holding a generated name off `kubectl get` finds out what it is (ADR-0091 §2).
				fmt.Fprintln(tw, "NAME\tCLUSTER\tTYPE\tENV\tMODE\tENDPOINT\tCAPABILITIES")
				for _, a := range listing.Addons {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", addonLabel(a), a.Name, a.Type, addonEnvColumn(a), a.Mode, a.Endpoint, strings.Join(a.Capabilities, ","))
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			// The retained-volume section is a cluster fact and a cluster remedy: a claim in the
			// add-on namespace, still billed, reclaimed with `kubectl delete pvc`. A managed tenant has
			// neither the namespace nor the kubectl, and the platform's own claims are not theirs to
			// read, so the section is omitted there rather than printed empty.
			if cloud {
				return nil
			}
			return printRetainedVolumes(out, listing.RetainedVolumes)
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// addonLabel is what an operator addresses an instance by: its label, falling back to its cluster
// name for a row from a control plane that predates labels — which is the right answer rather than a
// defensive one, since an environment's first instance is labelled with its own name (ADR-0091 §2).
func addonLabel(a client.Addon) string {
	if a.Label != "" {
		return a.Label
	}
	return a.Name
}

// addonEnvColumn renders the environment an instance serves, with a dash for a row that names none.
// It is a column rather than something to read off a name: an environment may hold several instances
// and a name no longer says which environment it belongs to (ADR-0091 §2).
func addonEnvColumn(a client.Addon) string {
	if a.Environment == "" {
		return "-"
	}
	return a.Environment
}

// addonListEmpty is the empty-state line, which differs by target kind because the way out of it
// does. On a cluster, installing an add-on is the answer and the command that does it is the useful
// thing to print. On the managed product it is not: `addon install` refuses there, and a listing
// that answered "you have none" with a command the tenant cannot run would be the exact failure the
// managed refusals are worded to avoid. So it states what is true instead — the platform provides
// the backing services, and an empty listing means it has not registered any for this tenant yet.
func addonListEmpty(cloud bool) string {
	if cloud {
		return "No add-ons registered for your tenant. The platform provides the backing services your apps use; nothing is installed by you."
	}
	return "No add-ons installed. Install one with `burrow addon install logs`."
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
			c, err := o.client(ctx, cmd.ErrOrStderr())
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
				Environment:       o.env,
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
	// --env is how a removal that names an instance by its LABEL says which environment's it means: a
	// label is unique within an environment, not across the cluster (ADR-0091 §2). A removal that names
	// the registry name outright needs none of it.
	bindEnv(cmd.Flags(), o)
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
	addonType, _ := addonInstanceOf(ctx, c, name)
	fmt.Fprintf(&b, "--delete-data DESTROYS the data volume %q in namespace %s. This cannot be undone.",
		controlplane.AddonDataVolumeName(controlplane.AddonType(addonType), name), connect.DefaultAddonNamespace)
	if addonType != string(controlplane.AddonPostgres) {
		return b.String()
	}
	b.WriteString("\n  For the postgres add-on that volume holds every attached app's database")
	if apps := attachedApps(ctx, c); len(apps) > 0 {
		fmt.Fprintf(&b, ": %s", pluralApps(apps))
	}
	// THIS INSTANCE names the claim that survives: backups are held one claim per INSTANCE
	// (ADR-0067 §1 through ADR-0091 §4), and naming another instance's would describe a volume this
	// removal is not touching. It is composed from the instance's own name rather than from its
	// environment, which is what keeps it right for an environment holding more than one.
	// Best-effort like the rest of the notice — an unreadable listing costs the sentence, never the
	// removal.
	if claim, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, name); err == nil {
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
	// The name the operator typed, not the generated one they have never seen (ADR-0091 §2). The
	// cluster name follows it when the two differ, because it is what `kubectl` showed.
	name := res.Instance
	if name == "" {
		name = res.Name
	}
	if res.Instance != "" && res.Instance != res.Name {
		fmt.Fprintf(&b, "removed add-on %q (the cluster %s)\n", name, res.Name)
	} else {
		fmt.Fprintf(&b, "removed add-on %q\n", name)
	}
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
