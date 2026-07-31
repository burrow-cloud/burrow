// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// The health verbs on the agent surface (ADR-0076 §5). The agent is here because it is the only
// party that CAN close this gap: Burrow cannot write a health endpoint into the user's application,
// and the agent has the source. ADR-0065 §1 puts the verb on this surface — its effect is one app's
// readiness probe, it is reversible with `health unset`, and it destroys nothing.
//
// The guidance below is the point of the command as much as the verb is. An agent asked to add
// /healthz reaches for the internet's most common example, which checks the database — the single
// worst thing to put in a readiness probe on this platform, because the database is shared and one
// blip would then remove every replica of every app from service at the same moment (ADR-0076 §2).
// Saying so here, in the text the agent reads before it acts, is the only place that warning lands
// in time.

// healthLong is the orientation shared by the health verbs.
const healthLong = `Kubernetes sends traffic to a pod the moment its container starts, so an application that boots
and cannot serve is still reported as a successful deploy. A readiness probe fixes that: a pod
failing it is removed from the load balancer until it passes again. Nothing is restarted.

What Burrow does with no configuration:

  published app    a TCP check on the port the app is published on
  unpublished app  no probe, because Burrow does not know a port and will not guess one

Burrow never scans for a listening port, never assumes 8080, never guesses a path, and NEVER sets
a liveness probe (a liveness probe restarts the container, so a wrong one manufactures a crash
loop out of a working process).

A real endpoint is better, and only the application can serve one. If this app has no health
endpoint, adding one is usually a few lines: serve a path that returns 200 when the app itself is
ready to handle a request, then run "burrow-agent health set <app> --path /healthz".

CRITICAL: that endpoint must check only THIS app's own readiness to serve. It must NOT check the
database, the cache, or any other dependency, however common that example is. The database is
shared between apps, so a dependency check in a readiness endpoint turns one brief blip into every
replica of every app being pulled out of service simultaneously.`

// newHealthCmd reads an app's health configuration and carries the set/unset writes as
// subcommands, the shape `config` uses.
func newHealthCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "health <app>",
		Short: "Show the readiness probe Burrow sets on an app (set/unset are subcommands)",
		Long:  "Show what health endpoint an app declares and what readiness probe Burrow sets from it.\n\n" + healthLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Health(ctx, args[0], env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.AddCommand(newHealthSetCmd(), newHealthUnsetCmd())
	return cmd
}

// newHealthSetCmd declares the endpoint the app serves.
func newHealthSetCmd() *cobra.Command {
	o := &connOpts{}
	var path string
	var port int32
	cmd := &cobra.Command{
		Use:   "set <app> --path /healthz [--port N]",
		Short: "Declare the health endpoint an app serves, so Burrow probes it",
		Long: "Declare the HTTP path this app serves its readiness answer on. Burrow then probes that\n" +
			"path instead of merely connecting to the port.\n\n" +
			"Omit --port to use the port the app is published on. Give --port when the app serves its\n" +
			"health path on another port, or when it is not published at all.\n\n" +
			"This re-applies the running workload so the app rolls onto the new probe. The rollout\n" +
			"starts a new pod before removing the old one, so a path that never answers stalls the\n" +
			"rollout and leaves the previous pods serving rather than taking the app down.\n\n" + healthLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if path == "" {
				return fmt.Errorf("--path is required: it is the path the application serves, and Burrow will not guess one")
			}
			return o.mutate(cmd, "health_set", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.SetHealth(ctx, args[0], env, path, port)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&path, "path", "", "the HTTP path the app serves its readiness answer on (e.g. /healthz)")
	cmd.Flags().Int32Var(&port, "port", 0, "the container port that path is served on (default: the port the app is published on)")
	return cmd
}

// newHealthUnsetCmd returns an app to the conservative default.
func newHealthUnsetCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "unset <app>",
		Short: "Remove an app's declared health endpoint, returning it to the default",
		Long: "Remove the health endpoint declared for an app, returning it to Burrow's default: a TCP\n" +
			"check on the published port, or no probe at all when the app is not published. This\n" +
			"re-applies the running workload so the app rolls off the HTTP check.\n\n" + healthLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.mutate(cmd, "health_unset", func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.UnsetHealth(ctx, args[0], env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
