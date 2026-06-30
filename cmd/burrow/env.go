// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// envManifests is the per-environment namespace + RBAC template, embedded from
// manifests/env.yaml.tmpl. It invokes the shared "appNamespaceRole" define (parsed first), so an
// environment's namespace gets exactly the same app-namespace Role install grants (ADR-0035 phase 2).
//
//go:embed manifests/env.yaml.tmpl
var envManifests string

var envTemplate = template.Must(template.Must(template.New("env").Parse(appRoleManifest)).Parse(envManifests))

// envOptions are the values rendered into the per-environment manifests. AppNamespace is the
// environment's namespace (where its apps deploy); Namespace is the control-plane namespace where
// burrowd's ServiceAccount lives; ServiceAccount is that ServiceAccount's name.
type envOptions struct {
	Namespace      string
	AppNamespace   string
	ServiceAccount string
}

func renderEnvManifests(o envOptions) (string, error) {
	if o.ServiceAccount == "" {
		o.ServiceAccount = "burrowd"
	}
	var sb strings.Builder
	if err := envTemplate.Execute(&sb, o); err != nil {
		return "", fmt.Errorf("rendering environment manifests: %w", err)
	}
	return sb.String(), nil
}

// applyFn applies rendered manifests to the cluster with kubectl. It is a package var so a test can
// substitute a fake for the privileged kubeconfig-side apply, the way `burrow env add` does the
// namespace + RBAC setup (like install) before registering the environment with burrowd.
var applyFn = kubectlApply

// newEnvCmd is the single environment surface (ADR-0036). An environment is a user-named handle
// resolving to {context, control-plane-namespace, app-namespace}, stored client-side in
// ~/.burrow/config (the local selector state, like the kubeconfig). The bare `burrow env` and
// `burrow env list` show the handles kubectx-style and mark the active one; `use`/`follow` pin or
// unpin the selection; `rename` renames a handle; `add` creates a namespace-per-environment
// environment (server-side setup) and records its handle. It supersedes `burrow context`.
func newEnvCmd() *cobra.Command {
	o := &envListOpts{}
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Select and manage Burrow environments (local handles)",
		Long: "env is the single environment surface (ADR-0036). An environment is a user-named handle\n" +
			"resolving to {context, control-plane-namespace, app-namespace}, kept client-side in\n" +
			"~/.burrow/config (override with $BURROW_CONFIG), like the kubeconfig.\n\n" +
			"With nothing pinned, commands follow the current kube context, so `kubectx`/`kubens` move\n" +
			"Burrow too; pin a handle with `burrow env use <name>` and return to following with\n" +
			"`burrow env follow`. The bare `burrow env` lists the handles and marks the active one.\n\n" +
			"This is distinct from `burrow app config`, which sets an app's environment variables.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEnvList(cmd.OutOrStdout(), o)
		},
	}
	bindEnvListFlags(cmd.Flags(), o)
	cmd.AddCommand(newEnvListCmd(), newEnvUseCmd(), newEnvFollowCmd(), newEnvRenameCmd(), newEnvAddCmd())
	return cmd
}

// envListOpts are the inputs to listing handles: which kubeconfig to resolve the active target
// against, and whether to print JSON. Listing reads no cluster.
type envListOpts struct {
	kubeconfig string
	json       bool
}

// bindEnvListFlags registers the listing flags, shared by the bare `burrow env` and `burrow env
// list` so both accept --kubeconfig and --json.
func bindEnvListFlags(flags *pflag.FlagSet, o *envListOpts) {
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig used to resolve the followed context (default: ambient)")
	flags.BoolVar(&o.json, "json", false, "print the raw JSON result")
}

// newEnvListCmd lists the local handles, kubectx-style, marking the active one and its mode. It
// reads ~/.burrow/config and the kubeconfig only; it never contacts a cluster.
func newEnvListCmd() *cobra.Command {
	o := &envListOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the environment handles and mark the active one",
		Long: "list reads ~/.burrow/config and prints the environment handles, marking the active one\n" +
			"and whether it is pinned or following the current kube context. It contacts no cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEnvList(cmd.OutOrStdout(), o)
		},
	}
	bindEnvListFlags(cmd.Flags(), o)
	return cmd
}

// envListResult is the JSON shape of `burrow env list`: the registered handles plus the resolved
// active selection (its name, mode, and the followed/pinned context and namespace).
type envListResult struct {
	Environments []localconfig.Environment `json:"environments"`
	Current      string                    `json:"current"`
	Mode         string                    `json:"mode"`
	Context      string                    `json:"context"`
	Namespace    string                    `json:"namespace"`
}

func runEnvList(w io.Writer, o *envListOpts) error {
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	resolved, err := localconfig.Resolve(cfg, o.kubeconfig)
	if err != nil {
		return err
	}
	if o.json {
		return emit(w, true, envListResult{
			Environments: cfg.Environments,
			Current:      resolved.Name,
			Mode:         string(resolved.Mode),
			Context:      resolved.Context,
			Namespace:    resolved.Namespace,
		}, "")
	}
	writeEnvList(w, cfg.Environments, resolved)
	return nil
}

// writeEnvList prints the handles kubectx-style. The active row gets a trailing
// "<--- current (pinned)" or "<--- current (following kubectl)"; when following an unregistered
// context (no handle matches), a trailing line names it so the next command's target is never
// ambiguous (ADR-0036).
func writeEnvList(w io.Writer, envs []localconfig.Environment, resolved localconfig.Resolved) {
	if len(envs) == 0 {
		fmt.Fprintln(w, "No environments. Add one with `burrow env add <name>`.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tCONTEXT\tNAMESPACE")
		for _, e := range envs {
			row := fmt.Sprintf("%s\t%s\t%s", e.Name, e.Context, e.AppNamespace)
			if resolved.Name != "" && e.Name == resolved.Name {
				switch resolved.Mode {
				case localconfig.ModePinned:
					row += "\t<--- current (pinned)"
				case localconfig.ModeFollowing:
					row += "\t<--- current (following kubectl)"
				}
			}
			fmt.Fprintln(tw, row)
		}
		_ = tw.Flush()
	}
	if resolved.Mode == localconfig.ModeFollowing && resolved.Name == "" && resolved.Context != "" {
		fmt.Fprintf(w, "following kubectl: %s (unregistered)\n", resolved.Context)
	}
}

// newEnvUseCmd pins a handle so commands target it regardless of kube context switches (ADR-0036).
func newEnvUseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "use <name>",
		Short: "Pin a handle so commands target it until you run `burrow env follow`",
		Long: "use pins the named handle: commands then target it regardless of which context kubectl\n" +
			"points at. Return to following the current kube context with `burrow env follow`.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := localconfig.Load()
			if err != nil {
				return err
			}
			env, ok := cfg.Lookup(name)
			if !ok {
				return fmt.Errorf("environment %q is not a registered handle; see `burrow env list`", name)
			}
			cfg.Current = name
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pinned %q (context %q). Commands target it until you run `burrow env follow`.\n", name, env.Context)
			return nil
		},
	}
	return cmd
}

// newEnvFollowCmd clears the pin so commands track the current kube context again (ADR-0036). It is
// a sibling subcommand, not a flag on `use`.
func newEnvFollowCmd() *cobra.Command {
	var kubeconfig string
	cmd := &cobra.Command{
		Use:   "follow",
		Short: "Return to following the current kube context",
		Long: "follow clears any pinned handle so commands track whatever context kubectl points at\n" +
			"(the default). It is the inverse of `burrow env use`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := localconfig.Load()
			if err != nil {
				return err
			}
			cfg.Current = ""
			if err := cfg.Save(); err != nil {
				return err
			}
			resolved, err := localconfig.Resolve(cfg, kubeconfig)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Now following the current kube context: %s\n", resolved.Render())
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig used to resolve the followed context (default: ambient)")
	return cmd
}

// newEnvRenameCmd renames a handle, keeping it current if it was current (ADR-0036).
func newEnvRenameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename an environment handle",
		Long: "rename changes a handle's name. If the renamed handle is the pinned one, the pin follows\n" +
			"the new name. It changes only the local config; no cluster is contacted.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldName, newName := args[0], args[1]
			cfg, err := localconfig.Load()
			if err != nil {
				return err
			}
			if err := cfg.Rename(oldName, newName); err != nil {
				return err
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Renamed environment %q to %q.\n", oldName, newName)
			return nil
		},
	}
	return cmd
}

// newEnvAddCmd creates a namespace-per-environment environment: it applies the environment's
// namespace and burrowd's Role there with the user's kubeconfig (the privileged setup burrowd
// cannot do itself), registers the environment with burrowd (ADR-0035), and additionally records a
// local handle for it (ADR-0036) so `burrow env list` shows it. The namespace defaults to
// <app-namespace>-<name>; --namespace overrides it.
func newEnvAddCmd() *cobra.Command {
	o := &commonOpts{}
	var namespace, appNamespace string
	var verbose bool
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create an environment: its namespace, burrowd's Role there, the registry entry, and a local handle",
		Long: "add creates an environment for namespace-per-environment (ADR-0035). It applies the\n" +
			"environment's namespace and a burrowd Role/RoleBinding in it with your kubeconfig (the\n" +
			"same privileged setup install does, because burrowd holds only namespaced Roles and cannot\n" +
			"create namespaces or RBAC itself), registers the environment with the control plane, and\n" +
			"records a local handle for it (ADR-0036) so `burrow env list` shows it.\n\n" +
			"The namespace defaults to <app-namespace>-<name> (e.g. burrow-apps-staging); override it\n" +
			"with --namespace.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			ns := namespace
			if ns == "" {
				ns = appNamespace + "-" + name
			}
			return runEnvAdd(ctx, o, name, ns, verbose, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	// `--namespace` is the environment's namespace here (not the control-plane namespace), so bind
	// the client flags without it and put the control-plane namespace under --control-plane-namespace.
	bindClientFlags(cmd.Flags(), o)
	cmd.Flags().StringVar(&o.namespace, "control-plane-namespace", connect.DefaultNamespace, "control-plane namespace Burrow is installed in (where burrowd's ServiceAccount lives)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "namespace for the environment's apps (default: <app-namespace>-<name>)")
	cmd.Flags().StringVar(&appNamespace, "app-namespace", connect.DefaultAppNamespace, "the installed app namespace, used to derive the default environment namespace")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show every resource kubectl applies instead of a summary")
	return cmd
}

func runEnvAdd(ctx context.Context, o *commonOpts, name, envNamespace string, verbose bool, stdout, stderr io.Writer) error {
	// (a) Privileged kubeconfig-side setup: create the environment's namespace and grant burrowd a
	// Role there. o.namespace is the control-plane namespace where burrowd's ServiceAccount lives.
	manifests, err := renderEnvManifests(envOptions{Namespace: o.namespace, AppNamespace: envNamespace})
	if err != nil {
		return err
	}
	if err := applyFn(ctx, o.kubeconfig, manifests, verbose, stdout, stderr); err != nil {
		return err
	}

	// (b) Register the environment with burrowd over its authenticated control-plane API.
	c, err := o.client(ctx)
	if err != nil {
		return err
	}
	if err := c.AddEnvironment(ctx, name, envNamespace); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Environment %q created (namespace %q).\n", name, envNamespace)

	// (c) Record a local handle so the env joins `burrow env list` (ADR-0036). The handle's context
	// is the context this command targeted (the --context override, else the current context).
	ctxName, err := connect.TargetContextName(o.kubeconfig, o.context)
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	handle := localconfig.Environment{
		Name:                  name,
		Context:               ctxName,
		ControlPlaneNamespace: o.namespace,
		AppNamespace:          envNamespace,
	}
	if _, ok := cfg.Lookup(name); ok {
		// Re-running add for an existing handle refreshes its target rather than erroring.
		for i := range cfg.Environments {
			if cfg.Environments[i].Name == name {
				cfg.Environments[i] = handle
				break
			}
		}
	} else if err := cfg.Add(handle); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Recorded local handle %q (context %q). See `burrow env list`.\n", name, ctxName)
	return nil
}
