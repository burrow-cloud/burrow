// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// newAuditCmd is the read path over the control plane's append-only audit log (ADR-0027): the
// durable record of guarded, mutating operations and the guardrail decisions that applied. It is
// top level rather than under `app` because it spans every app, host, and add-on. It is
// read-only — there is no way to write or alter the log through the CLI or the API.
func newAuditCmd() *cobra.Command {
	o := &commonOpts{}
	var app, operation, outcome string
	var limit int
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Review the audit log of guarded operations and guardrail decisions",
		Long: "audit lists the control plane's append-only record of guarded, mutating operations and\n" +
			"the guardrail decision and outcome for each — what was done, when, and whether a guardrail\n" +
			"allowed, held, or denied it. PRINCIPAL is who acted and CALLER is the kind of credential\n" +
			"they held (user, agent, machine); a request on the install's shared token names nobody and\n" +
			"reads shared-agent / control-plane. Newest first. Filter by app, operation, or outcome.\n" +
			"The log records redacted metadata only; it never holds an env value or a secret.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.readClient(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			entries, err := c.Audit(ctx, client.AuditFilter{App: app, Operation: operation, Outcome: outcome, Limit: limit})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, entries, "")
			}
			if len(entries) == 0 {
				fmt.Fprintln(out, "No audit records match.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			// PRINCIPAL is who acted, CALLER is what kind of credential they held (ADR-0084 §9).
			// The two sit side by side because they answer different questions and the pair is what
			// makes a row actionable: the same person's terminal and their agent are one principal
			// and two callers, so "did I do this or did my agent" is only readable with both.
			fmt.Fprintln(tw, "TIME\tOP\tTARGET\tPRINCIPAL\tCALLER\tCLIENT\tOUTCOME\tGUARDRAIL")
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					e.Timestamp.Format("2006-01-02 15:04:05"), e.Operation, dash(e.Target), dash(e.Principal), dash(e.Caller), dash(e.ClientVersion), e.Outcome, dash(e.GuardrailCode))
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&app, "app", "", "filter to one app/host/add-on target")
	cmd.Flags().StringVar(&operation, "operation", "", "filter to one operation (e.g. deploy, rollback, app_delete)")
	cmd.Flags().StringVar(&outcome, "outcome", "", "filter to one outcome (allowed, held, denied, executed, failed)")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum rows to return (default 200)")
	return cmd
}

// dash renders an empty field as "-" so a column never looks blank in the tabular output.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
