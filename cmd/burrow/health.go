// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// healthLong is the shared orientation for `burrow app health`. It says what Burrow does by default,
// what it will not do, and — the part a person needs before they write an endpoint — what the
// endpoint must and must not check (ADR-0076 §2, §5). Hard-wrapped, no em-dashes: user-facing output.
const healthLong = "Show or set the health endpoint Burrow probes on an app.\n\n" +
	"Kubernetes sends traffic to a pod as soon as its container starts, so an app that boots and\n" +
	"cannot serve still looks like a successful deploy. A readiness probe fixes that: a pod that\n" +
	"fails it is taken out of the load balancer until it passes, and nothing is restarted.\n\n" +
	"What Burrow does on its own:\n\n" +
	"  published app    a TCP check on the port you published, which proves the app bound its socket\n" +
	"  unpublished app  no probe at all, because Burrow does not know a port and will not guess one\n\n" +
	"Burrow never scans for a listening port, never assumes 8080, and never guesses a path like\n" +
	"/healthz. A probe it invented would fail a working deploy for a reason you could not see.\n\n" +
	"Burrow also never sets a liveness probe. A liveness probe restarts the container, so one that\n" +
	"is even slightly wrong kills a working process over and over and looks exactly like the crash\n" +
	"loop it was meant to catch.\n\n" +
	"A real endpoint is better, and only your app can serve one. Have it return 200 when the app\n" +
	"itself is ready to handle a request, then point Burrow at it with `health set`.\n\n" +
	"Do NOT have that endpoint check your database, cache, or any other dependency. It is the most\n" +
	"common example on the internet and it is the wrong thing here: your database is shared, so one\n" +
	"blip would fail every replica of every app at the same moment and take them all out of service\n" +
	"at once. A readiness endpoint answers \"can THIS process serve a request\", nothing wider."

// newAppHealthCmd groups the health-endpoint operations on one app (ADR-0076 §5).
func newAppHealthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Show or set the health endpoint Burrow probes on an app",
		Long:  healthLong,
	}
	cmd.AddCommand(newAppHealthShowCmd(), newAppHealthSetCmd(), newAppHealthUnsetCmd())
	return cmd
}

// newAppHealthShowCmd reports the declared endpoint and the probe Burrow actually authors.
func newAppHealthShowCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "show <app>",
		Short: "Show the readiness probe Burrow sets on an app",
		Long:  "Show what health endpoint an app declares and what readiness probe Burrow sets from it.\n\n" + healthLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Health(ctx, args[0], env)
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), o.json, res, healthHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newAppHealthSetCmd records the endpoint an app serves and rolls the workload onto it.
func newAppHealthSetCmd() *cobra.Command {
	o := &commonOpts{}
	var path string
	var port int32
	cmd := &cobra.Command{
		Use:   "set <app> --path /healthz [--port N]",
		Short: "Declare the health endpoint an app serves",
		Long: "Declare the HTTP path an app serves its readiness answer on. Burrow then probes that\n" +
			"path instead of just connecting to the port, so a release that boots but cannot serve\n" +
			"is caught instead of reported as a success.\n\n" +
			"Omit --port to use the port the app is published on, so moving the exposure to another\n" +
			"port does not mean declaring the endpoint again. Give --port when the app serves its\n" +
			"health path somewhere other than the port it publishes, or when it is not published.\n\n" +
			"Setting this re-applies the running workload, so the app rolls onto the new probe. The\n" +
			"rollout brings up a new pod before removing the old one, so an endpoint that never\n" +
			"answers stalls the rollout and leaves the previous pods serving, rather than taking the\n" +
			"app down.\n\n" +
			"Undo it with `burrow app health unset <app>`.\n\n" + healthLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if path == "" {
				return fmt.Errorf("--path is required: it is the path your app serves, and Burrow will not guess one")
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.SetHealth(ctx, args[0], env, path, port)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, healthHuman(res))
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "the HTTP path the app serves its readiness answer on (e.g. /healthz)")
	cmd.Flags().Int32Var(&port, "port", 0, "the container port that path is served on (default: the port the app is published on)")
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// newAppHealthUnsetCmd returns an app to the conservative default.
func newAppHealthUnsetCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "unset <app>",
		Short: "Remove an app's declared health endpoint",
		Long: "Remove the health endpoint declared for an app, returning it to Burrow's default: a TCP\n" +
			"check on the published port, or no probe at all if the app is not published.\n\n" +
			"This re-applies the running workload, so the app rolls off the HTTP check.\n\n" + healthLong,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.UnsetHealth(ctx, args[0], env)
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, healthHuman(res))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}

// healthHuman renders a health report for a person: the probe Burrow sets, where it came from, and
// the guidance when there is no declared endpoint. It leads with the PROBE rather than the
// declaration, because the probe is the thing that acts on the app.
func healthHuman(res client.HealthResult) string {
	var b strings.Builder
	switch res.Probe {
	case "http":
		fmt.Fprintf(&b, "%s: readiness probe HTTP GET %s on port %d (environment %q)", res.App, res.ProbePath, res.ProbePort, res.Environment)
	case "tcp":
		fmt.Fprintf(&b, "%s: readiness probe TCP connect on port %d (environment %q)", res.App, res.ProbePort, res.Environment)
	default:
		fmt.Fprintf(&b, "%s: no readiness probe (environment %q)", res.App, res.Environment)
	}
	switch res.Source {
	case "endpoint":
		b.WriteString("\n  from: the health endpoint declared for this app")
	case "exposure":
		b.WriteString("\n  from: the port this app is published on (Burrow's default)")
	}
	b.WriteString("\n  liveness probe: none, and Burrow never sets one by default")
	if res.AppliesOn != "" {
		fmt.Fprintf(&b, "\n  applies on: %s", res.AppliesOn)
	}
	if res.Hint != "" {
		fmt.Fprintf(&b, "\n\n%s", res.Hint)
	}
	return b.String()
}
