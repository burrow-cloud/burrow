// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow-agent is the coding agent's control channel to Burrow: a capability-reduced,
// JSON-first command-line surface the agent invokes directly (ADR-0049). It carries only the
// READ-ONLY operate-verbs (Phase 1) — the mutating and admin verbs are STRUCTURALLY ABSENT, not
// compiled into this binary, a stronger boundary than a runtime deny list. It authenticates to the
// control plane with the scoped control-plane credential (ADR-0038) and holds no cluster credentials
// (ADR-0005): in self-host it reaches the in-cluster control plane through the scoped, burrowd-only
// agent kubeconfig `burrow install` mints and the Kubernetes API-server proxy (ADR-0014). Every
// command prints its result as indented JSON so the agent can pipe, grep, and jq it. Setting
// BURROW_AGENT_REQUIRE_SCOPED refuses the ambient-kubeconfig fallback entirely.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/agentconn"
	"github.com/burrow-cloud/burrow/localconfig"
)

// version is the Burrow version this binary reports to the control plane (ADR-0039), sent as
// X-Burrow-Client-Version through agentconn.
var version = "v0.1.0"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "burrow-agent:", err)
		os.Exit(1)
	}
}

// run builds the root command and executes it with args. It is the single entry point the tests
// drive; building a fresh command tree each call keeps flag defaults (read from the environment) and
// output writers isolated per invocation.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.ExecuteContext(ctx)
}

// connOpts holds the control-plane connection flags every command binds.
type connOpts struct {
	controlPlane string
	token        string
	kubeconfig   string
	context      string
	namespace    string
	env          string
}

// bindConn registers the shared connection flags, defaulting from the environment.
func bindConn(flags *pflag.FlagSet, o *connOpts) {
	flags.StringVar(&o.controlPlane, "control-plane", os.Getenv("BURROW_CONTROL_PLANE_URL"), "control-plane API base URL (default: auto-connect via kubeconfig)")
	flags.StringVar(&o.token, "token", os.Getenv("BURROW_API_TOKEN"), "control-plane API token (default: read from the install Secret)")
	flags.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig for auto-connect (default: ambient)")
	flags.StringVar(&o.context, "context", "", "kubeconfig context to target (default: current context)")
	flags.StringVar(&o.namespace, "namespace", connect.DefaultNamespace, "namespace Burrow is installed in")
}

// bindEnv registers the --env flag naming the environment to operate in (ADR-0035 phase 2b). It is
// bound only on the env-scoped commands; an empty value means the default environment.
func bindEnv(flags *pflag.FlagSet, o *connOpts) {
	flags.StringVar(&o.env, "env", "", "environment to operate in (default: the default environment)")
}

// resolve builds a control-plane client and the environment name to send with the operation,
// applying the same precedence as burrow-mcp through the shared agentconn resolver (ADR-0038,
// ADR-0049). With --control-plane it talks to that URL directly and sends the raw --env. Otherwise it
// resolves the active handle (the pinned one, or the current kube context in follow mode) and applies
// the flag overrides, defaulting to the scoped, burrowd-only agent credential. Strict mode
// (BURROW_AGENT_REQUIRE_SCOPED) refuses the ambient fallback.
func (o *connOpts) resolve(ctx context.Context, stderr io.Writer) (*client.Client, string, error) {
	strict := truthy(os.Getenv("BURROW_AGENT_REQUIRE_SCOPED"))
	if o.controlPlane != "" {
		factory, err := agentconn.NewFactory(ctx, agentconn.Config{
			ControlPlaneURL: o.controlPlane,
			Token:           o.token,
			Version:         version,
		}, stderr)
		if err != nil {
			return nil, "", err
		}
		c, err := factory("")
		if err != nil {
			return nil, "", err
		}
		return c, o.env, nil
	}

	cfg, err := localconfig.Load()
	if err != nil {
		return nil, "", err
	}
	resolved, err := localconfig.Resolve(cfg, o.kubeconfig)
	if err != nil {
		return nil, "", err
	}
	kubeContext := resolved.Context
	if o.context != "" {
		kubeContext = o.context
	}
	envName := resolved.Env
	if o.env != "" {
		envName = o.env
	}
	cpNamespace := resolved.ControlPlaneNamespace
	if o.namespace != "" && o.namespace != connect.DefaultNamespace {
		cpNamespace = o.namespace
	}
	factory, err := agentconn.NewFactory(ctx, agentconn.Config{
		ControlPlaneURL: o.controlPlane,
		Token:           o.token,
		Kubeconfig:      o.kubeconfig,
		Namespace:       cpNamespace,
		Strict:          strict,
		Version:         version,
	}, stderr)
	if err != nil {
		return nil, "", err
	}
	c, err := factory(kubeContext)
	if err != nil {
		return nil, "", err
	}
	return c, envName, nil
}

// truthy reports whether an environment-variable value enables a flag: 1, true, or yes
// (case-insensitive, surrounding whitespace ignored). Empty, 0, or anything else is off.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// emitJSON writes v as indented JSON — the only output mode in Phase 1, so the agent can pipe, grep,
// and jq every result (ADR-0049).
func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
