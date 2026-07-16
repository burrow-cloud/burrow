// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
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
			"The default is off: auto-deploy is opt-in, so an app is never polled until you set a\n" +
			"level. Set minor (or patch/major) to turn it on. Use --env to set a different level per\n" +
			"environment (e.g. patch in prod, major in staging).\n\n" +
			"With one argument this shows the current level; with two it sets it. Setting the level is\n" +
			"a human action and is not available to the agent.\n\n" +
			"The show also reports, read-only, the current running version, the version auto-deploy\n" +
			"would move to within the level, and any higher version available above the level with the\n" +
			"exact deploy command to take it. If the registry cannot be listed (unreachable, a private\n" +
			"repo, or a non-semver running tag) the level is still shown with a short note. The poller\n" +
			"that actually deploys those upgrades arrives in a later phase.",
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
				return emit(cmd.OutOrStdout(), o.json, res, autoDeployShowHuman(res))
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

// autoDeployShowHuman renders the enriched auto-deploy show (ADR-0052 §2/§3): the level, the current
// running version, the version auto-deploy would move to within the level (or "up to date"), and any
// higher available upgrade above the level with the exact deploy command to take it. When the
// registry upgrade check could not run, it reports the level with a short note instead.
func autoDeployShowHuman(res client.AutoDeployResult) string {
	var b strings.Builder
	// When the safety stop turned auto-deploy off (a rollback or a manual downgrade), show the reason
	// next to the off level (ADR-0052 §5), e.g. "auto-deploy off (disabled by rollback)".
	if res.Level == "off" && res.DisabledReason != "" {
		fmt.Fprintf(&b, "%s: auto-deploy off (%s) in environment %q", res.App, res.DisabledReason, res.Env)
	} else {
		fmt.Fprintf(&b, "%s: auto-deploy %s in environment %q", res.App, res.Level, res.Env)
	}
	if res.Current != "" {
		fmt.Fprintf(&b, "\n  running: %s", res.Current)
	}
	switch {
	case res.Checked && res.Target != "":
		fmt.Fprintf(&b, "\n  auto-deploys to: %s (within the %s level)", res.Target, res.Level)
	case res.Checked:
		b.WriteString("\n  up to date within the level")
	case res.Note != "":
		fmt.Fprintf(&b, "\n  upgrade check unavailable: %s", res.Note)
	}
	if res.Upgrade != "" {
		image := res.Upgrade
		if res.Repository != "" {
			image = res.Repository + ":" + res.Upgrade
		}
		fmt.Fprintf(&b, "\n  available upgrade: %s (above the %s level) — take it with: burrow app deploy %s --image %s",
			res.Upgrade, res.Level, res.App, image)
	}
	return b.String()
}
