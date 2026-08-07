// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

func newPublishCmd() *cobra.Command {
	o := &commonOpts{}
	var host, issuer, provider string
	var port int
	var tls, noDNS, confirm bool
	cmd := &cobra.Command{
		Use:   "publish <app>",
		Short: "Make an app reachable at a hostname over HTTPS: routing, DNS, and the certificate",
		Long: "publish is the whole path to a reachable app in one command: it routes the hostname to\n" +
			"the app (a Service + Ingress), writes the DNS record when a provider is configured, checks\n" +
			"the host really reaches this cluster over plain HTTP, and only then requests the HTTPS\n" +
			"certificate and waits for it. It reports whether the app ended up live, and when it did not,\n" +
			"the one link it is waiting on.\n\n" +
			"TLS is on by default. Pass --tls=false to publish over plain HTTP; that is refused for a host\n" +
			"on an HSTS-preloaded domain such as .dev, where a browser will not open http:// at all.\n\n" +
			"The certificate is requested only AFTER the host is confirmed to resolve to this cluster and\n" +
			"answer on port 80, so a misconfigured hostname does not spend the certificate authority's\n" +
			"rate limit on an order that cannot complete.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if host == "" {
				return errors.New("--host is required")
			}
			if port <= 0 {
				return errors.New("--port is required")
			}
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := c.Publish(ctx, args[0], client.PublishRequest{
				Env: env, Host: host, Port: int32(port), NoTLS: !tls, Issuer: issuer,
				SkipDNS: noDNS, Provider: provider, Confirm: confirm,
			})
			if err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), res, res.Summary)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().StringVar(&host, "host", "", "external hostname to route to the app (required)")
	cmd.Flags().IntVar(&port, "port", 0, "the app's container port to forward to (required)")
	cmd.Flags().BoolVar(&tls, "tls", true, "request an HTTPS certificate for the host via cert-manager (--tls=false publishes plain HTTP)")
	cmd.Flags().StringVar(&issuer, "tls-issuer", "letsencrypt", "cert-manager ClusterIssuer to request the certificate from")
	cmd.Flags().BoolVar(&noDNS, "no-dns", false, "leave DNS alone; publish only routes the host and waits for what already points at the cluster")
	cmd.Flags().StringVar(&provider, "provider", "", "configured DNS provider to write the record at (default: the only one configured)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

func newReachabilityCmd() *cobra.Command {
	o := &commonOpts{}
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "reachability <app>",
		Short: "Report whether an app is reachable at its hostname (controller, address, TLS, DNS)",
		Long: "reachability reports the converged verdict for an app's reachability chain: whether it\n" +
			"is live and at what URL, or the first link it is blocked on. With --wait it polls until\n" +
			"the app is live or the timeout elapses, so a deploy/expose/DNS sequence can be confirmed\n" +
			"in one call.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnectRead(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if wait {
				res, err := c.WaitReachable(ctx, args[0], env, timeout, nil)
				if err != nil {
					return err
				}
				if res.Reachable {
					return emit(cmd.OutOrStdout(), o.json, res, fmt.Sprintf("%s is live at %s", res.App, res.URL))
				}
				human := fmt.Sprintf("not reachable after %s: waiting on %s", timeout, res.BlockedOn)
				return emit(cmd.OutOrStdout(), o.json, res, human)
			}
			res, err := c.Reachability(ctx, args[0], env)
			if err != nil {
				return err
			}
			// The plain summary is the human default; --json carries the full chain.
			return emit(cmd.OutOrStdout(), o.json, res, res.Summary)
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until the app is live (reachable) or the timeout elapses")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "how long to wait for the app to become live with --wait")
	return cmd
}

func newUnpublishCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "unpublish <app>",
		Short: "Stop serving an app at its hostname (removes its Service + Ingress)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, env, err := o.resolveAndConnect(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := c.Unexpose(ctx, args[0], env); err != nil {
				return err
			}
			return o.emitChange(cmd.OutOrStdout(), map[string]string{"app": args[0]}, fmt.Sprintf("unpublished %s", args[0]))
		},
	}
	bindCommon(cmd.Flags(), o)
	bindEnv(cmd.Flags(), o)
	return cmd
}
