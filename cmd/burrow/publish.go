// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"

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
	cmd := &cobra.Command{
		Use:   "reachability <app>",
		Short: "Report whether an app is reachable at its hostname (controller, address, DNS)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
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
