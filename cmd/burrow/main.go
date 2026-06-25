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
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "burrow:", err)
		os.Exit(1)
	}
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
	root.CompletionOptions.HiddenDefaultCmd = true
	root.AddCommand(
		newInstallCmd(),
		newUpgradeCmd(),
		newRegistryCmd(),
		newProviderCmd(),
		newDeployCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newRollbackCmd(),
		newScaleCmd(),
		newExposeCmd(),
		newUnexposeCmd(),
		newReachabilityCmd(),
		newGuardCmd(),
	)
	return root
}

// commonOpts holds the configuration the control-plane operations share.
type commonOpts struct {
	controlPlane string
	token        string
	kubeconfig   string
	namespace    string
	json         bool
}

// bindCommon registers the shared flags on the flag set, defaulting from the environment.
func bindCommon(flags *pflag.FlagSet, o *commonOpts) {
	flags.StringVar(&o.controlPlane, "control-plane", os.Getenv("BURROW_CONTROL_PLANE_URL"), "control-plane API base URL (default: auto-connect via kubeconfig)")
	flags.StringVar(&o.token, "token", os.Getenv("BURROW_API_TOKEN"), "control-plane API token (default: read from the install Secret)")
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig for auto-connect (default: ambient)")
	flags.StringVar(&o.namespace, "namespace", connect.DefaultNamespace, "namespace Burrow is installed in")
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
	return connect.Client(ctx, connect.Options{Kubeconfig: o.kubeconfig, Namespace: o.namespace})
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
