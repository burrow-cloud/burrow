// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// lockLong is the shared orientation for both verbs. It says what a lock stops, what it deliberately
// does not stop, and — the sentence that matters most — that it is not a security control.
// Hard-wrapped, no em-dashes: user-facing output.
const lockLong = "A lock makes destroying something take two deliberate acts.\n\n" +
	"Locked, these refuse:\n\n" +
	"  burrow app delete <app>\n" +
	"  burrow addon remove <instance>\n" +
	"  burrow addon detach <addon> <app> --delete-data\n\n" +
	"Everything else carries on exactly as before: deploys, rollbacks, scaling, restarts,\n" +
	"attaching, config changes, and an ordinary detach that keeps the data. Those all undo by\n" +
	"doing them again, and a lock that interrupted them would be one you turned off and left off.\n\n" +
	"A lock holds against everybody, including whoever set it. It is not a permission and it does\n" +
	"not ask who is calling: the mistake it exists to interrupt is running the right command\n" +
	"against the wrong app, the wrong instance, or the wrong environment.\n\n" +
	"It is NOT a security control. Anyone with write access to the namespace can delete the same\n" +
	"objects with kubectl and never touch Burrow. What a lock buys is that the path through Burrow\n" +
	"takes a separate command whose only purpose is to permit destruction.\n\n" +
	"Locking is a person's command. `burrow-agent` carries neither verb, so an agent can see that\n" +
	"something is locked and report it, and cannot unlock it."

// newLockCmd is `burrow lock <app>` and `burrow lock addon <instance>`.
func newLockCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "lock <app>",
		Short: "Lock an app so deleting it refuses until it is unlocked",
		Long:  "Lock an app so deleting it refuses until somebody unlocks it.\n\n" + lockLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.LockApp(ctx, args[0], env)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, lockHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.AddCommand(newLockAddonCmd())
	return cmd
}

// newLockAddonCmd is `burrow lock addon <instance>`. The add-on instance is in scope and not as an
// afterthought: the instance is what holds the data, where an app holds a workload a deploy can
// recreate.
func newLockAddonCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "addon <instance>",
		Short: "Lock an add-on instance so removing it refuses until it is unlocked",
		Long: "Lock an add-on instance so removing it, and detaching an app from it with --delete-data,\n" +
			"refuse until somebody unlocks it. An ordinary detach still works: it keeps the database, so\n" +
			"re-attaching gets it back.\n\n" + lockLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.LockAddon(ctx, args[0], env)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, lockHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newUnlockCmd is `burrow unlock <app>` and `burrow unlock addon <instance>`.
//
// IT TAKES NO --confirm, and that is a decision. `--confirm` catches a command nobody read; this
// command has no purpose other than to permit destruction, so a person typing it has already stated
// the intent a confirmation would ask for. Stacking one on top would make the pair ceremony, and
// ceremony is what gets switched off. Deleting a locked app still takes both an unlock and a
// confirmed delete: the two catch different mistakes and neither makes the other redundant.
func newUnlockCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "unlock <app>",
		Short: "Remove an app's lock, so it can be deleted",
		Long: "Remove an app's lock. Deleting it is then possible again, and still asks for confirmation\n" +
			"the way it always has.\n\n" +
			"This is the act the lock exists to require, so it is recorded in the audit trail. An unlock\n" +
			"with no deletion after it is a lock somebody removed and forgot to put back.\n\n" + lockLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.UnlockApp(ctx, args[0], env)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, lockHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.AddCommand(newUnlockAddonCmd())
	return cmd
}

// newUnlockAddonCmd is `burrow unlock addon <instance>`.
func newUnlockAddonCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "addon <instance>",
		Short: "Remove an add-on instance's lock, so it can be removed",
		Long: "Remove an add-on instance's lock. Removing the instance, and detaching with --delete-data,\n" +
			"are then possible again and still ask for confirmation the way they always have.\n\n" + lockLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.UnlockAddon(ctx, args[0], env)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, lockHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// lockHuman renders a lock result for a person. It states what is true now and what that stops,
// because "locked" on its own leaves somebody to guess whether their deploys still work — and the
// answer, that they do, is half the design.
func lockHuman(res client.LockResult) string {
	noun, unlock := "app", fmt.Sprintf("burrow unlock %s --env %s", res.Name, res.Environment)
	stops := "Deleting it refuses"
	if res.Subject == "addon_instance" {
		noun = "add-on instance"
		unlock = fmt.Sprintf("burrow unlock addon %s --env %s", res.Name, res.Environment)
		stops = "Removing it, and detaching with --delete-data, refuse"
	}
	if !res.Locked {
		if !res.Changed {
			return fmt.Sprintf("%s %q was not locked (environment %s). Nothing changed.", noun, res.Name, res.Environment)
		}
		return fmt.Sprintf("%s %q is unlocked (environment %s). It can be destroyed again, with the confirmation those commands already ask for.",
			noun, res.Name, res.Environment)
	}
	already := ""
	if !res.Changed {
		already = " It was already locked, and the lock keeps the time it was first set."
	}
	return fmt.Sprintf("%s %q is locked (environment %s).%s %s until somebody runs `%s`. Deploys, rollbacks, scaling and restarts are unaffected.",
		noun, res.Name, res.Environment, already, stops, unlock)
}
