// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newClusterConfigCmd is `burrow cluster config`: the operational limits an operator sets
// ([ADR-0068](../../docs/adr/0068-operational-limits-are-configuration.md)). A limit is a bound —
// the replica ceiling today — and exceeding one is refused, not held for confirmation and not
// dispositioned away. That is what separates it from `burrow guard`: a guardrail answers "what
// happens when this is attempted", a limit answers "where the line is".
//
// It carries two tiers, and which one a set lands in is decided by `--env` (ADR-0068 §1/§3):
// without it the value is the CLUSTER value and applies everywhere; with it the value applies to
// that environment alone and wins over the cluster value there. Absent both, a limit reads as its
// built-in default. A limit that declares itself cluster-wide rejects `--env` rather than silently
// storing a value nothing reads.
//
// It is deliberately absent from `burrow-agent` (ADR-0068 §4, ADR-0065 tier 1) and its absence is
// asserted by the surface guard, for the reason `guard set` is: a bound the agent can raise is not
// a bound. It is NOT `burrow config`, which is the external credentials Burrow uses, nor
// `burrow app config`, which belongs to the developer and reaches the pod.
func newClusterConfigCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "config",
		Short: "Inspect and set Burrow's operational limits (list/set)",
		Long: "config is the operational limits Burrow enforces: bounds a human sets, such as the largest\n" +
			"replica count an app may be asked for. Exceeding a limit is refused; unlike a guardrail there\n" +
			"is no disposition on it and no --confirm that opens it. Raising one is how you get past it.\n\n" +
			"Limits have two tiers. Without --env a value applies to the whole cluster; with --env it\n" +
			"applies to that environment and wins over the cluster value there. A limit nobody has set\n" +
			"reads as its built-in default.\n\n" +
			"This is not `burrow config` (the credentials Burrow uses) and not `burrow app config` (the\n" +
			"config vars your app reads). It is operator-only: the agent CLI carries no equivalent.",
	}
	parent.AddCommand(newClusterConfigListCmd(), newClusterConfigSetCmd())
	return parent
}

func newClusterConfigListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the operational limits, their effective values, and where each value came from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			limits, err := c.Limits(ctx, o.env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, limits, "")
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			// SET AT names the tier the effective value came from, so an operator can see at a
			// glance which values someone chose and which are simply the built-in ones.
			fmt.Fprintln(tw, "LIMIT\tVALUE\tSET AT\tDEFAULT\tDESCRIPTION")
			for _, l := range limits {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", l.Code, l.Value, limitScopeLabel(l.Scope), l.Default, l.Description)
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// limitScopeLabel renders which tier an effective limit value came from. "default" says plainly
// that nobody has set one, rather than leaving a blank the reader has to interpret.
func limitScopeLabel(scope string) string {
	switch scope {
	case "environment":
		return "environment"
	case "cluster":
		return "cluster"
	default:
		return "built-in default"
	}
}

func newClusterConfigSetCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "set <limit> <value>",
		Short: "Set an operational limit, for the cluster or for one environment",
		Long: "set writes an operational limit's value. Without --env it is the cluster value and applies\n" +
			"everywhere; with --env it applies to that environment alone and wins over the cluster value\n" +
			"there.\n\n" +
			"A value is rejected if it is not of the limit's kind (a count, a duration) or lies outside\n" +
			"the range the limit permits, so a typo is refused rather than stored.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			limits, err := c.SetLimit(ctx, o.env, args[0], args[1])
			if err != nil {
				return err
			}
			// A limit is written into whichever cluster this command reached, so it names that
			// cluster like every other change does (ADR-0078 §4).
			human := fmt.Sprintf("set limit %q to %q for the cluster", args[0], args[1])
			if o.env != "" && o.env != "default" {
				human = fmt.Sprintf("set limit %q to %q in environment %q", args[0], args[1], o.env)
			}
			return o.emitChange(cmd.OutOrStdout(), limits, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
