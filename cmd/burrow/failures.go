// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// `burrow failures` is the command that replaces "reach for kubectl" (ADR-0074 §8). It is
// CLUSTER-WIDE by construction, because the question someone has at that moment is "what is broken",
// not "is THIS broken" — the second question already has an answer in `burrow app status`.
//
// Two things about the presentation are load-bearing rather than cosmetic.
//
// IT GROUPS BY SHARED REASON AND ORDERS OLDEST FIRST (ADR-0074 §5). One cause routinely produces
// many rows: a taint on a node pool makes every backup Job, the add-on and the collector
// unschedulable in the same minute, and a flat list of thirty red lines is a wall of text at the
// moment someone can least afford to read one. The same reason across many objects in one window is
// the signature of a common cause, and the earliest first_seen in a cascade is the likeliest thing
// to actually fix, so it leads. A burst is itself information: thirty rows in one minute is ONE
// cluster-level event and should read as one.
//
// AND IT IS A HINT, NOT A DIAGNOSIS. Grouping by shared reason is a heuristic that will sometimes
// place two unrelated crash loops side by side, and Burrow will not assert a cause it cannot verify:
// a confidently wrong root cause sends someone down the wrong path during an incident, which is
// worse than offering none. The output says so, in the output, rather than leaving the reader to
// infer how much the grouping is claiming.
//
// The grouping lives HERE and not in the API. ADR-0074 §5 keeps the wire format rows-only so the
// agent — the consumer that turns twenty rows into "the node pool was tainted at 02:14" — correlates
// on its own terms instead of inheriting a human-facing heuristic it cannot see the shape of. `--json`
// on this command prints the same rows-and-coverage the API returned, ungrouped, for the same reason.

// newFailuresCmd builds the cluster-wide failure listing.
//
// The name follows ADR-0074 §8's recommendation. It is deliberately not `burrow health`: that reads
// well as a heading and badly as a thing that lists rows, and it invites a single green/red verdict
// for a cluster — a claim Burrow does not make and could not honour, since a listing whose coverage
// has a hole in it cannot say "healthy" about the hour it did not see.
func newFailuresCmd() *cobra.Command {
	o := &commonOpts{}
	var kind, reason, env, name string
	var since time.Duration
	var all bool
	var limit int
	cmd := &cobra.Command{
		Use:   "failures",
		Short: "List what is broken across everything Burrow manages",
		Long: "failures lists what is currently broken across every object Burrow manages — apps,\n" +
			"add-ons, backups, and exposures — read from the failure ledger the control plane keeps,\n" +
			"so you do not have to reach for kubectl. Rows are grouped by shared reason and ordered\n" +
			"oldest first: one cause often produces many rows, and the earliest one in a cascade is\n" +
			"usually the thing worth fixing.\n\n" +
			"Grouping is a hint about where to look, not a diagnosis. Burrow reports what it observed\n" +
			"and never claims a cause it cannot verify.\n\n" +
			"Every answer reports the observation coverage behind it. If the observer was down for an\n" +
			"hour, that hour is shown as a gap, because an empty list is not the same fact as\n" +
			"\"nothing broke\".\n\n" +
			"By default it shows failures that are still happening. Use --since to look back over a\n" +
			"window (including ones that have since recovered), or --all for the whole retained\n" +
			"history.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			report, err := c.Failures(ctx, client.FailureQuery{
				Kind:   kind,
				Name:   name,
				Env:    env,
				Reason: reason,
				Since:  since,
				All:    all,
				Limit:  limit,
			})
			if err != nil {
				return err
			}
			// --json prints the report exactly as the API returned it: rows, not groups.
			out := cmd.OutOrStdout()
			return emit(out, o.json, report, formatFailures(out, report, time.Now()))
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&kind, "kind", "", "filter to one kind of object ("+strings.Join(client.FailureKinds(), ", ")+")")
	cmd.Flags().StringVar(&name, "name", "", "filter to one object by name")
	cmd.Flags().StringVar(&env, "env", "", "filter to one environment")
	cmd.Flags().StringVar(&reason, "reason", "", "filter to one reason (e.g. Unschedulable, CrashLoopBackOff)")
	cmd.Flags().DurationVar(&since, "since", 0, "look back over this window, including failures that have since recovered (e.g. 24h)")
	cmd.Flags().BoolVar(&all, "all", false, "include failures that have since recovered, over the whole retained history")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum rows to return (default 500)")
	return cmd
}

// failureGroup is one shared reason and the rows that carry it. It exists only for presentation —
// the rows underneath stay individually addressable, and resolving the cause resolves each on its
// own schedule (ADR-0074 §5).
type failureGroup struct {
	Reason string
	Rows   []client.Failure
}

// oldest is the group's earliest first-seen, which is both how groups are ordered and the timestamp
// worth acting on: in a cascade it is the row closest to the cause.
func (g failureGroup) oldest() time.Time { return g.Rows[0].FirstSeen }

// groupFailures groups rows by shared reason, ordering rows within a group and groups themselves
// oldest first. It re-sorts rather than trusting the server's order, so the ordering rule is a
// property of this function and can be tested as one.
func groupFailures(rows []client.Failure) []failureGroup {
	byReason := make(map[string][]client.Failure)
	for _, f := range rows {
		byReason[f.Reason] = append(byReason[f.Reason], f)
	}
	groups := make([]failureGroup, 0, len(byReason))
	for reason, rs := range byReason {
		sort.SliceStable(rs, func(i, j int) bool {
			if !rs[i].FirstSeen.Equal(rs[j].FirstSeen) {
				return rs[i].FirstSeen.Before(rs[j].FirstSeen)
			}
			return rs[i].ID < rs[j].ID
		})
		groups = append(groups, failureGroup{Reason: reason, Rows: rs})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if !groups[i].oldest().Equal(groups[j].oldest()) {
			return groups[i].oldest().Before(groups[j].oldest())
		}
		return groups[i].Reason < groups[j].Reason
	})
	return groups
}

// formatFailures renders the human listing: the coverage caveat first, then the grouped rows, then
// the reminder that the grouping is correlation. now is passed in rather than read so the relative
// ages are deterministic under test.
//
// COVERAGE LEADS. It qualifies everything below it, and a caveat printed after a list of rows is a
// caveat nobody reads at 3am.
func formatFailures(w io.Writer, report client.FailureReport, now time.Time) string {
	var b strings.Builder
	b.WriteString(formatCoverage(w, report.Coverage, now))

	groups := groupFailures(report.Failures)
	if len(groups) == 0 {
		b.WriteString("\n")
		if report.Coverage.Complete() {
			b.WriteString("No failures are recorded for this period.\n")
		} else {
			// The one sentence this surface exists to be able to say honestly.
			b.WriteString("No failures are recorded for this period — but coverage is incomplete above, so\n" +
				"that is not the same as nothing having broken.\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}

	b.WriteString(fmt.Sprintf("\n%s across %s, oldest first.\n",
		plural(len(report.Failures), "failure", "failures"), plural(len(groups), "reason", "reasons")))
	for _, g := range groups {
		b.WriteString("\n")
		b.WriteString(formatGroup(g, now))
	}
	b.WriteString("\nRows are grouped because they share a reason and a window. That is a correlation and a\n" +
		"hint about where to look, not a cause: Burrow reports what it observed and does not diagnose.\n")
	return strings.TrimRight(b.String(), "\n")
}

// formatGroup renders one shared reason: a heading that says how wide the reason reaches and when it
// started, then a line per object with its own detail underneath.
func formatGroup(g failureGroup, now time.Time) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s — %s, oldest %s\n",
		g.Reason, plural(len(g.Rows), "object", "objects"), stamp(g.oldest(), now)))

	labels := make([]string, len(g.Rows))
	width := 0
	for i, f := range g.Rows {
		labels[i] = objectLabel(f.Object)
		if len(labels[i]) > width {
			width = len(labels[i])
		}
	}
	for i, f := range g.Rows {
		b.WriteString(fmt.Sprintf("  %-*s  %-8s  %s\n", width, labels[i], failureState(f), failureTiming(f, now)))
		if f.Detail != "" {
			b.WriteString("      " + f.Detail + "\n")
		}
	}
	return b.String()
}

// formatCoverage renders what the observer was doing over the period described — the part that says
// whether the list below can be read at face value.
//
// ADR-0074's consequences name the failure this prevents: "if the observer was down from 02:00 to
// 03:00, an empty ledger for that hour reads as 'nothing broke'". So the gap is printed, the reason
// it matters is printed with it, and the case where NOTHING was ever watching is the loudest of the
// three rather than the quietest.
func formatCoverage(w io.Writer, c client.Coverage, now time.Time) string {
	var b strings.Builder
	if !c.Observed() {
		b.WriteString(fmt.Sprintf(
			"%sNo observation coverage is recorded for %s.\n"+
				"    Nothing was watching, so an empty or short list below is not evidence that nothing\n"+
				"    is broken. Check that the control plane is running (`burrow version` reports the\n"+
				"    server it reaches).\n",
			warning(w), window(c.Since, c.Until)))
		return b.String()
	}
	if len(c.Gaps) > 0 {
		b.WriteString(fmt.Sprintf("%sObservation coverage is incomplete over %s:\n", warning(w), window(c.Since, c.Until)))
		for _, g := range c.Gaps {
			b.WriteString(fmt.Sprintf("    no observations from %s to %s (%s)\n",
				g.From.Local().Format(timestampLayout), g.To.Local().Format(timestampLayout), age(g.Duration())))
		}
		b.WriteString("    Failures that began and ended inside a gap were never seen and are not listed.\n")
	} else {
		b.WriteString(fmt.Sprintf("Observed continuously over %s (%s).\n",
			window(c.Since, c.Until), plural(int(totalSweeps(c)), "sweep", "sweeps")))
	}
	if c.DegradedSweeps > 0 {
		b.WriteString(fmt.Sprintf("%s%s could not read every object they set out to, so rows for those objects may be\n"+
			"    missing or stale", warning(w), plural(int(c.DegradedSweeps), "sweep", "sweeps")))
		if c.Detail != "" {
			b.WriteString(": " + c.Detail)
		}
		b.WriteString(".\n")
	}
	return b.String()
}

// totalSweeps is how many times the observer looked over the period.
func totalSweeps(c client.Coverage) int64 {
	var n int64
	for _, w := range c.Windows {
		n += w.Sweeps
	}
	return n
}

// timestampLayout is the listing's timestamp format: local, to the second, no timezone suffix — the
// reader is looking at their own cluster's recent past, and the second is the resolution that
// matters when the question is which of two failures came first.
const timestampLayout = "2006-01-02 15:04:05"

// stamp renders an absolute instant with its age beside it, because "when did it start" is usually
// asked as "how long has this been going on".
func stamp(t, now time.Time) string {
	return fmt.Sprintf("%s (%s ago)", t.Local().Format(timestampLayout), age(now.Sub(t)))
}

// window renders the period an answer covers, as the reader asked for it ("the last 24h") rather
// than as two instants they would have to subtract.
func window(since, until time.Time) string {
	return "the last " + age(until.Sub(since))
}

// objectLabel names one managed object the way the reader thinks of it: kind, name, environment.
func objectLabel(ref client.ObjectRef) string {
	label := fmt.Sprintf("%s/%s", ref.Kind, ref.Name)
	if ref.Environment != "" {
		label += " (" + ref.Environment + ")"
	}
	return label
}

// failureState says whether the row is still happening. It is a column of its own rather than a
// footnote: a resolved row in a `--since` listing is history, and reading it as current is the
// mistake the listing must not invite.
func failureState(f client.Failure) string {
	if f.Active() {
		return "active"
	}
	return "resolved"
}

// failureTiming renders the row's lifetime: when it started, how long it ran, and how many
// observations found it present. The occurrence count read with the sweep cadence is what separates
// a blip from a standing condition.
func failureTiming(f client.Failure, now time.Time) string {
	s := fmt.Sprintf("first seen %s", stamp(f.FirstSeen, now))
	if !f.Active() {
		s += fmt.Sprintf(", resolved %s after %s", f.ResolvedAt.Local().Format(timestampLayout), age(f.ResolvedAt.Sub(f.FirstSeen)))
	}
	return s + fmt.Sprintf(", %s", plural(int(f.Occurrences), "observation", "observations"))
}

// age renders a duration the way an operator reads one: coarse, largest unit first, never more
// precision than the question deserves.
func age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch h, m := int(d.Hours()), int(d.Minutes())%60; {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour && m == 0:
		return fmt.Sprintf("%dh", h)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", h, m)
	case h%24 == 0:
		return fmt.Sprintf("%dd", h/24)
	default:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	}
}

// plural renders a count with the right noun, so no line ever reads "1 objects".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
