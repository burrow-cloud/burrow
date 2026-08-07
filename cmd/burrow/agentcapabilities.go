// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// `burrow agent capabilities` lists the capabilities absent from the `burrow-agent` binary
// ([ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §7): the verbs that are not
// compiled into the agent's control channel at all, and the operator command that performs each one
// instead.
//
// It is a command of its own because the answer is a kind of its own. `guard list` used to print
// this under the dispositions, which said the two were halves of one setting (issue #445). A
// disposition is POLICY: an operator chose it and `guard set` can change it. An absent capability is
// SHAPE: no disposition will produce the verb, because the verb is not there. Removing it from
// `guard list` without giving it a home would have been a regression instead of a fix, since ADR-0065
// §7 requires the boundary to be legible rather than a dead end, so it moved here — beside the other
// `burrow agent` verbs, where somebody debugging "why did my agent say it cannot do that" looks.
//
// It reads a compiled-in catalogue and needs no cluster, no control plane, and no credential, so it
// answers on a machine where nothing is installed yet.

// newAgentCapabilitiesCmd builds `burrow agent capabilities`.
func newAgentCapabilitiesCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "capabilities",
		Short: "List the capabilities absent from burrow-agent, and what to run instead",
		Long: "List the capabilities absent from the `burrow-agent` binary, and the operator command that\n" +
			"performs each one instead.\n\n" +
			"These are not guardrails. A guardrail is policy an operator set and `burrow guard set` can\n" +
			"change; a capability listed here is not compiled into `burrow-agent` at all, so no disposition\n" +
			"produces it. `burrow-agent` reports the same list to the agent at runtime, which is what lets\n" +
			"the agent relay a real refusal naming who can do it instead of an unknown command.\n\n" +
			"It reads a compiled-in catalogue, so it needs no cluster and no credential.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			absent := agentsurface.AbsentFromAgentSurface()
			out := cmd.OutOrStdout()
			if asJSON {
				return emit(out, true, agentsurface.NewAbsentCapabilitiesReport(absent), "")
			}
			writeAbsentCapabilities(out, absent)
			return nil
		},
	}
	// --json carries the per-capability detail the two-column table leaves out: what each capability
	// is, why it is held back, and who can perform it. It is the same shape `burrow guard list --json`
	// reports under the same key.
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the full detail as JSON (what each capability is, why it is held back, who can run it)")
	return cmd
}

// writeAbsentCapabilities renders the human listing: a sentence saying what the list is, then the
// capability and the command to run instead, one per line.
//
// The table stays two columns so it fits a terminal; the per-capability detail the agent reads (what
// it is, why it is held back, who can do it) is in --json. An empty list says so rather than printing
// nothing, because a silent command cannot be told from a broken one.
//
// No em-dashes: this is user-facing CLI output.
func writeAbsentCapabilities(w io.Writer, absent []agentsurface.Capability) {
	if len(absent) == 0 {
		fmt.Fprintln(w, "No capabilities are absent from burrow-agent: every capability Burrow ships is on the agent surface.")
		return
	}
	fmt.Fprintf(w, "%d capabilities are absent from burrow-agent entirely. The verb is not compiled into\n", len(absent))
	fmt.Fprintln(w, "the binary, so no guardrail disposition produces it. burrow-agent reports each one to the")
	fmt.Fprintln(w, "agent with what it is and who can run it, so the agent relays a refusal instead of an")
	fmt.Fprintln(w, "unknown command. Use --json for the full detail.")
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CAPABILITY\tRUN INSTEAD")
	for _, c := range absent {
		fmt.Fprintf(tw, "%s\t%s\n", c.Path, c.Command)
	}
	_ = tw.Flush()
}
