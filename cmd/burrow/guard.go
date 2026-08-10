// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// newGuardCmd inspects and configures the control-plane guardrail policy (ADR-0020).
// `list` is read-only; `set` is the operator's lever — `burrow-agent` deliberately carries no
// `guard set`, so an agent cannot change its own guardrails.
//
// `list --json` also carries the capabilities absent from the agent binary (ADR-0065 §7), which
// burrow-agent consumes. They are NOT in the human table: a disposition is policy an operator set
// and can change here, an absent capability is the shape of another binary, and printing the two
// together as one listing said they were halves of one setting (issue #445). The human listing
// points at `burrow agent capabilities`, which is where that list lives on its own.
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
		Short: "List the guardrails and their dispositions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// readClient, not client: reading what binds the agent is the same route on a cluster and
			// on the managed product, and it is the read a tenant most needs (cloud issue #202).
			// `guard set` below stays on client — whether a tenant may edit the policy is a product
			// decision, not this change.
			c, err := o.readClient(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			scope := client.GuardScope{Env: o.env, Name: name}
			gs, err := c.Guardrails(ctx, scope)
			if err != nil {
				return err
			}
			// The capabilities the agent binary does not carry, from the same catalogue
			// burrow-agent reports (ADR-0065 §7). This CLI is a different binary and cannot walk
			// the agent's command tree, so it reports the catalogue's declaration — which the
			// surface-guard test pins to that tree, so the two answers agree. It rides in the
			// --json report, which burrow-agent consumes; the human table below is dispositions
			// only (issue #445).
			absent := agentsurface.AbsentFromAgentSurface()
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, agentsurface.NewGuardReport(scope, gs, absent), "")
			}
			if name == "" && o.env == controlplane.DefaultEnvironment {
				writeDefaultEnvironmentPolicyNotice(out, o.env)
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			// The SOURCE column shows which tier each effective disposition came from: set for the
			// named app or add-on instance, set for this environment, or inherited from the global
			// policy or the built-in default. It is printed only when the control plane answered with
			// tiers to name (see guardSourcesReported).
			//
			// The BINDS column names the credential kind an answering disposition was bound to
			// (ADR-0094 §6), and is printed only when at least one row has one. That keeps the
			// everyday listing — an install where nothing is bound — exactly the width it was, and
			// makes a binding visible the moment one exists rather than something an operator infers
			// from a surprise later.
			sources, binds := guardSourcesReported(gs), guardBindingsReported(gs)
			fmt.Fprintln(tw, guardHeader(sources, binds))
			for _, g := range gs {
				fmt.Fprintln(tw, guardRow(g, sources, binds, name))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			writeAbsentCapabilitiesPointer(out, absent)
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	bindGuardName(cmd.Flags(), &name, "read the effective policy for")
	return cmd
}

// writeAbsentCapabilitiesPointer prints the one line that says the absent-capability list exists and
// where to read it ([ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §7).
//
// It used to be a second table under the dispositions, and that was the wrong thing for this command
// to print (issue #445). A guardrail disposition is POLICY: an operator decided it and `guard set`
// can change it. An absent capability is SHAPE: the verb is not compiled into `burrow-agent` at all,
// and no disposition will produce it. Printed side by side they read as two halves of one setting,
// and the half that is not what the command is for was the larger one — with a RUN INSTEAD column
// addressed to somebody who was not being refused anything.
//
// Nothing is lost by not printing it. The agent receives the list over the API at runtime, which is
// what makes it relay a real refusal, `--json` still carries every capability in full, and the list
// has a command of its own in `burrow agent capabilities`, which this line names. Pointing at
// `guard list --json` instead would have kept the answer inside the command it does not belong to.
//
// No em-dashes: this is user-facing CLI output.
func writeAbsentCapabilitiesPointer(w io.Writer, absent []agentsurface.Capability) {
	if len(absent) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%d capabilities are absent from burrow-agent entirely, which no disposition changes.\n", len(absent))
	fmt.Fprintln(w, "Run `burrow agent capabilities` to see them and what to run instead.")
}

// guardSourcesReported decides whether the listing carries a SOURCE column, by reading the ANSWER
// rather than re-deriving the rule that produced it.
//
// The control plane fills a guardrail's Source in exactly when there is a tier to name, and leaves
// it empty when there is not (ADR-0085 §4). The global listing is the second case, and so is the
// default environment's: `prod` is the environment install created, resolution deliberately skips
// the environment tier for it, and its policy IS the global policy rather than a layer over it
// (ADR-0067 §2). Every row of that listing therefore comes back with an empty Source, and a column
// filled from empty strings can only invent a tier the disposition did not come from.
//
// Following the response keeps the two sides from drifting: the column appears when it has
// something true to say and is absent when it does not, without this CLI holding a second copy of
// the control plane's resolution rule that could disagree with it.
func guardSourcesReported(gs []client.Guardrail) bool {
	for _, g := range gs {
		if g.Source != "" {
			return true
		}
	}
	return false
}

// guardBindingsReported decides whether the listing carries a BINDS column, by the same rule: the
// column appears when a row has something true to say and is absent otherwise (ADR-0094 §6).
//
// Most installs never bind a disposition, and on those every row comes back with an empty Binds, so
// a column of dashes would be a permanent reminder of a feature nobody used. Where one IS bound the
// column is what stops the binding from being invisible until somebody trips over it.
func guardBindingsReported(gs []client.Guardrail) bool {
	for _, g := range gs {
		if g.Binds != "" {
			return true
		}
	}
	return false
}

// guardHeader and guardRow render the listing's four possible column sets from the two optional
// columns, so a header can never disagree with the row beneath it.
func guardHeader(sources, binds bool) string {
	cols := []string{"GUARDRAIL", "DISPOSITION"}
	if sources {
		cols = append(cols, "SOURCE")
	}
	if binds {
		cols = append(cols, "BINDS")
	}
	return strings.Join(append(cols, "DESCRIPTION"), "\t")
}

func guardRow(g client.Guardrail, sources, binds bool, name string) string {
	cols := []string{g.Code, g.Disposition}
	if sources {
		cols = append(cols, guardSourceLabel(g.Source, name))
	}
	if binds {
		cols = append(cols, guardBindsLabel(g.Binds))
	}
	return strings.Join(append(cols, g.Description), "\t")
}

// guardBindsLabel renders which kind of credential a disposition binds. An unbound disposition reads
// as "everyone" rather than as a dash, because that is the fact: it applies to every caller, which is
// a statement about the policy and not a missing value.
func guardBindsLabel(binds string) string {
	if binds == "" {
		return "everyone"
	}
	return binds
}

// writeDefaultEnvironmentPolicyNotice explains a listing for the default environment, which is the
// global policy under another name (ADR-0067 §2) and is printed without a SOURCE column for that
// reason. It is the read-side counterpart of the line `guard set --env prod` prints: an operator
// who asked about one environment is told that the answer is not scoped to it, rather than left to
// infer it from a missing column.
//
// No em-dashes: this is user-facing CLI output.
func writeDefaultEnvironmentPolicyNotice(w io.Writer, env string) {
	fmt.Fprintf(w, "%s is the default environment, so this is the global policy: these dispositions\n", env)
	fmt.Fprintln(w, "apply everywhere until another environment sets its own.")
	fmt.Fprintln(w)
}

// guardSourceLabel renders a guardrail's source for a scoped listing. A disposition set for the
// named app or add-on instance reads as that name, so the row says what it is about; an env-specific
// override reads as "environment"; the inherited cases name where the value comes from so it is
// clear nothing was set for the thing that was asked about.
//
// An unset source is rendered as a dash, never as one of the inherited labels. A control plane
// reports no source when there was no tier to distinguish, which is not the same statement as
// "nothing was set for this anywhere", and a row that claimed the latter would be reporting a
// resolution that never happened.
func guardSourceLabel(source, name string) string {
	switch source {
	case "name":
		return name
	case "env":
		return "environment"
	case "global":
		return "inherited (global)"
	case "default":
		return "inherited (default)"
	default:
		return "-"
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
	var name, binds string
	cmd := &cobra.Command{
		Use:   "set <guardrail> <allow|confirm|deny>",
		Short: "Set a guardrail's disposition",
		Long: "Set a guardrail's disposition, for the whole cluster, for one environment, or for one\n" +
			"app or add-on instance in an environment. With --binds it applies to one kind of\n" +
			"credential and leaves every other caller reading the disposition underneath it.\n\n" +
			"  burrow guard set app.deploy confirm                                    every app, everywhere\n" +
			"  burrow guard set --env staging app.deploy allow                        every app in staging\n" +
			"  burrow guard set --env prod --name website app.deploy deny             one app\n" +
			"  burrow guard set --env prod --name burrow-postgres addon.remove deny   one add-on instance\n" +
			"  burrow guard set --binds agent --env prod --name website app.deploy deny\n" +
			"                                                                         one app, agents only\n\n" +
			"--name needs --env: on its own a name cannot be told apart from an environment of the\n" +
			"same name. Not every guardrail can be set for one thing; those that cannot say why.\n\n" +
			"--binds needs an install people have signed in to, because it binds the KIND of credential\n" +
			"a caller holds and the shared install token has no kind. Run `burrow auth login` first.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			gs, err := c.SetGuardrail(ctx, client.GuardScope{Env: o.env, Name: name}, binds, args[0], args[1])
			if err != nil {
				return err
			}
			// The policy is written into whichever cluster this command reached, so it names that
			// cluster like every other change does (ADR-0078 §4).
			return o.emitChange(cmd.OutOrStdout(), gs, guardSetResult(args[0], args[1], o.env, name, binds))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	bindGuardName(cmd.Flags(), &name, "set it for")
	// The kind is validated by the control plane, not here. It is a closed set of three, but whether
	// this install ISSUES any of them is a fact only the control plane holds, and a client-side check
	// on the spelling alone would refuse in one voice and accept in another.
	cmd.Flags().StringVar(&binds, "binds", "", "bind the disposition to one kind of credential (user, agent, or machine); omit it to bind every caller")
	return cmd
}

// guardSetResult is the line `guard set` prints after a successful write. It reports THE SCOPE THAT
// WAS WRITTEN, which is not always the scope that was typed.
//
// `--env prod` is the case that differs. `prod` is the default environment, and resolution
// deliberately skips the environment tier for it: with one environment there is nothing a
// per-environment override could differ from, so the default environment's policy IS the global
// policy and `guard set --env prod` and `guard set` are the same write (ADR-0067 §2). That is the
// right behaviour, but "in environment \"prod\"" describes a narrower scope than the one that
// landed, and an operator who reads it believes a later environment would start from a clean sheet.
// So the line says what was written and why, in one sentence.
//
// A name always keeps its environment, because there the environment is a segment of the policy key
// rather than a synonym for the baseline (ADR-0085 §1).
//
// A BINDING is reported as a clause on the end, and it says what the operator did NOT do as much as
// what they did: "binds agent credentials only; every other caller reads the disposition underneath
// it". An operator who binds a deny and reads back a line that says only "set to deny" has no way to
// tell it apart from the blunt write they were avoiding.
func guardSetResult(code, disposition, env, name, binds string) string {
	var where string
	switch {
	case name != "":
		where = fmt.Sprintf("set guardrail %q to %q for %q in environment %q", code, disposition, name, envOrDefault(env))
	case env == controlplane.DefaultEnvironment:
		where = fmt.Sprintf("set guardrail %q to %q globally (%s is the default environment, so its policy is the global policy)", code, disposition, env)
	case env != "":
		where = fmt.Sprintf("set guardrail %q to %q in environment %q", code, disposition, env)
	default:
		where = fmt.Sprintf("set guardrail %q to %q globally", code, disposition)
	}
	if binds == "" {
		return where
	}
	return where + fmt.Sprintf("; it binds %s credentials only, and every other caller reads the disposition underneath it", binds)
}

// envOrDefault names the environment a message refers to. The control plane refuses a name without
// an environment, so this only ever fills in a blank the server has already accepted.
func envOrDefault(env string) string {
	if env == "" {
		return controlplane.DefaultEnvironment
	}
	return env
}
