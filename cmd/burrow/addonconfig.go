// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// newAddonConfigCmd is `burrow addon config`: change an add-on instance that already exists
// (ADR-0082).
//
// A SUBCOMMAND PER SETTING, NOT FLAGS ON ONE COMMAND, and the shape is the record's argument made
// visible. The settings an add-on has are specific to it — Postgres has standbys and a volume, the
// cache will have a topology — so a shared flag set would either mean different things per type or
// collapse into a `--set key=value` that validates nothing. And each of these carries a consequence
// that has to be explained where it is used: a flag gets one line of `--help`, which is not enough
// room to say that adding a standby restarts every attached app, or that a volume that grows never
// shrinks back. A subcommand gets a paragraph, its own validation, and a refusal written for the
// specific thing being refused.
//
// The TYPE is a command rather than a positional for the same reason. `addon config postgres
// standbys 1` reads as a positional followed by a setting, and cobra cannot register subcommands
// after a positional — but more to the point the settings belong to the type, so the type is where
// they hang.
//
// It is operator-only (§4): absent from `burrow-agent` entirely and reported by `guard`, because it
// PROVISIONS HARDWARE. An agent deploying an image spends nothing; an agent adding a standby or
// doubling a volume spends money on infrastructure nobody approved, and the ease of reversing the
// change does not make the spend reversible.
//
// NOTHING HERE RUNS BY ITSELF (§5). There is no threshold and no autoscaler: an add-on changing
// shape unattended is a cost event and a topology change nobody decided, at whatever hour the
// threshold was crossed.
func newAddonConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Change an add-on instance that already exists (standbys, storage)",
		Long: "config changes the shape of an add-on instance after it is installed. An instance is created\n" +
			"plain — `addon install` takes no shape flags, because the install is the one moment nobody can\n" +
			"answer whether a database will need a standby — and everything about its shape is set here,\n" +
			"when the need is real.\n\n" +
			"`burrow addon config postgres` lists what can be set and what it is set to. Each setting is\n" +
			"its own subcommand, because each carries consequences worth stating where it is used.\n\n" +
			"Growing proceeds; shrinking asks first and names the apps it affects. A volume cannot shrink\n" +
			"at all, and that is refused here rather than left to fail on the cluster.\n\n" +
			"It is operator-only: the agent CLI carries no equivalent, because changing an instance's shape\n" +
			"provisions hardware.",
	}
	cmd.AddCommand(newAddonConfigPostgresCmd())
	return cmd
}

// newAddonConfigPostgresCmd is `burrow addon config postgres [<setting> <value>]`: bare, it lists
// what the environment's Postgres instance can be told and what it is set to (ADR-0082 §1); with a
// setting subcommand it changes one.
//
// The bare listing is not decoration. An operator who does not know what an add-on can be told is
// one command away from finding out, and it is also where a change is confirmed to have landed —
// which is the only reason the settings need a listing of their own rather than living in help text.
func newAddonConfigPostgresCmd() *cobra.Command {
	o := &commonOpts{}
	var instance string
	cmd := &cobra.Command{
		Use:   "postgres",
		Short: "List what an environment's Postgres instance can be told, and what it is set to",
		Long: "postgres shows the configurable shape of one environment's Postgres instance: how many\n" +
			"standbys it runs and how big its data volume is, each with what changing it would do.\n\n" +
			"The values are read from the instance itself, not from what an install once asked for, so\n" +
			"this is also where a change is confirmed to have landed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// readClient: listing an instance's shape changes nothing, and knowing what an add-on can
			// be told is what makes the change deliberate rather than a surprise.
			c, err := o.readClient(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.AddonSettings(ctx, string(controlplane.AddonPostgres), o.env, instance)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, res, "")
			}
			fmt.Fprintf(out, "postgres instance %s (environment %s)\n\n", res.Instance, res.Environment)
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SETTING\tVALUE\tWHAT IT IS")
			for _, s := range res.Settings {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Setting, s.Value, s.Description)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			// The consequences go BELOW the table rather than in a column: they are sentences, and a
			// sentence in a tabwriter column is a line nobody reads to the end of.
			fmt.Fprintln(out)
			for _, s := range res.Settings {
				fmt.Fprintf(out, "  %s — %s\n", s.Setting, s.Consequence)
			}
			fmt.Fprintln(out)
			// Changing a setting reaches a cluster and is refused on the managed product, where the
			// instance is the platform's, so what closes the listing depends on the target
			// (targethints.go).
			fmt.Fprintln(out, addonConfigListTail(o.onManagedTarget()))
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.AddCommand(newAddonConfigStandbysCmd(), newAddonConfigStorageCmd())
	return cmd
}

// newAddonConfigStandbysCmd is `burrow addon config postgres standbys <n>` (ADR-0081, ADR-0082 §2).
//
// A STANDBY IS BOTH THINGS AT ONCE, which is why there is one setting rather than two. CloudNativePG
// runs it as a hot standby: it takes over when the primary dies AND it serves reads, so failover and
// a readable replica arrive together and there is no separate replica feature to ask for.
//
// Adding the first one restarts every attached app so it picks up the read address, and that costs
// nothing worth avoiding: an app does not benefit from a replica by existing near one — somebody has
// to route read-only queries down the second connection, which is a code change and a deploy anyway.
func newAddonConfigStandbysCmd() *cobra.Command {
	o := &commonOpts{}
	var instance string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "standbys <n>",
		Short: "Set how many standbys run beside the primary",
		Long: "standbys sets how many standby pods run beside the primary of an environment's Postgres\n" +
			"instance. A standby is a hot standby: it takes over if the primary is lost, and it serves\n" +
			"reads — so failover and a read replica come together.\n\n" +
			"ADDING THE FIRST ONE RESTARTS EVERY ATTACHED APP. Each is given a second connection string\n" +
			"pointing at the standbys, beside the one it already has, and restarted so it can read the\n" +
			"variable. Using the replica is then a change to the application's own code.\n\n" +
			"REMOVING THE LAST ONE WITHDRAWS THAT ADDRESS and restarts the apps again. Leaving it would\n" +
			"leave a variable pointing at an endpoint that resolves to nothing, which fails at the app's\n" +
			"next read rather than at the operation that caused it.\n\n" +
			"Adding a standby means the database is cloned onto it: safe, not free, and better done\n" +
			"before an incident than during one. It also costs money — a standby is a pod and a volume,\n" +
			"the most expensive thing an add-on provisions.\n\n" +
			"A standby is NOT a backup. It replicates a mistake faithfully: a DROP TABLE on the primary\n" +
			"is a DROP TABLE on the standby in the time it takes to stream.\n\n" +
			"Reducing the count asks first and names the apps it affects; raising it proceeds.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddonConfigSet(cmd, o, instance, controlplane.AddonSettingStandbys, args[0], confirm)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` to configure (default: the environment's own instance)")
	cmd.Flags().BoolVar(&confirm, "confirm", false,
		"proceed with a REDUCTION without the typed-name prompt: fewer standbys is less of the instance surviving a lost pod, and going to zero withdraws the read address from every attached app. Raising the count never consults it.")
	return cmd
}

// newAddonConfigStorageCmd is `burrow addon config postgres storage <size>` (ADR-0082 §2).
//
// GROWING IS ONE-WAY and the help says so where somebody is about to type a number. A
// PersistentVolumeClaim expands and never contracts, so an operator who over-provisions pays for it
// until the instance is rebuilt from a backup — and a shrink is refused here rather than written and
// left to fail in a `Cluster` status field.
func newAddonConfigStorageCmd() *cobra.Command {
	o := &commonOpts{}
	var instance string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "storage <size>",
		Short: "Grow the instance's data volume (this cannot be undone)",
		Long: "storage sets the size of an environment's Postgres data volume, as a Kubernetes quantity\n" +
			"(50Gi).\n\n" +
			"THIS CANNOT BE UNDONE. A volume grows and never shrinks, so a size set too high is paid for\n" +
			"until the instance is rebuilt from a backup. A smaller size than the instance already has is\n" +
			"refused outright rather than attempted.\n\n" +
			"Growing needs a storage class that allows volume expansion, which most managed providers do.\n" +
			"The database keeps serving while the volume grows.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddonConfigSet(cmd, o, instance, controlplane.AddonSettingStorage, args[0], confirm)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&instance, "name", "", "the add-on `instance` to configure (default: the environment's own instance)")
	// The flag exists on both settings so the pair reads the same way, and it does nothing here: a
	// volume shrink is a refusal rather than a confirmation, because there is nothing achievable for
	// an operator to be agreeing to.
	cmd.Flags().BoolVar(&confirm, "confirm", false,
		"has no effect on storage: a volume cannot shrink, so a smaller size is refused rather than held for confirmation")
	return cmd
}

// runAddonConfigSet is the body both setting subcommands share: read the shape, hold a shrink for the
// operator to confirm, then change it.
//
// THE PRE-FLIGHT READ IS FOR THE NOTICE, NOT FOR THE DECISION. The server reads the instance's shape
// itself and refuses an unconfirmed reduction whatever this client believes — so a value that could
// not be read here costs the prompt, never the safety. That is the same division `addon
// restore-instance` draws between the notice it prints and the guardrail the server enforces.
func runAddonConfigSet(cmd *cobra.Command, o *commonOpts, instance string, setting controlplane.AddonSetting, value string, confirm bool) error {
	ctx := cmd.Context()
	c, err := o.client(ctx, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	addon := string(controlplane.AddonPostgres)
	// THE INSTANCE THE PROMPT NAMES COMES FROM THE SERVER, not from a name composed here. An
	// environment may hold more than one instance and only the first one's name is derivable
	// (ADR-0091 §2), so the CLI reads the label off the same answer it reads the current value from —
	// which is also the read that decides whether the prompt is needed at all.
	current, label, haveCurrent := addonSettingValue(ctx, c, addon, o.env, instance, setting)
	if !confirm && haveCurrent && isStandbyReduction(setting, current, value) {
		// The typed-name gate goes to stderr so a --json run keeps a clean stdout.
		if !stdinIsTerminal(cmd.InOrStdin()) {
			return errStandbyReductionNeedsTerminal(label, current, value)
		}
		if err := confirmStandbyReduction(ctx, c, label, o.env, current, value, cmd.InOrStdin(), cmd.ErrOrStderr()); err != nil {
			return err
		}
		confirm = true
	}
	res, err := c.ConfigureAddon(ctx, addon, o.env, instance, string(setting), value, confirm)
	if err != nil {
		return err
	}
	// emitChange, not emit: this changes an instance on a target, so the result names the target it
	// changed (ADR-0078 §4) — "which control plane did that standby appear on" is not a question to
	// ask after the fact about something that costs money.
	return o.emitChange(cmd.OutOrStdout(), res, addonConfigSummary(res))
}

// addonSettingValue reads one setting's current value AND the instance's label, best-effort. An
// unreadable answer means the caller falls through to the server, which is the authority on whether a
// change is a reduction — and the prompt that would have named the instance is the thing being
// skipped, so a label nobody could read costs nothing.
func addonSettingValue(ctx context.Context, c *client.Client, addon, env, instance string, setting controlplane.AddonSetting) (value, label string, ok bool) {
	res, err := c.AddonSettings(ctx, addon, env, instance)
	if err != nil {
		return "", "", false
	}
	for _, s := range res.Settings {
		if s.Setting == string(setting) {
			return s.Value, res.Instance, s.Value != ""
		}
	}
	return "", res.Instance, false
}

// isStandbyReduction reports whether this change takes standbys away. A value that will not parse is
// not a reduction as far as this client is concerned: it is a request the server is about to refuse
// for a better reason than "could not compare it".
func isStandbyReduction(setting controlplane.AddonSetting, current, requested string) bool {
	if setting != controlplane.AddonSettingStandbys {
		return false
	}
	from, err := parseCount(current)
	if err != nil {
		return false
	}
	to, err := parseCount(requested)
	if err != nil {
		return false
	}
	return to < from
}

// parseCount reads a standby count, so the notice can tell a reduction from a raise without asking
// the server twice.
func parseCount(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// configEnvironment resolves the environment flag for the purpose of NAMING the instance in the
// notice, exactly as the physical restore's does: an unnamed environment is the default one, which
// is what the server resolves it to when exactly one is registered.
func configEnvironment(env string) string {
	if env == "" {
		return controlplane.DefaultEnvironment
	}
	return env
}

// errStandbyReductionNeedsTerminal is the refusal a reduction returns off a terminal without
// --confirm. It refuses rather than proceeding, and names the way out — the same posture
// `--delete-data` takes, for the weaker reason that this costs availability rather than data.
func errStandbyReductionNeedsTerminal(instance, from, to string) error {
	return fmt.Errorf("taking the postgres instance %q from %s to %s standbys asks for the instance's name to "+
		"be typed back, which needs an interactive terminal; re-run with --confirm to say so explicitly in a "+
		"script", instance, from, to)
}

// confirmStandbyReduction is the reduction's human gate: it prints what the change would take away
// and requires the instance's name to be typed back.
//
// THE APPS ARE NAMED, NOT COUNTED (ADR-0082 §2, borrowing ADR-0064 §2's reasoning). "3 apps are
// affected" is a number to nod at; "api, web and worker lose the read address" is the sentence that
// makes somebody stop when one of those names should not be on the list.
//
// THIS PROMPT IS FOR HUMANS AND IS NOT A SECURITY CONTROL. What keeps an agent away from this verb
// is that `addon config` is not compiled into burrow-agent at all (ADR-0082 §4, ADR-0065 §2 tier 1).
// That structural absence must never be relaxed on the grounds that "there's a confirmation anyway".
func confirmStandbyReduction(ctx context.Context, c *client.Client, instance, env, from, to string, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, warning(out)+standbyReductionConsequence(ctx, c, instance, env, from, to))
	fmt.Fprintln(out)
	typed, err := readLine(in, out, fmt.Sprintf("Type the instance's name (%s) to proceed, or anything else to abort: ", instance))
	if err != nil {
		return err
	}
	if typed != instance {
		return fmt.Errorf("aborted: %q is not the instance's name (%s); nothing was changed", typed, instance)
	}
	return nil
}

// standbyReductionConsequence renders what the reduction is about to do, in the same terms the
// server's own refusal uses so an operator reads one account rather than two differently-worded
// ones. Like every other lookup behind a notice it is best-effort: an unreadable app listing costs
// the sentence, and the server's refusal carries the authoritative list either way.
func standbyReductionConsequence(ctx context.Context, c *client.Client, instance, env, from, to string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "this takes the postgres instance %q in environment %s from %s standbys to %s.",
		instance, configEnvironment(env), from, to)
	zero, err := parseCount(to)
	toZero := err == nil && zero == 0
	if toZero {
		b.WriteString("\n  With no standby left there is nothing for the read address to resolve to, so it is\n" +
			"  REMOVED from every attached app and each one is restarted.")
	} else {
		b.WriteString("\n  Fewer standbys is less of the instance surviving the loss of a pod.")
	}
	if apps := attachedAppsIn(ctx, c, env); len(apps) > 0 {
		if toZero {
			fmt.Fprintf(&b, "\n  Attached to it: %s. Each loses its read address and is restarted.", pluralApps(apps))
		} else {
			fmt.Fprintf(&b, "\n  Attached to it: %s.", pluralApps(apps))
		}
	}
	b.WriteString("\n  The data is untouched, and backups are unaffected: a standby is availability, not recoverability.")
	return b.String()
}

// addonConfigSummary renders what a change did. It always states the values either side of it,
// because "configured the instance" leaves the only question a reader has unanswered — and it always
// says what happened to the read address, since that is the part that reached the apps.
func addonConfigSummary(res client.ConfigureAddonResult) string {
	var b strings.Builder
	if !res.Changed {
		fmt.Fprintf(&b, "the %s instance %q in environment %s already has %s %s; nothing was changed",
			res.Addon, res.Instance, res.Environment, res.Setting, res.To)
		return b.String()
	}
	fmt.Fprintf(&b, "set %s on the %s instance %q in environment %s: %s -> %s",
		res.Setting, res.Addon, res.Instance, res.Environment, res.From, res.To)
	switch res.ReadAddress.Action {
	case string(controlplane.ReadAddressWritten):
		if len(res.ReadAddress.Apps) > 0 {
			fmt.Fprintf(&b, "\nwrote the read address into %s and restarted them — routing reads to it is a change to the app's own code",
				strings.Join(res.ReadAddress.Apps, ", "))
		} else {
			b.WriteString("\nno app is attached, so no read address was written")
		}
	case string(controlplane.ReadAddressWithdrawn):
		if len(res.ReadAddress.Apps) > 0 {
			fmt.Fprintf(&b, "\nwithdrew the read address from %s and restarted them: with no standby it resolves to nothing",
				strings.Join(res.ReadAddress.Apps, ", "))
		} else {
			b.WriteString("\nno app is attached, so there was no read address to withdraw")
		}
	}
	if res.ReadAddress.Note != "" {
		fmt.Fprintf(&b, "\n%s", res.ReadAddress.Note)
	}
	for _, s := range res.ReadAddress.Stranded {
		fmt.Fprintf(&b, "\n%q was NOT updated: %s", s.App, s.Reason)
	}
	if res.Setting == string(controlplane.AddonSettingStorage) {
		b.WriteString("\nthe volume grows in place and cannot be made smaller again")
	}
	return b.String()
}
