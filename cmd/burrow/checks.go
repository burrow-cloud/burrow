// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// `burrow app checks` is the visible, disableable face of a Burrow-SUPPLIED default (ADR-0076 §4).
//
// ADR-0072 described the post-deploy phase as user-configured. §4 puts Burrow's own check on it, so a
// command nobody configured now runs in the user's own image after every deploy of an app with a
// database or a published port. ADR-0076's consequences say that must be visible and disableable
// rather than silent, and this command is both halves: the listing says exactly what runs and why
// Burrow believes it is a dependency, and enable/disable turn it off.
//
// Disabling is on this ADMIN CLI only. The read is a fact about the app and is on the agent surface;
// the write is a standing decision to stop verifying what Burrow handed the app, which is the same
// class of decision as setting a lifecycle hook or an auto-deploy level (ADR-0065 §1).

// checksLong is the shared orientation for `burrow app checks`. Hard-wrapped, no em-dashes:
// user-facing output.
const checksLong = "After a deploy, Burrow checks the things it provisioned for the app, from inside the app's own\n" +
	"container, with the app's own environment and credentials.\n\n" +
	"What it checks is DERIVED, never configured. Burrow attached the database and wrote the\n" +
	"connection string into the app's Secret; Burrow recorded the port the app publishes and made\n" +
	"the Service in front of it. It does not have to be told what your app depends on to verify\n" +
	"what it gave you:\n\n" +
	"  an attached database   connect with the app's own DATABASE_URL and run SELECT 1\n" +
	"  a published port       request the app's port and report the status code\n\n" +
	"The check runs in your image, which may have no shell and no client tools, so Burrow copies\n" +
	"its own static binary in through an init container. An app Burrow provisioned nothing for\n" +
	"runs no check at all, and costs nothing.\n\n" +
	"A FAILED CHECK NEVER FAILS THE DEPLOY. It does not roll back and does not retroactively fail\n" +
	"a release: the deploy has already landed by the time the check runs, and the result is\n" +
	"reported alongside it. A check that could block a deploy would be a new way for an app to\n" +
	"become undeployable during an incident, which is the one thing worth avoiding more than a\n" +
	"missed report.\n\n" +
	"This is not a readiness probe and must not become one. A probe that tested the shared\n" +
	"database would pull every replica of every app out of service the moment that database\n" +
	"blipped. The same check run once, at deploy time, catches the misconfiguration with none of\n" +
	"the blast radius."

// newAppChecksCmd groups the deploy-time dependency check operations on one app (ADR-0076 §4).
func newAppChecksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checks",
		Short: "Show or turn off the deploy-time dependency checks Burrow runs for an app",
		Long:  checksLong,
	}
	cmd.AddCommand(newAppChecksShowCmd(), newAppChecksEnableCmd(), newAppChecksDisableCmd())
	return cmd
}

// newAppChecksShowCmd reports what Burrow derived and whether it checks at all.
func newAppChecksShowCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "show <app>",
		Short: "Show what Burrow checks after a deploy of an app",
		Long:  "Show what Burrow checks after a deploy of this app, and whether it checks at all.\n\n" + checksLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Checks(ctx, args[0], env)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, res, checksHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newAppChecksDisableCmd turns the default off for one app.
func newAppChecksDisableCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "disable <app>",
		Short: "Stop running the deploy-time dependency check for an app",
		Long: "Stop running the deploy-time dependency check for this app.\n\n" +
			"Deploys are unaffected in every other way. The check never blocked one, so turning it\n" +
			"off removes a report and nothing else.\n\n" +
			"Reach for this when the report is noise rather than signal, for instance when the port\n" +
			"the app publishes deliberately does not speak HTTP. Undo it with\n" +
			"`burrow app checks enable <app>`.\n\n" + checksLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.SetChecks(ctx, args[0], env, false)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, checksHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newAppChecksEnableCmd puts an app back on the default.
func newAppChecksEnableCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "enable <app>",
		Short: "Run the deploy-time dependency check for an app again (the default)",
		Long: "Run the deploy-time dependency check for this app again.\n\n" +
			"Checking is Burrow's default, so this is only needed after `burrow app checks disable`.\n\n" + checksLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.SetChecks(ctx, args[0], env, true)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, checksHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// checksHuman renders a checks report for a person. It leads with whether the check runs, because
// that is the question the command answers, and then names each dependency with the thing Burrow
// provisioned that puts it on the list — so a reader can see the derivation rather than take it on
// trust.
func checksHuman(res client.ChecksResult) string {
	var b strings.Builder
	state := "on"
	if !res.Enabled {
		state = "off"
	}
	fmt.Fprintf(&b, "%s: deploy-time dependency checks %s (environment %q)", res.App, state, res.Environment)
	for _, d := range res.Dependencies {
		fmt.Fprintf(&b, "\n  %s: %s", d.Kind, d.Provisioned)
		switch {
		case d.EnvKey != "":
			fmt.Fprintf(&b, "\n    checked with: the app's own %s, from inside its container", d.EnvKey)
		case d.Endpoint != "":
			fmt.Fprintf(&b, "\n    checked with: a request to %s", d.Endpoint)
		}
	}
	b.WriteString("\n  a failed check is reported and never fails the deploy")
	if res.Note != "" {
		fmt.Fprintf(&b, "\n\n%s", res.Note)
	}
	return b.String()
}

// deployDependencyHuman renders the dependency-check results carried back on a deploy, for the human
// deploy output. It prints NOTHING when every check passed: a deploy that worked should not grow a
// paragraph confirming that the database Burrow attached is still there.
func deployDependencyHuman(results []client.DependencyResult) string {
	var lines []string
	for _, r := range results {
		if r.Outcome == "passed" {
			continue
		}
		line := fmt.Sprintf("  %s: %s", r.Kind, r.Outcome)
		if r.Reason != "" {
			line += " (" + r.Reason + ")"
		}
		if r.Detail != "" {
			line += "\n    " + r.Detail
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "dependency check (the deploy is live and was NOT rolled back):\n" + strings.Join(lines, "\n")
}
