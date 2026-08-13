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
//
// WHAT it lists is the same on every target and WHO PERFORMS each one is not, which is issue #582 and
// the whole of what the target kind decides here. The agent boundary is one binary's shape: the same
// `burrow-agent` withholds the same verbs for the same reasons whether it is pointed at a self-hosted
// cluster or at the managed product. The commands in the second column are another matter — about
// half of them are refused to a managed tenant (clusteronly.go), so the catalogue was handing a
// reader a page of commands they could not run, which is the leak targethints.go closed on the
// operate verbs. The catalogue answers it in the same place the reasons live: each entry that needs
// one carries a second remedy for a managed reader (internal/agentsurface), and this command passes
// the kind of target selected.

// newAgentCapabilitiesCmd builds `burrow agent capabilities`.
func newAgentCapabilitiesCmd() *cobra.Command {
	// Only --json is bound onto it. This command connects to nothing, so the connection flags would
	// be flags with nothing to act on; what it needs from commonOpts is the one field the rest of the
	// CLI records the target kind in, so there is one way to ask that question and not two
	// (targethints.go).
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "capabilities",
		Short: "List the capabilities absent from burrow-agent, and what to run instead",
		Long: "List the capabilities absent from the `burrow-agent` binary, and who performs each one\n" +
			"instead.\n\n" +
			"These are not guardrails. A guardrail is policy an operator set and `burrow guard set` can\n" +
			"change; a capability listed here is not compiled into `burrow-agent` at all, so no disposition\n" +
			"produces it. `burrow-agent` reports the same list to the agent at runtime, which is what lets\n" +
			"the agent relay a real refusal naming who can do it instead of an unknown command.\n\n" +
			"It reads a compiled-in catalogue, so it needs no cluster and no credential.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.recordSelectedTarget()
			managed := o.onManagedTarget()
			absent := agentsurface.AbsentFromAgentSurface(managed)
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, agentsurface.NewAbsentCapabilitiesReport(absent), "")
			}
			writeAbsentCapabilities(out, absent, managed)
			return nil
		},
	}
	// --json carries the per-capability detail the two-column table leaves out: what each capability
	// is, why it is held back, and who can perform it. It is the same shape `burrow guard list --json`
	// reports under the same key.
	cmd.Flags().BoolVar(&o.json, "json", false, "emit the full detail as JSON (what each capability is, why it is held back, who can run it)")
	return cmd
}

// writeAbsentCapabilities renders the human listing: a sentence saying what the list is, then the
// capability and what happens instead, one per line.
//
// The table stays two columns so it fits a terminal; the per-capability detail the agent reads (what
// it is, why it is held back, who can do it) is in --json. An empty list says so rather than printing
// nothing, because a silent command cannot be told from a broken one.
//
// On a cluster the output is what it always was, byte for byte. On the managed product two things
// differ and the difference is deliberately small: a sentence saying the LIST is the same either way,
// so a tenant does not read a reworded second column as the agent being held tighter on the managed
// product; and the column's heading, because a row whose remedy is "the platform does it" is an
// answer rather than an instruction, and RUN INSTEAD would be the wrong word over it.
//
// No em-dashes: this is user-facing CLI output.
func writeAbsentCapabilities(w io.Writer, absent []agentsurface.Capability, managed bool) {
	if len(absent) == 0 {
		fmt.Fprintln(w, "No capabilities are absent from burrow-agent: every capability Burrow ships is on the agent surface.")
		return
	}
	fmt.Fprintf(w, "%d capabilities are absent from burrow-agent entirely. The verb is not compiled into\n", len(absent))
	fmt.Fprintln(w, "the binary, so no guardrail disposition produces it. burrow-agent reports each one to the")
	fmt.Fprintln(w, "agent with what it is and who can run it, so the agent relays a refusal instead of an")
	fmt.Fprintln(w, "unknown command. Use --json for the full detail.")
	if managed {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "This list does not depend on your target: burrow-agent is one binary and holds back the")
		fmt.Fprintln(w, "same capabilities for the same reasons everywhere. What differs on the managed product is")
		fmt.Fprintln(w, "who performs each one instead, because the cluster, the add-on instances and the backups")
		fmt.Fprintln(w, "are operated for you.")
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CAPABILITY\t"+insteadHeading(managed))
	for _, c := range absent {
		fmt.Fprintf(tw, "%s\t%s\n", c.Path, insteadCell(c))
	}
	_ = tw.Flush()
}

// insteadHeading names the second column for the kind of target the reader is on. RUN INSTEAD is
// what a self-hosted operator has always seen and every row under it is theirs to run. On the managed
// product many rows name no command at all, because the platform performs them, so the heading says
// INSTEAD: the cell is the answer to "then what happens", which is sometimes a command and sometimes
// a person.
func insteadHeading(managed bool) string {
	if managed {
		return "INSTEAD"
	}
	return "RUN INSTEAD"
}

// insteadCell is the second column's value: the command when there is one to run, and otherwise who
// performs the capability.
//
// The fallback is not a formatting nicety. A capability the platform performs has no invocation to
// print, and the alternatives were both worse than naming the person: a blank cell reads as missing
// data, and inventing a command to fill it would put a string in front of a tenant that does not
// exist. Who is always populated (agentsurface.AbsentFrom), so the cell is never empty.
func insteadCell(c agentsurface.Capability) string {
	if c.Command != "" {
		return c.Command
	}
	return c.Who
}
