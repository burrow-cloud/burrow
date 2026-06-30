// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import "github.com/spf13/cobra"

// The CLI is grouped by the task a person is doing rather than as a flat verb list (ADR-0024):
// `app` operates a deployed application, `config` sets up the credentials Burrow uses, and
// `system` manages cluster-wide infrastructure. install/upgrade, guard, and version stay at the
// top level. The grouping is a human-discoverability aid; the MCP tool surface stays flat.

// newAppCmd groups the operations on a deployed application.
func newAppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Deploy and operate applications",
		Long: "app groups everything you do to a deployed application: deploy and roll back, read\n" +
			"status and logs, scale, and make it reachable at a hostname (publish + domain).",
	}
	cmd.AddCommand(
		newAppListCmd(),
		newDeployCmd(),
		newAppDeleteCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newRollbackCmd(),
		newScaleCmd(),
		newAppConfigCmd(),
		newSecretCmd(),
		newReachabilityCmd(),
		newPublishCmd(),
		newUnpublishCmd(),
		newDomainCmd(),
	)
	return cmd
}

// newConfigCmd groups the external credentials a user configures for Burrow to use.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configure the credentials Burrow uses (providers, registries)",
		Long: "config sets up the external credentials Burrow uses on your behalf: DNS/cloud\n" +
			"providers and container-registry pull secrets. Burrow stores and reads them; the\n" +
			"agent never holds them.",
	}
	cmd.AddCommand(newProviderCmd(), newRegistryCmd())
	return cmd
}

// newSystemCmd groups the cluster-wide infrastructure Burrow installs and manages.
func newSystemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Manage cluster-wide infrastructure Burrow uses (ingress, TLS)",
		Long: "system manages the shared, cluster-wide infrastructure Burrow builds on — the ingress\n" +
			"controller and cert-manager — as opposed to per-app operations (`app`) or Burrow's own\n" +
			"control plane (`install`/`upgrade`).",
	}
	cmd.AddCommand(newIngressCmd())
	return cmd
}
