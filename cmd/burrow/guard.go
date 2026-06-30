// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newGuardCmd inspects and configures the control-plane guardrail policy (ADR-0020).
// `list` is read-only; `set` is the operator's lever — there is deliberately no MCP tool
// for it, so an agent cannot change its own guardrails.
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
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the guardrails and their dispositions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			gs, err := c.Guardrails(ctx, o.env)
			if err != nil {
				return err
			}
			if o.json {
				return emit(cmd.OutOrStdout(), true, gs, "")
			}
			named := o.env != "" && o.env != "default"
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if named {
				// The SOURCE column shows whether each effective disposition is set for this
				// environment or inherited from the global policy or the built-in default.
				fmt.Fprintln(tw, "GUARDRAIL\tDISPOSITION\tSOURCE\tDESCRIPTION")
				for _, g := range gs {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", g.Code, g.Disposition, guardSourceLabel(g.Source), g.Description)
				}
			} else {
				fmt.Fprintln(tw, "GUARDRAIL\tDISPOSITION\tDESCRIPTION")
				for _, g := range gs {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", g.Code, g.Disposition, g.Description)
				}
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// guardSourceLabel renders a guardrail's source for the env-scoped listing. An env-specific override
// reads as "environment"; the inherited cases name where the value comes from so it is clear nothing
// was set for this environment.
func guardSourceLabel(source string) string {
	switch source {
	case "env":
		return "environment"
	case "global":
		return "inherited (global)"
	default:
		return "inherited (default)"
	}
}

func newGuardSetCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "set <guardrail> <allow|confirm|deny>",
		Short: "Set a guardrail's disposition",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			gs, err := c.SetGuardrail(ctx, o.env, args[0], args[1])
			if err != nil {
				return err
			}
			if o.json {
				return emit(cmd.OutOrStdout(), true, gs, "")
			}
			if o.env != "" && o.env != "default" {
				fmt.Fprintf(cmd.OutOrStdout(), "set guardrail %q to %q in environment %q\n", args[0], args[1], o.env)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "set guardrail %q to %q\n", args[0], args[1])
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
