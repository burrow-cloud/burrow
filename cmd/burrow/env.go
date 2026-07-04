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

// applyFn applies rendered manifests to the cluster with client-go server-side apply (ADR-0037), so
// no kubectl binary is required. It is a package var so a test can substitute a fake for the
// privileged kubeconfig-side apply, the way `burrow env add` does the namespace + RBAC setup (like
// install) before registering the environment with burrowd.
var applyFn = serverSideApply

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
		Short: "Select and manage Burrow environments",
		Long: "Select and manage Burrow environments. An environment is a named handle for a cluster and\n" +
			"namespace; your commands target the active one. With nothing pinned, Burrow follows your\n" +
			"current kube context.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEnvList(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	bindEnvListFlags(cmd.Flags(), o)
	cmd.AddCommand(newEnvListCmd(), newEnvUseCmd(), newEnvFollowCmd(), newEnvRenameCmd(), newEnvAddCmd())
	return cmd
}

// envListOpts are the inputs to listing handles: which kubeconfig to resolve the active target
// against, and whether to print JSON. Bare listing reads no cluster; discover adds the networked
// probe-and-register pass (namespace is the control-plane namespace it probes, used only then).
type envListOpts struct {
	kubeconfig string
	json       bool
	discover   bool
	namespace  string
}

// bindEnvListFlags registers the listing flags shared by the bare `burrow env` and `burrow env
// list` so both accept --kubeconfig and --json. The discover-only flags (--discover, --namespace)
// are bound separately, on `list` alone.
func bindEnvListFlags(flags *pflag.FlagSet, o *envListOpts) {
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig used to resolve the followed context (default: ambient)")
	flags.BoolVar(&o.json, "json", false, "print the raw JSON result")
}

// newEnvListCmd lists the local handles, kubectx-style, marking the active one and its mode. By
// default it reads ~/.burrow/config and the kubeconfig only and contacts no cluster. With
// --discover it first probes every kube context for an installed Burrow and registers a handle for
// each installed context that has none yet, then prints the now-updated list.
func newEnvListCmd() *cobra.Command {
	o := &envListOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the environment handles and mark the active one",
		Long: "list reads ~/.burrow/config and prints the environment handles, marking the active one\n" +
			"and whether it is pinned or following the current kube context. By default it contacts no\n" +
			"cluster and mutates nothing.\n\n" +
			"With --discover it first walks every context in your kubeconfig and probes each cluster for\n" +
			"an installed Burrow control plane (in the control-plane namespace, default \"burrow\"; override\n" +
			"with --namespace). It prints what it finds, registers a local handle for each installed\n" +
			"context that does not have one yet, then prints the updated list. Discovery reads clusters\n" +
			"but changes only ~/.burrow/config (override with $BURROW_CONFIG); it never modifies a cluster.\n" +
			"To install Burrow into a cluster that has none, use `burrow install`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEnvList(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	bindEnvListFlags(cmd.Flags(), o)
	cmd.Flags().BoolVar(&o.discover, "discover", false, "probe every kube context for an installed Burrow and register the ones it finds")
	cmd.Flags().StringVar(&o.namespace, "namespace", connect.DefaultNamespace, "control-plane namespace to probe for burrowd (only with --discover)")
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

func runEnvList(ctx context.Context, w io.Writer, o *envListOpts) error {
	// --discover turns list into the networked probe-and-register pass: walk every context, probe
	// each for an installed Burrow, and register a handle for any installed context without one. The
	// human form prints the probe report ahead of the list; JSON stays quiet and reflects only the
	// registered result. Without --discover, list stays offline and read-only.
	if o.discover {
		rows, added, err := discoverEnvironments(ctx, o.kubeconfig, o.namespace)
		if err != nil {
			return err
		}
		if !o.json {
			writeDiscoverReport(w, rows, added, o.namespace)
			fmt.Fprintln(w)
		}
	}

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

// envFooter points to the full environment command list, printed after both the populated table
// and the empty-state block so the surface is discoverable from either.
const envFooter = "Run `burrow env -h` for all environment commands."

// writeEnvList prints the handles as a CURRENT/NAME/CONTEXT/NAMESPACE table, matching the install
// context listing's column style: the active env carries a "*" in the CURRENT column. A single
// legend below the table explains the marker and the active mode in plain words (a user need not
// know what "pinned" means). When following an unregistered context (no handle matches), a line
// names it instead, since no row is marked (ADR-0036). With no handles at all it prints a structured
// empty-state instead, routing a new user to the ways to register one. Both forms close with the
// help footer.
func writeEnvList(w io.Writer, envs []localconfig.Environment, resolved localconfig.Resolved) {
	if len(envs) == 0 {
		writeEnvEmptyState(w, resolved)
		return
	}
	active := resolved.Name != ""
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tNAME\tCONTEXT\tNAMESPACE")
	for _, e := range envs {
		marker := ""
		if active && e.Name == resolved.Name {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, e.Name, e.Context, e.AppNamespace)
	}
	_ = tw.Flush()
	switch {
	case active && resolved.Mode == localconfig.ModePinned:
		fmt.Fprintln(w, "\n* current environment, pinned. Return to following your kube context with `burrow env follow`.")
	case active: // following a registered context
		fmt.Fprintln(w, "\n* current environment, following your kube context. Pin one with `burrow env use <name>`.")
	case resolved.Mode == localconfig.ModeFollowing && resolved.Context != "":
		fmt.Fprintf(w, "\nfollowing kubectl: %s (unregistered)\n", resolved.Context)
	}
	fmt.Fprintf(w, "\n%s\n", envFooter)
}

// writeEnvEmptyState renders the zero-handle block: the active kube context (always following, since
// nothing can be pinned with no handles), the three ways to register an environment with their
// commands and descriptions column-aligned, and the help footer. It replaces the prior bare
// one-liner so a brand-new user sees the affordances and usage, not a dead end.
func writeEnvEmptyState(w io.Writer, resolved localconfig.Resolved) {
	fmt.Fprintln(w, "Active environment")
	fmt.Fprintf(w, "  following kubectl context: %s   (no handle registered)\n\n", resolved.Context)
	fmt.Fprintln(w, "No environments registered yet:")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "  burrow env list --discover\tdiscover and register an existing Burrow")
	fmt.Fprintln(tw, "  burrow install <context>\tinstall Burrow into a cluster")
	fmt.Fprintln(tw, "  burrow env add <name>\tcreate a namespace-scoped environment")
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%s\n", envFooter)
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
		Short: "Create a namespace-scoped environment and register it.",
		Long: "add creates an environment for namespace-per-environment. It applies the environment's\n" +
			"namespace and a burrowd Role/RoleBinding in it with your kubeconfig (the same privileged\n" +
			"setup install does, because burrowd holds only namespaced Roles and cannot create\n" +
			"namespaces or RBAC itself), registers the environment with the control plane, and records\n" +
			"a local handle for it so `burrow env list` shows it.\n\n" +
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
	cmd.Flags().BoolVar(&verbose, "verbose", false, "show every resource burrow applies instead of a summary")
	return cmd
}

func runEnvAdd(ctx context.Context, o *commonOpts, name, envNamespace string, verbose bool, stdout, stderr io.Writer) error {
	// (a) Privileged kubeconfig-side setup: create the environment's namespace and grant burrowd a
	// Role there. o.namespace is the control-plane namespace where burrowd's ServiceAccount lives.
	manifests, err := renderEnvManifests(envOptions{Namespace: o.namespace, AppNamespace: envNamespace})
	if err != nil {
		return err
	}
	if err := applyFn(ctx, o.kubeconfig, o.context, manifests, verbose, stdout, stderr); err != nil {
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
		// Namespace-per-environment: commands send burrowd this registered NAME (the same one just
		// registered above), which burrowd maps to envNamespace and the env's guardrails (ADR-0036).
		// burrowd resolves the NAME, never the raw namespace, and errors on an unknown one.
		Env: name,
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
