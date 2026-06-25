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

func newDeployCmd() *cobra.Command {
	o := &commonOpts{}
	var image, build string
	var replicas int
	var env kvFlag
	cmd := &cobra.Command{
		Use:   "deploy <app>",
		Short: "Deploy an app by image reference (optionally build & push first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app := args[0]
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
				Image:    image,
				Env:      env.m,
				Replicas: int32(replicas),
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
	cmd.Flags().StringVar(&build, "build", "", "build and push the image from this directory before deploying")
	cmd.Flags().Var(&env, "env", "environment variable KEY=VALUE (repeatable)")
	return cmd
}

func newStatusCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "status <app>",
		Short: "Show an app's release and live workload status",
		Args:  cobra.ExactArgs(1),
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
		Args:  cobra.ExactArgs(1),
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
		Args:  cobra.ExactArgs(1),
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
	cmd := &cobra.Command{
		Use:   "scale <app> <replicas>",
		Short: "Set an app's replica count",
		Args:  cobra.ExactArgs(2),
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
			res, err := c.Scale(ctx, args[0], int32(n))
			if err != nil {
				return err
			}
			human := fmt.Sprintf("scaled %s from %d to %d replica(s)", args[0], res.PreviousReplicas, res.Replicas)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
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
