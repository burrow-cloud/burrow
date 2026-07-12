// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newHistoryCmd is the read-only deploy timeline for an app: the releases recorded for it, newest
// first — what versions it has been rolled to, when, and whether each landed (ADR-0007). It reads
// the deploy records the control plane already writes; it records nothing and changes nothing. It
// lives under `app` (ADR-0024) because it is scoped to one application.
func newHistoryCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "history <app>",
		Short: "Show an app's deploy timeline: the versions it has been rolled to, when, and whether each landed",
		Long: "history shows an app's deploy timeline: every release recorded for it, newest first — the\n" +
			"image (version) each deploy rolled to, when it was recorded, and its status (deployed,\n" +
			"superseded, failed, or pending), which conveys whether it landed. It is read-only; it reads\n" +
			"the same deploy records rollback uses and changes nothing.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			releases, err := c.History(ctx, args[0], env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, releases, "")
			}
			if len(releases) == 0 {
				fmt.Fprintf(out, "No releases recorded for %s. Deploy one with `burrow app deploy %s --image <ref>`.\n", args[0], args[0])
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tWHEN\tSTATUS\tTRIGGER")
			for _, r := range releases {
				// An auto deploy (ADR-0052 §5) shows the level it ran under, e.g. "auto (minor)"; a
				// manual deploy shows "manual". Rows written before provenance existed render blank.
				trigger := r.Trigger
				if r.Trigger == "auto" && r.AutoLevel != "" {
					trigger = fmt.Sprintf("auto (%s)", r.AutoLevel)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Image, r.CreatedAt.Format("2006-01-02 15:04:05"), r.Status, trigger)
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
