// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
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
			"the same deploy records rollback uses and changes nothing.\n\n" +
			"A status of `deployed` means Burrow applied that release, which is not the same as its pods\n" +
			"having served: where the deploy waited and the rollout did not become ready, the status\n" +
			"carries the reason it did not, e.g. `deployed (not ready: CrashLoopBackOff)`.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
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
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Image, r.CreatedAt.Format("2006-01-02 15:04:05"), historyStatus(r), trigger)
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// historyStatus renders a release's STATUS cell: the recorded status, qualified by what the deploy
// observed of the rollout when the two say different things (ADR-0092 §4).
//
// The two ARE different things. `deployed` records that Burrow applied the release and that it is
// what a rollback returns to; it never meant the pods came up, and a history that shows nothing else
// invites the reading that it did — the reading issue #546 is about. A row that settled says nothing
// extra, because that is the ordinary case and a column of confirmations is noise; a row whose
// rollout did not become ready carries the reason it did not.
//
// A row with no observation at all — a deploy that declined to wait, or any release recorded before
// deploys waited — renders exactly as it always did. There is nothing to add, and inventing an
// "unknown" marker for every historical row would make an absence look like an event.
func historyStatus(r client.Release) string {
	if r.Rollout != string(controlplane.RolloutUnsettled) {
		return r.Status
	}
	reason := r.RolloutReason
	if reason == "" {
		reason = "no reason recorded"
	}
	return fmt.Sprintf("%s (not ready: %s)", r.Status, reason)
}
