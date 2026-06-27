// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"strconv"

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
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			apps, err := c.Apps(ctx)
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
	return cmd
}

func newDeployCmd() *cobra.Command {
	o := &commonOpts{}
	var image, build string
	var replicas int
	var metricsPort int
	var confirm bool
	var env kvFlag
	cmd := &cobra.Command{
		Use:   "deploy <app> [-- command args...]",
		Short: "Deploy an app by image reference (optionally build & push first)",
		Long: "Deploy an app by image reference (optionally build & push first).\n\n" +
			"To run something other than the image's default entrypoint, pass the command after a\n" +
			"-- separator, like kubectl run:\n" +
			"  burrow app deploy worker --image myrepo/app:1.2.3 -- ./worker --queue emails",
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
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if build != "" {
				if err := buildAndPush(ctx, build, image, execRunner(cmd.ErrOrStderr(), cmd.ErrOrStderr())); err != nil {
					return err
				}
			}
			res, err := c.Deploy(ctx, app, client.DeployRequest{
				Image:       image,
				Env:         env.m,
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
	cmd.Flags().StringVar(&image, "image", "", "container image reference to deploy (required)")
	cmd.Flags().IntVar(&replicas, "replicas", 1, "number of replicas")
	cmd.Flags().IntVar(&metricsPort, "metrics-port", 0, "annotate the pod so the metrics add-on scrapes /metrics on this port")
	cmd.Flags().StringVar(&build, "build", "", "build and push the image from this directory before deploying")
	cmd.Flags().Var(&env, "env", "environment variable KEY=VALUE (repeatable)")
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
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.Status(ctx, args[0])
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, res, formatStatus(res))
		},
	}
	bindCommon(cmd.Flags(), o)
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
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			lines, err := c.Logs(ctx, args[0], tail)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, lines, "")
			}
			if len(lines) == 0 {
				fmt.Fprintln(out, "(no logs)")
				return nil
			}
			for _, l := range lines {
				fmt.Fprintf(out, "%s  %s\n", l.Pod, l.Message)
			}
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().IntVar(&tail, "tail", 0, "maximum number of recent log lines (0 = adapter default)")
	return cmd
}

func newRollbackCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "rollback <app>",
		Short: "Roll an app back to its previous release",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.Rollback(ctx, args[0])
			if err != nil {
				return err
			}
			human := fmt.Sprintf("rolled %s back to release %s (image %s) as release %s; superseded release %s",
				args[0], res.RolledBackToReleaseID, res.Release.Image, res.Release.ID, res.SupersededReleaseID)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
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
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.Scale(ctx, args[0], int32(n), confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("scaled %s from %d to %d replica(s)", args[0], res.PreviousReplicas, res.Replicas)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
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
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.DeleteApp(ctx, args[0], confirm); err != nil {
				return err
			}
			human := fmt.Sprintf("deleted app %s (workload, routing, and release history)", args[0])
			return emit(cmd.OutOrStdout(), o.json, map[string]string{"app": args[0]}, human)
		},
	}
	bindCommon(cmd.Flags(), o)
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
	} else {
		s += "workload: not running"
	}
	return s
}
