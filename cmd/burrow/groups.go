// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/localconfig"
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

// firstRunBanner is shown before the help when no local config exists yet, routing a brand-new
// user straight to install rather than the full command wall (ADR-0037). No em-dashes: it is
// user-facing CLI output.
const firstRunBanner = "Burrow is not set up yet. Point it at a cluster to get started:\n\n" +
	"  burrow install <context>\n\n" +
	"Run `burrow install` with no argument to list your kubeconfig contexts, or\n" +
	"`burrow env scan` to find an existing Burrow already in your clusters.\n\n"

// installFirstRunBanner wraps the root help so a first-time user (no ~/.burrow/config) sees a
// short banner pointing at install before the grouped help. It leads only the root command's
// help; subcommand help and the help shown once a config exists are unchanged (ADR-0037).
func installFirstRunBanner(root *cobra.Command) {
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if !cmd.HasParent() {
			if exists, err := localconfig.Exists(); err == nil && !exists {
				fmt.Fprint(cmd.OutOrStdout(), firstRunBanner)
			}
		}
		defaultHelp(cmd, args)
	})
}

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
