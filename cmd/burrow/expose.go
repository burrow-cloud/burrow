// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newExposeCmd() *cobra.Command {
	o := &commonOpts{}
	var host string
	var port int
	var confirm bool
	cmd := &cobra.Command{
		Use:   "expose <app>",
		Short: "Make an app reachable at a hostname (creates a Service + Ingress)",
		Args:  cobra.ExactArgs(1),
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
			res, err := c.Expose(ctx, args[0], host, int32(port), confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("exposed %s at %s (%s)\nReachable once an ingress controller is running and DNS points %s at the cluster.",
				res.App, res.Host, res.URL, res.Host)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&host, "host", "", "external hostname to route to the app (required)")
	cmd.Flags().IntVar(&port, "port", 0, "the app's container port to forward to (required)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

func newUnexposeCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "unexpose <app>",
		Short: "Remove an app's exposure (Service + Ingress)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			if err := c.Unexpose(ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unexposed %s\n", args[0])
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}
