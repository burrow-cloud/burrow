// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

func newAppListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the apps Burrow manages",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			apps, err := c.Apps(ctx, env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, apps, "")
			}
			if len(apps) == 0 {
				// Name where it is empty. "No apps deployed." is a true sentence about the target this
				// command reached and reads as a statement about everything the person has — which is
				// exactly how it read to somebody who had just signed in to the managed product and
				// watched their cluster's apps disappear from a listing that never said it had changed
				// clusters (cloud#201). The clause is the one the rest of the CLI uses for the same
				// question, so an empty read and a deploy name the same place in the same words.
				fmt.Fprintf(out, "No apps deployed %s. Deploy one with `burrow app deploy <app> --image <ref>`.\n", o.acted.Clause())
				return nil
			}
			// Size the columns from the data through a tabwriter so a long image reference
			// keeps its gutter to the next column instead of colliding with it (#306).
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tIMAGE\tREPLICAS\tAVAILABLE\tLOCKED\tISSUE")
			for _, a := range apps {
				// Show the raw issue reason (e.g. ImagePullBackOff) so a wedged rollout is visible
				// here without opening `burrow app logs`; the full actionable message and fix live in
				// `burrow app status` and the --json output (#307).
				// LOCKED is a column rather than a footnote because a lock nobody sees is a lock
				// somebody removes and forgets to restore (cloud ADR-0060, Consequences).
				fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%t\t%s\t%s\n", a.App, a.Image, a.ReadyReplicas, a.DesiredReplicas, a.Available, lockedColumn(a.Locked), a.IssueReason)
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

func newDeployCmd() *cobra.Command {
	o := &commonOpts{}
	var image, build string
	var replicas int
	var metricsPort int
	var confirm bool
	wait := true
	cmd := &cobra.Command{
		Use:   "deploy <app> [-- command args...]",
		Short: "Deploy an app by image reference (optionally build & push first)",
		Long: "Deploy an app by image reference (optionally build & push first).\n\n" +
			"To run something other than the image's default entrypoint, pass the command after a\n" +
			"-- separator, like kubectl run:\n" +
			"  burrow app deploy worker --image myrepo/app:1.2.3 -- ./worker --queue emails\n\n" +
			"Environment configuration is set separately and is the single source of truth, sourced\n" +
			"at deploy time, set it with `burrow app config set <app> KEY=VALUE` before deploying a\n" +
			"release that needs it, so the new release boots with it on first start.\n\n" +
			"Deploy waits for the rollout and reports what it did: it exits non-zero, naming the pod's\n" +
			"own reason, when the new replicas do not become ready — Kubernetes keeps the previous\n" +
			"version serving in that case, and nothing is rolled back. Pass --wait=false to return at\n" +
			"submission instead, which reports that the outcome is unknown rather than that it worked.",
		// Exactly one positional (the app name) before any --; everything after -- overrides the
		// container command.
		Args: func(cmd *cobra.Command, args []string) error {
			n := len(args)
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				n = d
			}
			if n != 1 {
				return fmt.Errorf("expected exactly one app name, got %d", n)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			// Everything after the -- separator overrides the container's command. The engine,
			// the deploy API, and `burrow-agent deploy` already carry Command; this surfaces it on
			// the CLI so a human has the same reach the agent does.
			var command []string
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				command = args[d:]
			}
			if image == "" {
				return errors.New("--image is required")
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if build != "" {
				if err := buildAndPush(ctx, build, image, execRunner(cmd.ErrOrStderr(), cmd.ErrOrStderr())); err != nil {
					return err
				}
			}
			// The deploy reports what it is doing as it does it (issue #480). It goes to STDERR, like
			// the targeting line above it and for the same reason: stdout carries the result, and
			// --json makes that a contract.
			res, err := c.Deploy(ctx, app, client.DeployRequest{
				Env:         env,
				Image:       image,
				Command:     command,
				MetricsPort: int32(metricsPort),
				Replicas:    int32(replicas),
				Confirm:     confirm,
				NoWait:      !wait,
				Progress:    newDeployProgressPrinter(cmd.ErrOrStderr(), time.Now),
			})
			if err != nil {
				return err
			}
			human := deployHuman(app, res, o.onManagedTarget())
			// The deploy-time dependency check's result (ADR-0076 §4), printed only when something
			// did not pass. It follows the deploy line rather than replacing it, because the deploy
			// succeeded: the check is a report about a live release, not a verdict on it.
			if deps := deployDependencyHuman(res.Dependencies); deps != "" {
				human += "\n\n" + deps
			}
			if err := o.emitChange(cmd.OutOrStdout(), res, human); err != nil {
				return err
			}
			// The report is printed; the exit code carries the same verdict for whatever is reading
			// it as a process rather than as prose (ADR-0092 §2).
			return rolloutExitError(app, res.Rollout)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&image, "image", "", "container image reference to deploy (required)")
	cmd.Flags().IntVar(&replicas, "replicas", 0, "desired replicas (0 = keep current; new apps default to 1; ignored while autoscaling is enabled)")
	cmd.Flags().IntVar(&metricsPort, "metrics-port", 0, "annotate the pod so the metrics add-on scrapes /metrics on this port")
	cmd.Flags().StringVar(&build, "build", "", "build and push the image from this directory before deploying")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for the rollout and report its outcome; --wait=false returns at submission, with the outcome unknown")
	return cmd
}

func newStatusCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "status <app>",
		Short: "Show an app's release and live workload status",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Status(ctx, args[0], env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			return emit(out, o.json, res, formatStatus(out, res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

func newLogsCmd() *cobra.Command {
	o := &commonOpts{}
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Show recent logs for an app",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Print the source note and a divider to stderr up front, before the logs, so the metadata
			// leads and the log lines are the last, uninterrupted thing (a bottom note would be missed,
			// and never appear at all once these logs are streamed/followed). `app logs` reads live Kubernetes
			// pod logs (current pods only, lost on restart/reschedule), which is easy to mistake for
			// a durable history, so it points at the logs add-on for retained, queryable logs. How it
			// points there depends on the target: installing the add-on is an operator's job and is
			// refused on the managed product, where the platform already runs one (targethints.go).
			// Stderr keeps it off a piped or redirected log stream; skipped for --json
			// (metadata-free result).
			if !o.json {
				stderr := cmd.ErrOrStderr()
				fmt.Fprintln(stderr, logsSourceNote(o.onManagedTarget()))
				fmt.Fprintln(stderr, strings.Repeat("─", 60))
			}
			lines, err := c.Logs(ctx, args[0], env, tail)
			if err != nil {
				return err
			}
			if o.json {
				return emit(out, true, lines, "")
			}
			if len(lines) == 0 {
				fmt.Fprintln(out, "(no logs)")
			} else {
				for _, l := range lines {
					fmt.Fprintf(out, "%s  %s\n", l.Pod, l.Message)
				}
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().IntVar(&tail, "tail", 0, "maximum number of recent log lines (0 = adapter default)")
	return cmd
}

func newRollbackCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm, skipHooks bool
	wait := true
	cmd := &cobra.Command{
		Use:   "rollback <app>",
		Short: "Roll an app back to its previous release",
		Long: "Roll an app back to its previous release.\n\n" +
			"A failed pre-rollback hook aborts the rollback, because letting the older code serve against\n" +
			"a half-reverted schema is what the hook's ordering exists to prevent. When the hook failed\n" +
			"for a reason that has nothing to do with the schema — it could not pull, could not schedule,\n" +
			"or the command is wrong — --skip-hooks rolls back without running it. The hook stays\n" +
			"configured, the skip is stated in the output, and it is recorded in the audit log.\n\n" +
			"Rollback waits for the rollout and reports what it did: it exits non-zero, naming the pod's\n" +
			"own reason, when the restored image does not become ready — Kubernetes keeps the release you\n" +
			"are rolling away from serving in that case, so nothing has changed except the record. Pass\n" +
			"--wait=false to return at submission instead, which reports that the outcome is unknown\n" +
			"rather than that it worked.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Rollback(ctx, args[0], env, client.RollbackOptions{Confirm: confirm, SkipHooks: skipHooks, NoWait: !wait})
			if err != nil {
				return err
			}
			human := rollbackHuman(args[0], res, o.onManagedTarget())
			// The hints follow the result line rather than replacing it, because the rollback happened:
			// a skipped hook is a fact about how it happened, not a verdict on whether it did.
			for _, hint := range res.Hints {
				human += "\n\n" + hint
			}
			if err := o.emitChange(cmd.OutOrStdout(), res, human); err != nil {
				return err
			}
			// The report is printed; the exit code carries the same verdict for whatever is reading it
			// as a process rather than as prose (ADR-0093 §2).
			return rolloutExitError(args[0], res.Rollout)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a rollback a guardrail holds for confirmation")
	cmd.Flags().BoolVar(&skipHooks, "skip-hooks", false,
		"roll back without running the app's pre-rollback hook, for a hook that is broken or cannot run; the hook stays configured and the skip is recorded")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for the rollout and report its outcome; --wait=false returns at submission, with the outcome unknown")
	return cmd
}

func newScaleCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "scale <app> <replicas>",
		Short: "Set an app's replica count",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("replicas must be a number, got %q", args[1])
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Scale(ctx, args[0], env, int32(n), confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("scaled %s from %d to %d replica(s)", args[0], res.PreviousReplicas, res.Replicas)
			return o.emitChange(cmd.OutOrStdout(), res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

func newRunCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm bool
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "run <app> -- command args...",
		Short: "Run a one-off command in an app's own image and environment",
		Long: "Run a one-off command inside an app's own current image, in its namespace, with its\n" +
			"config and secrets injected exactly as the running app sees them. Use it for the tasks\n" +
			"that belong in the app's runtime: database migrations, seed and fixture loads, data\n" +
			"backfills, a maintenance script.\n\n" +
			"Pass the command after a -- separator, like kubectl run:\n" +
			"  burrow app run web -- npm run migrate\n\n" +
			"The run is synchronous: Burrow launches the command, waits for it to finish, and reports\n" +
			"the exit code and the command's combined stdout+stderr output (Kubernetes interleaves the\n" +
			"two into one stream). A non-zero exit code is a normal outcome, not a CLI failure.\n\n" +
			"The finished Job is garbage-collected after --ttl (default 1h; 0 deletes it as soon as the\n" +
			"output is captured), which only bounds the window to inspect a failure by hand.\n\n" +
			"Running is gated by the app.run guardrail (confirm by default), which gates whether the\n" +
			"command runs, not what it does: this is a command runner, not a SQL firewall, so a\n" +
			"command can still make destructive changes. For risky data changes, back up first.",
		// Exactly one positional (the app name) before any --; everything after -- is the command.
		Args: func(cmd *cobra.Command, args []string) error {
			n := len(args)
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				n = d
			}
			if n != 1 {
				return fmt.Errorf("expected exactly one app name before --, got %d", n)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			var command []string
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				command = args[d:]
			}
			if len(command) == 0 {
				return errors.New("a command is required after --, e.g. `burrow app run web -- npm run migrate`")
			}
			req := client.RunRequest{Command: command, Confirm: confirm}
			// An omitted --ttl leaves TTLSeconds nil so the server applies its default (1h); a supplied
			// duration (including 0, delete immediately) is sent as seconds. A negative is rejected here.
			if cmd.Flags().Changed("ttl") {
				if ttl < 0 {
					return fmt.Errorf("--ttl must not be negative, got %s", ttl)
				}
				secs := int32(ttl.Seconds())
				req.TTLSeconds = &secs
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			req.Env = env
			res, err := c.Run(ctx, app, req)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, formatRunResult(app, res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long to keep the finished Job before it is garbage-collected (e.g. 30m; 0 = delete immediately; omit to keep the default of 1h)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a run a guardrail holds for confirmation")
	return cmd
}

// formatRunResult renders a one-off command's outcome for a human: the exit code, then the captured
// output under a single "output" heading. The output is the COMBINED stdout+stderr stream (Kubernetes
// interleaves the two), so it is not split into separate sections that would imply a distinction that
// does not exist (ADR-0048, ADR-0009). No em-dashes: it is user-facing CLI output.
func formatRunResult(app string, r client.RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ran command in %s: exit code %d", app, r.ExitCode)
	if r.TimedOut {
		b.WriteString(" (timed out before the command finished)")
	}
	// Stdout carries the combined stream; Stderr is reserved and currently always empty, but append
	// it defensively so a future separation is never dropped from the human view.
	out := r.Stdout + r.Stderr
	if out != "" {
		b.WriteString("\noutput (combined stdout+stderr):\n")
		b.WriteString(out)
	}
	return b.String()
}

func newAutoscaleCmd() *cobra.Command {
	o := &commonOpts{}
	var (
		min, max, cpu, memory int32
		confirm               bool
	)
	cmd := &cobra.Command{
		Use:   "autoscale <app> [off]",
		Short: "Autoscale an app, or turn autoscaling off",
		Long: "autoscale sets up a HorizontalPodAutoscaler on the app's Deployment so it scales\n" +
			"between --min and --max replicas to hold a target CPU (and optional memory)\n" +
			"utilization. The max is bounded by the replica ceiling an operator sets. Autoscaling needs\n" +
			"metrics-server; without it the autoscaler is set but will not scale until it is\n" +
			"installed.\n\n" +
			"Run \"burrow app autoscale <app> off\" to remove autoscaling.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
			off := len(args) == 2
			if off && args[1] != "off" {
				return fmt.Errorf("second argument must be \"off\" to turn autoscaling off, got %q", args[1])
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if off {
				if err := c.DisableAutoscale(ctx, app, env, confirm); err != nil {
					return err
				}
				human := fmt.Sprintf("turned autoscaling off for %s", app)
				return o.emitChange(cmd.OutOrStdout(), map[string]string{"app": app}, human)
			}
			res, err := c.Autoscale(ctx, app, client.AutoscaleRequest{Env: env, Min: min, Max: max, CPU: cpu, Memory: memory, Confirm: confirm})
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, formatAutoscale(cmd.OutOrStdout(), res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().Int32Var(&min, "min", 1, "minimum replicas")
	cmd.Flags().Int32Var(&max, "max", 10, "maximum replicas (bounded by the replica ceiling an operator sets)")
	cmd.Flags().Int32Var(&cpu, "cpu", 80, "target average CPU utilization percent")
	cmd.Flags().Int32Var(&memory, "memory", 0, "target average memory utilization percent (0 leaves it unset)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

// formatAutoscale renders the applied autoscaling shape for the human-readable result. When
// metrics-server was not detected it appends the plain-language note the result carries (no
// em-dashes: it is printed verbatim).
func formatAutoscale(w io.Writer, res client.AutoscaleResult) string {
	target := fmt.Sprintf("%d%% CPU", res.CPUPercent)
	if res.MemoryPercent > 0 {
		target += fmt.Sprintf(" and %d%% memory", res.MemoryPercent)
	}
	env := res.Env
	if env == "" {
		env = "default"
	}
	s := fmt.Sprintf("set %s to autoscale between %d and %d replicas at %s in the %s environment",
		res.App, res.MinReplicas, res.MaxReplicas, target, env)
	if res.Warning != "" {
		s += "\n" + note(w) + res.Warning
	}
	return s
}

func newAppDeleteCmd() *cobra.Command {
	o := &commonOpts{}
	var confirm bool
	cmd := &cobra.Command{
		Use:   "delete <app>",
		Short: "Delete an app entirely (its workload, routing, and release history)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := c.DeleteApp(ctx, args[0], env, confirm); err != nil {
				return err
			}
			human := fmt.Sprintf("deleted app %s (workload, routing, and release history)", args[0])
			return o.emitChange(cmd.OutOrStdout(), map[string]string{"app": args[0]}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

// lockedColumn renders lock state for a listing: the word for a locked thing, a dash for the rest.
// A dash rather than "no" keeps the column quiet on the install where nothing is locked, and leaves
// the one row that IS locked the only thing with a word in it.
func lockedColumn(locked bool) string {
	if locked {
		return "locked"
	}
	return "-"
}

func formatStatus(w io.Writer, res client.StatusResult) string {
	s := "app: " + res.App + "\n"
	if res.HasRelease {
		s += fmt.Sprintf("release: %s (image %s, %s)\n", res.Release.ID, res.Release.Image, res.Release.Status)
	} else {
		s += "release: none recorded\n"
	}
	if res.Running {
		avail := "not available"
		if res.Workload.Available {
			avail = "available"
		}
		s += fmt.Sprintf("workload: %d/%d replicas ready, %s", res.Workload.ReadyReplicas, res.Workload.DesiredReplicas, avail)
		if res.Workload.Issue != "" {
			s += "\nissue: " + res.Workload.Issue
		}
	} else {
		s += "workload: not running"
	}
	// The lock, when there is one (cloud ADR-0060 §5). It is printed only when the app is locked:
	// unlocked is what everything is, and a line saying so on every status would be the noise that
	// makes the one that matters easy to skim past.
	if res.Locked {
		s += "\nlocked: yes — deleting this app refuses until `burrow unlock " + res.App + "` is run. Deploys, rollbacks and scaling are unaffected."
	}
	return s + formatStatusFailures(w, res, time.Now())
}

// formatStatusFailures appends the app's recent failure history to its status (ADR-0074 §8). The
// workload block above is the live present tense; this is the half nothing can reconstruct
// afterwards — whether the app crash-looped at 02:00 and recovered, and when it started.
//
// It prints the coverage caveat whenever coverage is incomplete, INCLUDING when the history is
// empty. An app with no rows because burrowd was down all night must not read as an app that had a
// quiet night, and that is the same rule the cluster-wide listing follows.
func formatStatusFailures(w io.Writer, res client.StatusResult, now time.Time) string {
	var b strings.Builder
	if len(res.Failures) > 0 {
		width := 0
		for _, f := range res.Failures {
			if len(f.Reason) > width {
				width = len(f.Reason)
			}
		}
		b.WriteString(fmt.Sprintf("\nrecent failures (%s):\n", window(res.Coverage.Since, res.Coverage.Until)))
		for _, f := range res.Failures {
			b.WriteString(fmt.Sprintf("  %-*s  %-8s  %s\n", width, f.Reason, failureState(f), failureTiming(f, now)))
			if f.Detail != "" {
				b.WriteString("      " + f.Detail + "\n")
			}
		}
	}
	if res.Coverage.Complete() {
		return strings.TrimRight(b.String(), "\n")
	}
	if len(res.Failures) == 0 {
		b.WriteString(fmt.Sprintf("\nrecent failures (%s): none recorded\n", window(res.Coverage.Since, res.Coverage.Until)))
	}
	b.WriteString(strings.TrimRight(formatCoverage(w, res.Coverage, now), "\n") + "\n")
	b.WriteString("  Run `burrow failures` for what else is broken.\n")
	return strings.TrimRight(b.String(), "\n")
}
