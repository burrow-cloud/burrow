// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// The deploy-time dependency check on the agent surface, READ ONLY (ADR-0076 §4, ADR-0065 §1).
//
// The read is here because a deploy result can carry a failed dependency and an agent needs to be
// able to ask what the check actually covers before it reasons about one. It moves no secret: a
// dependency carries an environment variable's key NAME and an in-cluster address Burrow composed.
//
// TURNING THE CHECK OFF IS NOT HERE, and the omission is the point. It is standing authority to stop
// verifying what Burrow handed an app — the same class of decision as setting a lifecycle hook or an
// auto-deploy level, both of which are operator-only for the same reason. An agent that could silence
// a check it was failing would be an agent that could make its own work look correct.

const checksLong = `After a deploy, Burrow checks the things it provisioned for this app, from inside the app's own
container, with the app's own environment and credentials.

What is checked is DERIVED from what Burrow provisioned, never configured:

  an attached database   connect with the app's own DATABASE_URL and run SELECT 1
  a published port       request the app's port and report the status code

A FAILED CHECK NEVER FAILS THE DEPLOY. Burrow does not roll back by itself. A "failed" entry sits on
a deploy that succeeded and is live, and it means the release is running but something it was given
does not answer: usually a credential the container cannot see, a host it cannot resolve, or a port
nothing is listening on.

This is deliberately NOT a readiness probe, and a health endpoint must not do it either. A readiness
probe that tested the shared database would pull every replica of every app out of service the moment
that database blipped. Run once at deploy time, the same check catches the misconfiguration with none
of the blast radius.`

// newChecksCmd reads what Burrow checks after a deploy of an app. There is no mutating counterpart
// on this surface.
func newChecksCmd() *cobra.Command {
	o := &connOpts{}
	cmd := &cobra.Command{
		Use:   "checks <app>",
		Short: "Show the deploy-time dependency checks Burrow derives for an app",
		Long:  "Show what Burrow checks after a deploy of this app, and whether it checks at all.\n\n" + checksLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.withClient(cmd, func(ctx context.Context, c *client.Client, env string) (any, error) {
				return c.Checks(ctx, args[0], env)
			})
		},
	}
	bindConn(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
