// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/burrow-cloud/burrow/client"
)

func cmdDeploy(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := addCommon(fs)
	image := fs.String("image", "", "container image reference to deploy (required)")
	replicas := fs.Int("replicas", 1, "number of replicas")
	build := fs.String("build", "", "build and push the image from this directory before deploying")
	var env kvFlag
	fs.Var(&env, "env", "environment variable KEY=VALUE (repeatable)")
	pos, flagArgs := splitArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	app := arg(pos, 0)
	if app == "" || *image == "" {
		return fmt.Errorf("usage: burrow deploy <app> --image <ref> [--replicas n] [--env K=V] [--build dir]")
	}
	c, err := o.client()
	if err != nil {
		return err
	}

	if *build != "" {
		if err := buildAndPush(ctx, *build, *image, execRunner(stderr, stderr)); err != nil {
			return err
		}
	}

	res, err := c.Deploy(ctx, app, client.DeployRequest{
		Image:    *image,
		Env:      env.m,
		Replicas: int32(*replicas),
	})
	if err != nil {
		return err
	}
	human := fmt.Sprintf("deployed %s as release %s (image %s, %d replica(s), %s)",
		app, res.Release.ID, res.Release.Image, res.Release.Replicas, res.Release.Status)
	if res.SupersededReleaseID != "" {
		human += fmt.Sprintf("; superseded release %s", res.SupersededReleaseID)
	}
	return emit(stdout, o.json, res, human)
}

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := addCommon(fs)
	pos, flagArgs := splitArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	app := arg(pos, 0)
	if app == "" {
		return fmt.Errorf("usage: burrow status <app>")
	}
	c, err := o.client()
	if err != nil {
		return err
	}
	res, err := c.Status(ctx, app)
	if err != nil {
		return err
	}
	return emit(stdout, o.json, res, formatStatus(res))
}

func cmdLogs(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := addCommon(fs)
	tail := fs.Int("tail", 0, "maximum number of recent log lines (0 = adapter default)")
	pos, flagArgs := splitArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	app := arg(pos, 0)
	if app == "" {
		return fmt.Errorf("usage: burrow logs <app> [--tail n]")
	}
	c, err := o.client()
	if err != nil {
		return err
	}
	lines, err := c.Logs(ctx, app, *tail)
	if err != nil {
		return err
	}
	if o.json {
		return emit(stdout, true, lines, "")
	}
	if len(lines) == 0 {
		fmt.Fprintln(stdout, "(no logs)")
		return nil
	}
	for _, l := range lines {
		fmt.Fprintf(stdout, "%s  %s\n", l.Pod, l.Message)
	}
	return nil
}

func cmdRollback(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := addCommon(fs)
	pos, flagArgs := splitArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	app := arg(pos, 0)
	if app == "" {
		return fmt.Errorf("usage: burrow rollback <app>")
	}
	c, err := o.client()
	if err != nil {
		return err
	}
	res, err := c.Rollback(ctx, app)
	if err != nil {
		return err
	}
	human := fmt.Sprintf("rolled %s back to release %s (image %s) as release %s; superseded release %s",
		app, res.RolledBackToReleaseID, res.Release.Image, res.Release.ID, res.SupersededReleaseID)
	return emit(stdout, o.json, res, human)
}

func cmdScale(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("scale", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := addCommon(fs)
	pos, flagArgs := splitArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	app, nStr := arg(pos, 0), arg(pos, 1)
	if app == "" || nStr == "" {
		return fmt.Errorf("usage: burrow scale <app> <replicas>")
	}
	n, err := strconv.Atoi(nStr)
	if err != nil {
		return fmt.Errorf("replicas must be a number, got %q", nStr)
	}
	c, err := o.client()
	if err != nil {
		return err
	}
	res, err := c.Scale(ctx, app, int32(n))
	if err != nil {
		return err
	}
	human := fmt.Sprintf("scaled %s from %d to %d replica(s)", app, res.PreviousReplicas, res.Replicas)
	return emit(stdout, o.json, res, human)
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
