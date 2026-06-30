// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"github.com/spf13/cobra"
)

// The CLI is grouped by the task a person is doing rather than as a flat verb list (ADR-0024):
// `app` operates a deployed application, `config` sets up the credentials Burrow uses, and
// `cluster` both inspects the cluster's capabilities and provisions its shared infrastructure
// (ingress/TLS). install/upgrade, guard, and version stay at the top level. The grouping is a
// human-discoverability aid; the MCP tool surface stays flat.

// The top-level command groups, ordered along the golden path (ADR-0037): get started, pick an
// environment, operate apps, then govern with guardrails and the audit trail. version and the
// auto-generated completion/help commands are left ungrouped and render under "Additional
// Commands".
const (
	groupGetStarted   = "get-started"
	groupEnvironments = "environments"
	groupOperate      = "operate"
	groupGovern       = "govern"
)

// addGroups registers the command groups on the root in golden-path order, so `burrow --help`
// presents the commands under labeled headings instead of one flat wall (ADR-0037).
func addGroups(root *cobra.Command) {
	root.AddGroup(
		&cobra.Group{ID: groupGetStarted, Title: "Get started:"},
		&cobra.Group{ID: groupEnvironments, Title: "Environments:"},
		&cobra.Group{ID: groupOperate, Title: "Operate:"},
		&cobra.Group{ID: groupGovern, Title: "Govern:"},
	)
}

// grouped tags a command with a group ID and returns it, so it can be added inline in the root's
// AddCommand call.
func grouped(cmd *cobra.Command, id string) *cobra.Command {
	cmd.GroupID = id
	return cmd
}

// rootShortDesc is the root command's one-line description, reused atop the first-run banner so the
// two stay in sync. No em-dashes: it is user-facing CLI output.
const rootShortDesc = "Operate applications on your own Kubernetes cluster through the Burrow control plane."

// firstRunBanner is what bare `burrow` prints when no local config exists yet, routing a brand-new
// user straight to install rather than the full command wall (ADR-0037). It leads with the one-line
// description and ends with a pointer to `-h` for the full command list. It is shown only on the
// bare-invocation path (root RunE); `burrow -h` shows the grouped help without it. No em-dashes: it
// is user-facing CLI output.
const firstRunBanner = rootShortDesc + "\n\n" +
	"Burrow is not set up yet. Point it at a cluster to get started:\n\n" +
	"  burrow install <context>\n\n" +
	"Run `burrow install` with no argument to list your kubeconfig contexts, or\n" +
	"`burrow env scan` to find an existing Burrow already in your clusters.\n\n" +
	"Run `burrow -h` for the full list of commands.\n"

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
