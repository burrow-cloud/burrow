// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// newGuardCmd inspects and configures the control-plane guardrail policy (ADR-0020).
// `list` is read-only; `set` is the operator's lever — `burrow-agent` deliberately carries no
// `guard set`, so an agent cannot change its own guardrails.
//
// `list` also reports the capabilities absent from the agent binary (ADR-0065 §7), which is the
// other half of the boundary: a disposition is a limit this CLI can move, an absent capability is
// one it cannot, and an operator asking "what can my agent do?" needs both.
func newGuardCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "guard",
		Short: "Inspect and configure the control-plane guardrail policy (list/set)",
	}
	parent.AddCommand(newGuardListCmd(), newGuardSetCmd())
	return parent
}

func newGuardListCmd() *cobra.Command {
	o := &commonOpts{}
	var name string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the guardrails and their dispositions, and the capabilities absent from burrow-agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			gs, err := c.Guardrails(ctx, client.GuardScope{Env: o.env, Name: name})
			if err != nil {
				return err
			}
			// The capabilities the agent binary does not carry, from the same catalogue
			// burrow-agent reports (ADR-0065 §7). This CLI is a different binary and cannot walk
			// the agent's command tree, so it reports the catalogue's declaration — which the
			// surface-guard test pins to that tree, so the two answers agree.
			absent := agentsurface.AbsentFromAgentSurface()
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, agentsurface.NewGuardReport(gs, absent), "")
			}
			named := name != "" || (o.env != "" && o.env != "default")
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			if named {
				// The SOURCE column shows which tier each effective disposition came from: set for
				// the named app or add-on instance, set for this environment, or inherited from the
				// global policy or the built-in default.
				fmt.Fprintln(tw, "GUARDRAIL\tDISPOSITION\tSOURCE\tDESCRIPTION")
				for _, g := range gs {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", g.Code, g.Disposition, guardSourceLabel(g.Source, name), g.Description)
				}
			} else {
				fmt.Fprintln(tw, "GUARDRAIL\tDISPOSITION\tDESCRIPTION")
				for _, g := range gs {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", g.Code, g.Disposition, g.Description)
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			writeAbsentCapabilities(out, absent)
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	bindGuardName(cmd.Flags(), &name, "read the effective policy for")
	return cmd
}

// writeAbsentCapabilities prints the capabilities the `burrow-agent` binary does not carry, under
// the guardrail table ([ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §7).
//
// It is the operator's side of the same answer the agent gets. Two limits govern what an agent can
// do and they are not the same: a guardrail disposition above, which this CLI can change with
// `guard set`, and a capability that is simply not compiled into the agent binary, which it
// cannot. Showing them together is how an operator sees the whole boundary in one place, and the
// RUN INSTEAD column is what a human does when the agent relays "that is not something I can do".
//
// The human table stays two columns so it fits a terminal; the per-capability detail the agent
// reads (what it is, why it is held back, who can do it) is in the --json report. No em-dashes:
// this is user-facing CLI output.
func writeAbsentCapabilities(w io.Writer, absent []agentsurface.Capability) {
	if len(absent) == 0 {
		return
	}
	fmt.Fprintf(w, "\nAbsent from burrow-agent: %d capabilities the agent binary cannot express.\n", len(absent))
	fmt.Fprintln(w, "It reports each one to the agent with what it is and who can run it, so the agent")
	fmt.Fprintln(w, "relays a refusal instead of an unknown command. Use --json for the full detail.")
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CAPABILITY\tRUN INSTEAD")
	for _, c := range absent {
		fmt.Fprintf(tw, "%s\t%s\n", c.Path, c.Command)
	}
	_ = tw.Flush()
}

// guardSourceLabel renders a guardrail's source for a scoped listing. A disposition set for the
// named app or add-on instance reads as that name, so the row says what it is about; an env-specific
// override reads as "environment"; the inherited cases name where the value comes from so it is
// clear nothing was set for the thing that was asked about.
func guardSourceLabel(source, name string) string {
	switch source {
	case "name":
		return name
	case "env":
		return "environment"
	case "global":
		return "inherited (global)"
	default:
		return "inherited (default)"
	}
}

// bindGuardName registers the --name flag shared by `guard list` and `guard set`. One flag covers
// both kinds of target because the guardrail code already says which kind it is: an application for
// the app.* codes, an add-on instance for the addon.* ones (ADR-0085 §1).
func bindGuardName(flags *pflag.FlagSet, name *string, verb string) {
	flags.StringVar(name, "name", "", "app or add-on instance to "+verb+" (requires --env; the guardrail decides which kind of name it is)")
}

func newGuardSetCmd() *cobra.Command {
	o := &commonOpts{}
	var name string
	cmd := &cobra.Command{
		Use:   "set <guardrail> <allow|confirm|deny>",
		Short: "Set a guardrail's disposition",
		Long: "Set a guardrail's disposition, for the whole cluster, for one environment, or for one\n" +
			"app or add-on instance in an environment.\n\n" +
			"  burrow guard set app.deploy confirm                                    every app, everywhere\n" +
			"  burrow guard set --env staging app.deploy allow                        every app in staging\n" +
			"  burrow guard set --env prod --name website app.deploy deny             one app\n" +
			"  burrow guard set --env prod --name burrow-postgres addon.remove deny   one add-on instance\n\n" +
			"--name needs --env: on its own a name cannot be told apart from an environment of the\n" +
			"same name. Not every guardrail can be set for one thing; those that cannot say why.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			gs, err := c.SetGuardrail(ctx, client.GuardScope{Env: o.env, Name: name}, args[0], args[1])
			if err != nil {
				return err
			}
			// The policy is written into whichever cluster this command reached, so it names that
			// cluster like every other change does (ADR-0078 §4).
			human := fmt.Sprintf("set guardrail %q to %q", args[0], args[1])
			switch {
			case name != "":
				human = fmt.Sprintf("set guardrail %q to %q for %q in environment %q", args[0], args[1], name, envOrDefault(o.env))
			case o.env != "" && o.env != "default":
				human = fmt.Sprintf("set guardrail %q to %q in environment %q", args[0], args[1], o.env)
			}
			return o.emitChange(cmd.OutOrStdout(), gs, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	bindGuardName(cmd.Flags(), &name, "set it for")
	return cmd
}

// envOrDefault names the environment a message refers to. The control plane refuses a name without
// an environment, so this only ever fills in a blank the server has already accepted.
func envOrDefault(env string) string {
	if env == "" {
		return "prod"
	}
	return env
}
