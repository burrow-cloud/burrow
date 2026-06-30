// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newPublishCmd() *cobra.Command {
	o := &commonOpts{}
	var host, issuer string
	var port int
	var tls, confirm bool
	cmd := &cobra.Command{
		Use:   "publish <app>",
		Short: "Make an app reachable at a hostname over HTTP(S)",
		Long: "publish routes an external hostname to the app (a Service + Ingress), optionally with\n" +
			"an HTTPS certificate via cert-manager (--tls). Point the hostname's DNS at the cluster\n" +
			"with `burrow app domain add` to finish the reachability chain.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if host == "" {
				return errors.New("--host is required")
			}
			if port <= 0 {
				return errors.New("--port is required")
			}
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.Expose(ctx, args[0], host, int32(port), tls, issuer, confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("published %s at %s (%s)\nReachable once an ingress controller is running and DNS points %s at the cluster.",
				res.App, res.Host, res.URL, res.Host)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&host, "host", "", "external hostname to route to the app (required)")
	cmd.Flags().IntVar(&port, "port", 0, "the app's container port to forward to (required)")
	cmd.Flags().BoolVar(&tls, "tls", false, "request an HTTPS certificate for the host via cert-manager")
	cmd.Flags().StringVar(&issuer, "tls-issuer", "letsencrypt", "cert-manager ClusterIssuer to request the certificate from")
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
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if wait {
				res, err := c.WaitReachable(ctx, args[0], timeout, nil)
				if err != nil {
					return err
				}
				if res.Reachable {
					return emit(cmd.OutOrStdout(), o.json, res, fmt.Sprintf("%s is live at %s", res.App, res.URL))
				}
				human := fmt.Sprintf("not reachable after %s: waiting on %s", timeout, res.BlockedOn)
				return emit(cmd.OutOrStdout(), o.json, res, human)
			}
			res, err := c.Reachability(ctx, args[0])
			if err != nil {
				return err
			}
			// The plain summary is the human default; --json carries the full chain.
			return emit(cmd.OutOrStdout(), o.json, res, res.Summary)
		},
	}
	bindCommon(cmd.Flags(), o)
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
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.Unexpose(ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unpublished %s\n", args[0])
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}
