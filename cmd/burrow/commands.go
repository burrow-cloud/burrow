// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
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
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
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
				fmt.Fprintln(out, "No apps deployed. Deploy one with `burrow app deploy <app> --image <ref>`.")
				return nil
			}
			fmt.Fprintf(out, "%-20s%-34s%-12s%s\n", "NAME", "IMAGE", "REPLICAS", "AVAILABLE")
			for _, a := range apps {
				fmt.Fprintf(out, "%-20s%-34s%-12s%t\n", a.App, a.Image, fmt.Sprintf("%d/%d", a.ReadyReplicas, a.DesiredReplicas), a.Available)
			}
			return nil
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
	cmd := &cobra.Command{
		Use:   "deploy <app> [-- command args...]",
		Short: "Deploy an app by image reference (optionally build & push first)",
		Long: "Deploy an app by image reference (optionally build & push first).\n\n" +
			"To run something other than the image's default entrypoint, pass the command after a\n" +
			"-- separator, like kubectl run:\n" +
			"  burrow app deploy worker --image myrepo/app:1.2.3 -- ./worker --queue emails\n\n" +
			"Environment configuration is set separately and is the single source of truth, sourced\n" +
			"at deploy time, set it with `burrow app config set <app> KEY=VALUE` before deploying a\n" +
			"release that needs it, so the new release boots with it on first start.",
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
			// the deploy API, and the MCP deploy tool already carry Command; this surfaces it on
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
			res, err := c.Deploy(ctx, app, client.DeployRequest{
				Env:         env,
				Image:       image,
				Command:     command,
				MetricsPort: int32(metricsPort),
				Replicas:    int32(replicas),
				Confirm:     confirm,
			})
			if err != nil {
				return err
			}
			human := fmt.Sprintf("deployed %s as release %s (image %s, %d replica(s), %s)",
				app, res.Release.ID, res.Release.Image, res.Release.Replicas, res.Release.Status)
			if res.SupersededReleaseID != "" {
				human += fmt.Sprintf("; superseded release %s", res.SupersededReleaseID)
			}
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&image, "image", "", "container image reference to deploy (required)")
	cmd.Flags().IntVar(&replicas, "replicas", 0, "desired replicas (0 = keep current; new apps default to 1; ignored while autoscaling is enabled)")
	cmd.Flags().IntVar(&metricsPort, "metrics-port", 0, "annotate the pod so the metrics add-on scrapes /metrics on this port")
	cmd.Flags().StringVar(&build, "build", "", "build and push the image from this directory before deploying")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
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
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Status(ctx, args[0], env)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, res, formatStatus(res))
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
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Print the source note and a divider to stderr up front — after the targeting line
			// resolveAndConnect already emitted and before the logs — so the metadata leads and the
			// log lines are the last, uninterrupted thing (a bottom note would be missed, and never
			// appear at all once these logs are streamed/followed). `app logs` reads live Kubernetes
			// pod logs (current pods only, lost on restart/reschedule), which is easy to mistake for
			// a durable history, so it points at the logs add-on for retained, queryable logs. Stderr
			// keeps it off a piped or redirected log stream; skipped for --json (metadata-free result).
			if !o.json {
				stderr := cmd.ErrOrStderr()
				fmt.Fprintln(stderr,
					"Source: live Kubernetes pod logs — current pods only, not retained across restarts. "+
						"For durable, queryable history across replicas, install the logs add-on "+
						"(`burrow addon install logs`), then query with `burrow addon logs`.")
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
	var confirm bool
	cmd := &cobra.Command{
		Use:   "rollback <app>",
		Short: "Roll an app back to its previous release",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Rollback(ctx, args[0], env, confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("rolled %s back to release %s (image %s) as release %s; superseded release %s",
				args[0], res.RolledBackToReleaseID, res.Release.Image, res.Release.ID, res.SupersededReleaseID)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a rollback a guardrail holds for confirmation")
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
			return emit(cmd.OutOrStdout(), o.json, res, human)
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
			return emit(cmd.OutOrStdout(), o.json, res, formatRunResult(app, res))
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
			"utilization. The max is bounded by the replica-ceiling guardrail. Autoscaling needs\n" +
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
				return emit(cmd.OutOrStdout(), o.json, map[string]string{"app": app}, human)
			}
			res, err := c.Autoscale(ctx, app, client.AutoscaleRequest{Env: env, Min: min, Max: max, CPU: cpu, Memory: memory, Confirm: confirm})
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, res, formatAutoscale(cmd.OutOrStdout(), res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().Int32Var(&min, "min", 1, "minimum replicas")
	cmd.Flags().Int32Var(&max, "max", 10, "maximum replicas (bounded by the replica-ceiling guardrail)")
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
			return emit(cmd.OutOrStdout(), o.json, map[string]string{"app": args[0]}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

func formatStatus(res client.StatusResult) string {
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
	return s
}
