// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newHookCmd groups an app's lifecycle hooks (ADR-0072): the commands Burrow runs at a named moment
// in a deploy or a rollback. It is ONE command with the phase named rather than a command per phase,
// and the phase names say when the command runs rather than what it is for.
//
// Setting a hook is an operator action, not an agent one: a pre-deploy hook runs a command on every
// deploy of the app, so it is configuration with the blast radius of the app itself. It sits on this
// CLI beside the auto-deploy level for the same reason.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Manage the commands an app runs around a deploy or a rollback",
		Long: "hook manages the commands Burrow runs at a named moment in an app's life. Each hook is\n" +
			"named for WHEN it runs:\n\n" +
			"  pre-deploy    runs before a deploy's new image reaches the cluster, FROM that new image.\n" +
			"                It runs on every deploy, including automated ones (auto-deploy), which is\n" +
			"                the point: it is how a schema change ships with the code that needs it.\n" +
			"                If it fails, the deploy does not happen and the running version keeps\n" +
			"                serving on the old schema.\n" +
			"  pre-rollback  runs before a rollback puts the older image back, FROM the image being\n" +
			"                rolled back AWAY from, because that is where the code that knows how to\n" +
			"                undo its own migration lives. Unset by default, and leaving it unset is\n" +
			"                the safe choice for anyone who migrates forward only.\n\n" +
			"A hook is a command in the app's own image, with the app's config and secrets, exactly as\n" +
			"\"burrow app run\" is. Burrow does not understand migrations: it runs your command, and\n" +
			"versioning and ordering stay your migration tool's job.\n\n" +
			"Pass the command after a -- separator:\n" +
			"  burrow app hook set web --on pre-deploy -- ./manage.py migrate",
	}
	cmd.AddCommand(newHookSetCmd(), newHookListCmd(), newHookUnsetCmd())
	return cmd
}

// hookPhaseFlagUsage describes --on once, so set and unset cannot drift on what a phase is.
const hookPhaseFlagUsage = "when the command runs: pre-deploy (before a deploy's new image rolls out) or pre-rollback (before a rollback puts the older image back)"

func newHookSetCmd() *cobra.Command {
	o := &commonOpts{}
	var phase string
	cmd := &cobra.Command{
		Use:   "set <app> --on <phase> -- command args...",
		Short: "Set the command an app runs at a phase",
		Long: "set stores the command to run at a phase, replacing whatever was set there before.\n\n" +
			"The command runs as a one-off Job in the app's own image, in the app's namespace, with the\n" +
			"app's config and secrets injected exactly as the running app sees them. A pre-deploy hook\n" +
			"runs from the image being deployed; a pre-rollback hook runs from the image being rolled\n" +
			"back away from.\n\n" +
			"A pre-deploy hook runs on EVERY deploy of this app in this environment, automated ones\n" +
			"included, and a command that starts failing blocks deploys until you change or remove it.",
		// Exactly one positional (the app name) before any --; everything after -- is the command.
		Args: func(cmd *cobra.Command, args []string) error {
			n := len(args)
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				n = d
			}
			if n != 1 {
				return fmt.Errorf("expected exactly one app name before --, got %d", n)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			var command []string
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				command = args[d:]
			}
			if len(command) == 0 {
				return errors.New("a command is required after --, e.g. `burrow app hook set web --on pre-deploy -- ./manage.py migrate`")
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			hook, err := c.SetHook(ctx, app, env, phase, command)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("set the %s hook on %s: %s", hook.Phase, app, strings.Join(hook.Command, " "))
			return emit(cmd.OutOrStdout(), o.json, hook, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&phase, "on", "", hookPhaseFlagUsage)
	_ = cmd.MarkFlagRequired("on")
	return cmd
}

func newHookUnsetCmd() *cobra.Command {
	o := &commonOpts{}
	var phase string
	cmd := &cobra.Command{
		Use:   "unset <app> --on <phase>",
		Short: "Remove an app's hook at a phase",
		Long: "unset removes the command set at a phase. Afterwards that phase runs nothing, which is\n" +
			"exactly how an app with no hook behaves. Unsetting a phase that has no hook succeeds.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := c.UnsetHook(ctx, app, env, phase); err != nil {
				return err
			}
			human := fmt.Sprintf("removed the %s hook from %s; that phase now runs nothing", phase, app)
			return emit(cmd.OutOrStdout(), o.json, map[string]string{"app": app, "phase": phase}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&phase, "on", "", hookPhaseFlagUsage)
	_ = cmd.MarkFlagRequired("on")
	return cmd
}

func newHookListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list <app>",
		Short: "List an app's hooks",
		Long: "list shows which phases have a command set, in the order they would fire. A phase that is\n" +
			"not listed has no hook and runs nothing.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			hooks, err := c.Hooks(ctx, args[0], env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, hooks, "")
			}
			if len(hooks) == 0 {
				fmt.Fprintf(out, "no hooks set on %s; nothing runs around a deploy or a rollback\n", args[0])
				return nil
			}
			for _, h := range hooks {
				fmt.Fprintf(out, "%s\t%s\n", h.Phase, strings.Join(h.Command, " "))
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
