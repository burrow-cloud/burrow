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

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
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

// newEnvCmd groups the commands that manage namespace-per-environment environments (ADR-0035 phase
// 2): one cluster, several app namespaces, one per environment. It is a top-level `burrow env`,
// distinct from `app config` (an app's environment variables), which is unrelated.
func newEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage environments (namespace-per-environment: dev, staging, prod)",
		Long: "env manages environments for namespace-per-environment (ADR-0035): one cluster with a\n" +
			"separate app namespace per environment, so an agent can be given free rein in staging and\n" +
			"held back in prod. `burrow env add` creates an environment's namespace and grants burrowd a\n" +
			"Role there (privileged kubeconfig-side setup, like install); `burrow env list` shows them.\n\n" +
			"This is distinct from `burrow app config`, which sets an app's environment variables.",
	}
	cmd.AddCommand(newEnvAddCmd(), newEnvListCmd())
	return cmd
}

// newEnvAddCmd creates an environment: it applies the environment's namespace and burrowd's Role
// there with the user's kubeconfig (the privileged setup burrowd cannot do itself), then registers
// the environment with burrowd. The namespace defaults to <app-namespace>-<name>; --namespace
// overrides it.
func newEnvAddCmd() *cobra.Command {
	o := &commonOpts{}
	var namespace, appNamespace string
	var verbose bool
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create an environment: its namespace, burrowd's Role there, and the registry entry",
		Long: "add creates an environment for namespace-per-environment (ADR-0035). It applies the\n" +
			"environment's namespace and a burrowd Role/RoleBinding in it with your kubeconfig — the\n" +
			"same privileged setup install does, because burrowd holds only namespaced Roles and cannot\n" +
			"create namespaces or RBAC itself — then registers the environment with the control plane.\n\n" +
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
	return nil
}

// newEnvListCmd lists the environments the control plane knows about, marking the default.
func newEnvListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the environments and mark the default",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			envs, err := c.ListEnvironments(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, envs, "")
			}
			writeEnvList(out, envs)
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// writeEnvList prints the environments (default first, as the engine orders them), marking the
// default one with a *.
func writeEnvList(w io.Writer, envs []client.Environment) {
	if len(envs) == 0 {
		fmt.Fprintln(w, "No environments.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEFAULT\tNAME\tNAMESPACE")
	for _, e := range envs {
		marker := ""
		if e.Default {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", marker, e.Name, e.Namespace)
	}
	_ = tw.Flush()
}
