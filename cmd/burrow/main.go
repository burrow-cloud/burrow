// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow is the Burrow CLI: the human-facing way to operate Burrow. It calls the
// same control-plane API the MCP server does (ADR-0002) — deploy by image reference,
// status, logs, rollback, scale — and can build and push an image first (the client-side
// build path, ADR-0008). Like the MCP server it carries no orchestration logic and no
// cluster credentials, only the control-plane API token (ADR-0005).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "burrow:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command given")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "install":
		return cmdInstall(ctx, rest, stdout, stderr)
	case "deploy":
		return cmdDeploy(ctx, rest, stdout, stderr)
	case "status":
		return cmdStatus(ctx, rest, stdout, stderr)
	case "logs":
		return cmdLogs(ctx, rest, stdout, stderr)
	case "rollback":
		return cmdRollback(ctx, rest, stdout, stderr)
	case "scale":
		return cmdScale(ctx, rest, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `burrow — operate applications on your cluster through the Burrow control plane

Usage:
  burrow <command> [flags]

Commands:
  install          Install the Burrow control plane into your cluster
  deploy <app>     Deploy an app by image reference (optionally build & push first)
  status <app>     Show an app's release and live workload status
  logs <app>       Show recent logs for an app
  rollback <app>   Roll an app back to its previous release
  scale <app> <n>  Set an app's replica count

Configuration (flags override environment):
  --control-plane URL   control-plane API base URL   (env BURROW_CONTROL_PLANE_URL)
  --token TOKEN         control-plane API token      (env BURROW_API_TOKEN)
  --json                print the raw JSON result

Run "burrow <command> -h" for command-specific flags.
`)
}

// commonOpts holds the configuration every command shares.
type commonOpts struct {
	controlPlane string
	token        string
	kubeconfig   string
	namespace    string
	json         bool
}

// addCommon registers the shared flags on fs, defaulting from the environment.
func addCommon(fs *flag.FlagSet) *commonOpts {
	o := &commonOpts{}
	fs.StringVar(&o.controlPlane, "control-plane", os.Getenv("BURROW_CONTROL_PLANE_URL"), "control-plane API base URL (default: auto-connect via kubeconfig)")
	fs.StringVar(&o.token, "token", os.Getenv("BURROW_API_TOKEN"), "control-plane API token (default: read from the install Secret)")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig for auto-connect (default: ambient)")
	fs.StringVar(&o.namespace, "namespace", connect.DefaultNamespace, "namespace Burrow is installed in")
	fs.BoolVar(&o.json, "json", false, "print the raw JSON result")
	return o
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

// splitArgs separates leading positional arguments from flags. Burrow's CLI convention
// is positionals first, then flags (e.g. "burrow deploy web --image x"), which sidesteps
// the standard flag package halting at the first non-flag argument. App names and
// replica counts never start with "-", so the split is unambiguous.
func splitArgs(args []string) (positionals, flags []string) {
	i := 0
	for i < len(args) && (args[i] == "" || args[i][0] != '-') {
		i++
	}
	return args[:i], args[i:]
}

// arg returns the i-th positional, or "" if absent.
func arg(positionals []string, i int) string {
	if i < len(positionals) {
		return positionals[i]
	}
	return ""
}

// kvFlag collects repeated KEY=VALUE flags into a map.
type kvFlag struct{ m map[string]string }

func (f *kvFlag) String() string { return "" }

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
