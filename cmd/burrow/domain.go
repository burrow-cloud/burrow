// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// newDomainCmd manages the DNS records that point a hostname at the cluster, through a
// configured provider (ADR-0018). These are guarded operations through burrowd: the agent
// initiates them, but burrowd holds the provider token and is the only thing that calls the
// vendor, and a public DNS change is gated by the dns.write / dns.delete guardrails.
func newDomainCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "domain",
		Short: "Point a hostname at the cluster via a DNS provider (add/remove)",
		Long: "domain manages the DNS record that points a hostname at your cluster's external\n" +
			"address through a configured provider (e.g. DigitalOcean or Cloudflare). Get the\n" +
			"address from `burrow app reachability <app>` once the app is exposed.",
	}
	parent.AddCommand(newDomainAddCmd(), newDomainRemoveCmd())
	return parent
}

func newDomainAddCmd() *cobra.Command {
	o := &commonOpts{}
	var provider, address, app string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "add <host>",
		Short: "Point a hostname at the cluster (creates/updates a DNS record)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if address == "" && app == "" {
				return errors.New("give --address (the cluster's external address) or --app (an exposed app to read it from)")
			}
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.AddDomain(ctx, args[0], provider, address, app, confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("pointed %s at %s (%s record) via provider %q", res.Host, res.Address, res.Type, res.Provider)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&provider, "provider", "", "configured DNS provider to write the record at (default: the only one configured)")
	cmd.Flags().StringVar(&address, "address", "", "the cluster's external IP or hostname to point at (or use --app)")
	cmd.Flags().StringVar(&app, "app", "", "an exposed app whose external address to point at (instead of --address)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

func newDomainRemoveCmd() *cobra.Command {
	o := &commonOpts{}
	var provider string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "remove <host>",
		Short: "Remove the DNS record for a hostname",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			res, err := c.RemoveDomain(ctx, args[0], provider, confirm)
			if err != nil {
				return err
			}
			human := fmt.Sprintf("removed the DNS record for %s via provider %q", res.Host, res.Provider)
			return emit(cmd.OutOrStdout(), o.json, res, human)
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&provider, "provider", "", "configured DNS provider holding the record (default: the only one configured)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}
