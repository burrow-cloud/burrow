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
			gs, err := c.Guardrails(ctx)
			if err != nil {
				return err
			}
			if o.json {
				return emit(cmd.OutOrStdout(), true, gs, "")
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "GUARDRAIL\tDISPOSITION\tDESCRIPTION")
			for _, g := range gs {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", g.Code, g.Disposition, g.Description)
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

func newGuardSetCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "set <guardrail> <allow|confirm|deny>",
		Short: "Set a guardrail's disposition",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			gs, err := c.SetGuardrail(ctx, args[0], args[1])
			if err != nil {
				return err
			}
			if o.json {
				return emit(cmd.OutOrStdout(), true, gs, "")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set guardrail %q to %q\n", args[0], args[1])
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}
