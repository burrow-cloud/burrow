// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow is the Burrow CLI: the human-facing way to operate Burrow. It calls the
// same control-plane API the MCP server does (ADR-0002) — deploy by image reference,
// status, logs, rollback, scale — and can build and push an image first (the client-side
// build path, ADR-0008). Like the MCP server it carries no orchestration logic and no
// cluster credentials, only the control-plane API token (ADR-0005). Its command surface is
// built with Cobra (ADR-0019).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		// errArgsReported is already printed (the friendly message + the command's usage), so
		// don't print it again — just exit non-zero.
		if !errors.Is(err, errArgsReported) {
			fmt.Fprintln(os.Stderr, "burrow:", err)
		}
		os.Exit(1)
	}
}

// errArgsReported is returned by exactArgs after it has printed a helpful message naming the
// expected arguments and the command's usage; main exits non-zero without reprinting it.
var errArgsReported = errors.New("invalid arguments (already reported)")

// exactArgs requires exactly n positional arguments. On a mismatch it prints a plain message
// naming the arguments the command expects — drawn from its Use line, e.g. "<app>" — followed
// by the command's usage, so a user who forgets an argument sees what to pass instead of
// Cobra's bare "accepts 1 arg(s), received 0".
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == n {
			return nil
		}
		w := cmd.ErrOrStderr()
		expected := usageArgs(cmd.Use)
		if len(args) < n {
			fmt.Fprintf(w, "%s needs %s.\n\n", cmd.CommandPath(), expected)
		} else {
			fmt.Fprintf(w, "%s takes only %s.\n\n", cmd.CommandPath(), expected)
		}
		fmt.Fprint(w, cmd.UsageString())
		return errArgsReported
	}
}

// usageArgs extracts the argument placeholders from a command's Use line — the <...> / [...]
// tokens after the command name, e.g. "scale <app> <replicas>" -> "<app> <replicas>".
func usageArgs(use string) string {
	fields := strings.Fields(use)
	var parts []string
	for _, f := range fields[1:] { // skip the command name itself
		if strings.HasPrefix(f, "<") || strings.HasPrefix(f, "[") {
			parts = append(parts, f)
		}
	}
	if len(parts) == 0 {
		return "different arguments"
	}
	return strings.Join(parts, " ")
}

// run builds the root command and executes it with args. It is the single entry point the
// tests drive; building a fresh command tree each call keeps flag defaults (read from the
// environment) and output writers isolated per invocation.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.ExecuteContext(ctx)
}

// Command groups organize `burrow --help` by intent along the golden path (ADR-0037), rather
// than as a flat verb wall. version, completion, and help carry no group, so Cobra lists them
// in the default "Additional Commands" section.
const (
	groupGetStarted   = "get-started"
	groupEnvironments = "environments"
	groupOperate      = "operate"
	groupGovern       = "govern"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "burrow",
		Short: "Operate applications on your cluster through the Burrow control plane",
		Long: "burrow operates applications on your Kubernetes cluster through the Burrow\n" +
			"control plane: deploy by image reference, then status, logs, rollback, and scale.\n" +
			"It auto-connects via your kubeconfig; no config beyond kubectl access.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Cobra's built-in completion command stays enabled and visible (ADR-0037): it emits the
	// bash/zsh/fish/PowerShell scripts and lists alongside version and help.

	root.AddGroup(
		&cobra.Group{ID: groupGetStarted, Title: "Get started:"},
		&cobra.Group{ID: groupEnvironments, Title: "Environments:"},
		&cobra.Group{ID: groupOperate, Title: "Operate:"},
		&cobra.Group{ID: groupGovern, Title: "Govern:"},
	)

	// Get started: install and grow the control plane and the cluster it runs on. `system` is
	// folded into `cluster` and `context` into `env` in sibling ADR-0036/0037 slices; until those
	// land they are grouped with their successors so the help stays coherent.
	addGrouped(root, groupGetStarted, newInstallCmd(), newUpgradeCmd(), newClusterCmd(), newConfigCmd(), newSystemCmd())
	// Environments: select which cluster/namespace a command targets.
	addGrouped(root, groupEnvironments, newEnvCmd(), newContextCmd())
	// Operate: act on deployed applications and their backing add-ons.
	addGrouped(root, groupOperate, newAppCmd(), newAddonCmd())
	// Govern: the guardrail policy and the audit trail.
	addGrouped(root, groupGovern, newGuardCmd(), newAuditCmd())
	// version sits with completion and help in the default "Additional Commands" section.
	root.AddCommand(newVersionCmd())

	installFirstRunBanner(root)
	return root
}

// addGrouped adds each command to root under the given help group.
func addGrouped(root *cobra.Command, group string, cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.GroupID = group
		root.AddCommand(c)
	}
}

// firstRunBanner leads the help of a brand-new install (no client-side config yet), routing the
// user to `burrow install` before the grouped command list (ADR-0037).
const firstRunBanner = "Burrow is not set up yet. Start with \"burrow install\" to install the\n" +
	"control plane into a cluster, then re-run \"burrow\" to see all commands.\n\n"

// installFirstRunBanner wraps the root help so that, on first run (when the client-side config
// does not exist), the help leads with a short banner pointing at `burrow install`. Once the
// config exists the normal grouped help shows. Subcommand help is unaffected.
func installFirstRunBanner(root *cobra.Command) {
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if cmd == root {
			if exists, err := localconfig.Exists(); err == nil && !exists {
				fmt.Fprint(cmd.OutOrStdout(), firstRunBanner)
			}
		}
		defaultHelp(cmd, args)
	})
}

// commonOpts holds the configuration the control-plane operations share.
type commonOpts struct {
	controlPlane string
	token        string
	kubeconfig   string
	context      string
	namespace    string
	env          string
	json         bool
}

// bindCommon registers the shared flags on the flag set, defaulting from the environment.
func bindCommon(flags *pflag.FlagSet, o *commonOpts) {
	bindClientFlags(flags, o)
	flags.StringVar(&o.namespace, "namespace", connect.DefaultNamespace, "namespace Burrow is installed in")
}

// bindEnv registers the --env flag selecting which namespace-per-environment to operate in (ADR-0035
// phase 2b). It is added only to the per-app operation commands; an empty value means the default
// environment. The flag is distinct from --context (which selects the cluster) and from `burrow app
// config` (an app's environment variables).
func bindEnv(flags *pflag.FlagSet, o *commonOpts) {
	flags.StringVar(&o.env, "env", "", "environment to operate in (default: the default environment)")
}

// bindClientFlags registers the control-plane connection flags without --namespace, so a command
// that needs --namespace for a different meaning (e.g. `env add`, where it is the environment's
// namespace) can bind the control-plane namespace under its own flag name.
func bindClientFlags(flags *pflag.FlagSet, o *commonOpts) {
	flags.StringVar(&o.controlPlane, "control-plane", os.Getenv("BURROW_CONTROL_PLANE_URL"), "control-plane API base URL (default: auto-connect via kubeconfig)")
	flags.StringVar(&o.token, "token", os.Getenv("BURROW_API_TOKEN"), "control-plane API token (default: read from the install Secret)")
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig for auto-connect (default: ambient)")
	flags.StringVar(&o.context, "context", "", "kubeconfig context to target (default: current context); selects which cluster's burrowd to operate")
	flags.BoolVar(&o.json, "json", false, "print the raw JSON result")
}

// client returns a control-plane client. With --control-plane set it talks to that URL
// directly (e.g. an ingress) using --token. Otherwise it auto-connects through the
// Kubernetes API-server proxy with the ambient kubeconfig, reading the token from the
// install Secret — so a developer with kubectl access configures nothing (ADR-0014).
func (o *commonOpts) client(ctx context.Context) (*client.Client, error) {
	if o.controlPlane != "" {
		if o.token == "" {
			return nil, errors.New("--token (or BURROW_API_TOKEN) is required with --control-plane")
		}
		return client.NewClient(o.controlPlane, o.token), nil
	}
	return connect.Client(ctx, connect.Options{Kubeconfig: o.kubeconfig, Context: o.context, Namespace: o.namespace})
}

// emit prints v as indented JSON when asJSON, otherwise the human-readable line.
func emit(w io.Writer, asJSON bool, v any, human string) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	fmt.Fprintln(w, human)
	return nil
}

// kvFlag collects repeated KEY=VALUE flags into a map. It satisfies pflag.Value.
type kvFlag struct{ m map[string]string }

func (f *kvFlag) String() string { return "" }

func (f *kvFlag) Type() string { return "KEY=VALUE" }

func (f *kvFlag) Set(s string) error {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return fmt.Errorf("expected KEY=VALUE, got %q", s)
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[s[:i]] = s[i+1:]
	return nil
}
