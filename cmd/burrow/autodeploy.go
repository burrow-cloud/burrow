// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/controlplane"
)

// newAutoDeployCmd shows or sets an app's per-environment auto-deploy level (ADR-0052 §6). Setting
// the level is a governance decision, so it is a human operator action on this CLI only; there is
// deliberately no burrow-agent verb for it, so the agent cannot change what deploys unattended
// (ADR-0038). With one positional it shows the current level; with two it sets it.
func newAutoDeployCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "auto-deploy <app> [off|patch|minor|major]",
		Short: "Show or set an app's auto-deploy level",
		Long: "Show or set how far Burrow may auto-deploy new versions of an app's own image on its\n" +
			"own (ADR-0052). The level caps auto-deploy to a semver range, per environment:\n\n" +
			"  off    nothing auto-deploys; explicit deploys only (also how you pin a version)\n" +
			"  patch  patches within the current minor (1.2.6, 1.2.7 for an app on 1.2.5)\n" +
			"  minor  any patch or minor within the current major (1.2.6, 1.3.0), never a major\n" +
			"  major  anything newer, including a breaking major (2.0.0)\n\n" +
			"The default is minor, on for every app, so an app auto-takes patches and minors within\n" +
			"its major until you dial it down. Use --env to set a different level per environment\n" +
			"(e.g. patch in prod, major in staging).\n\n" +
			"With one argument this shows the current level; with two it sets it. Setting the level is\n" +
			"a human action and is not available to the agent.\n\n" +
			"Note: surfacing an available upgrade above the level (a held 2.0.0 under minor) arrives\n" +
			"with the registry poller in a later phase; for now this shows and sets the level only.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			// Validate the level client-side for a fast, friendly error before the round trip; the
			// control plane validates again authoritatively.
			if len(args) == 2 {
				if _, err := controlplane.ParseAutoDeployLevel(args[1]); err != nil {
					return err
				}
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if len(args) == 1 {
				res, err := c.AutoDeploy(ctx, app, env)
				if err != nil {
					return err
				}
				human := fmt.Sprintf("%s: auto-deploy %s in environment %q", res.App, res.Level, res.Env)
				return emit(cmd.OutOrStdout(), o.json, res, human)
			}
			res, err := c.SetAutoDeploy(ctx, app, env, args[1])
			if err != nil {
				return err
			}
			human := fmt.Sprintf("set %s auto-deploy to %s in environment %q", res.App, res.Level, res.Env)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
