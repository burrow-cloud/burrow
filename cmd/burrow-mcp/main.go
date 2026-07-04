// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow-mcp is the Burrow MCP server: the thin, agent-neutral control surface
// that exposes Burrow's tools to any MCP client and translates tool calls into
// control-plane API calls (ADR-0003). It holds no cluster-operating credentials (ADR-0005);
// in self-host it reaches the in-cluster control plane through the scoped, burrowd-only agent
// kubeconfig `burrow install` mints (ADR-0038) and the Kubernetes API-server proxy (ADR-0014).
// Unlike the human CLI, it fails closed: a handle that records a scoped credential whose file is
// missing is an error, never a silent escalation to the ambient/admin kubeconfig, and setting
// BURROW_MCP_REQUIRE_SCOPED refuses the ambient fallback entirely. Absent that strict mode, a
// handle that records no scoped credential (a pre-scoped-credential cluster) still falls back to
// the ambient kubeconfig for backward compatibility. It speaks MCP over stdio, so an agent launches
// it as a subprocess.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
	burrowmcp "github.com/burrow-cloud/burrow/mcp"
)

// version is the Burrow version this binary reports to MCP clients.
var version = "v0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "burrow-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	clientFor, err := clientFactory(ctx, os.Stderr)
	if err != nil {
		return err
	}
	// The kubeconfig path is used only to mark which local handle is current in burrow_environments;
	// it never selects an agent's target (ADR-0036). An env argument resolves through the local
	// handle config ($BURROW_CONFIG, else ~/.burrow/config), read by the MCP server per call.
	kubeconfig := os.Getenv("BURROW_KUBECONFIG")
	return burrowmcp.Serve(ctx, clientFor, kubeconfig, version)
}

// clientFactory builds the per-context control-plane client factory the MCP server uses to target
// one environment per call (ADR-0035). Its credential precedence is, highest first:
//
//  1. BURROW_CONTROL_PLANE_URL — a direct URL (e.g. an ingress) with BURROW_API_TOKEN; it names
//     exactly one control plane, so the per-call context does not apply and every call uses it.
//  2. BURROW_KUBECONFIG — an explicit kubeconfig used for every context.
//  3. the scoped, burrowd-only agent kubeconfig recorded for the handle whose context matches the
//     requested one (ADR-0038), so the agent's reachable credential is confined to burrowd.
//  4. the ambient kubeconfig — the fallback for a context with no matching handle or scoped credential.
//
// Unlike the human CLI, burrow-mcp fails closed (ADR-0038): step 4 is refused when the handle
// records a scoped credential whose file is missing (always an error, never a silent escalation to
// admin), and when BURROW_MCP_REQUIRE_SCOPED is set the ambient fallback is refused entirely — only
// the explicit escape hatches (steps 1 and 2, the operator's deliberate choice) remain. So an agent
// can launch burrow-mcp with no configuration beyond kubectl access, and a registered environment
// reaches only burrowd. The proxy-path factory is concurrency-safe and caches one client per context
// (an empty context is the current kubeconfig context).
func clientFactory(ctx context.Context, stderr io.Writer) (burrowmcp.ClientForContext, error) {
	if baseURL := os.Getenv("BURROW_CONTROL_PLANE_URL"); baseURL != "" {
		token := os.Getenv("BURROW_API_TOKEN")
		if token == "" {
			return nil, errors.New("BURROW_API_TOKEN is required with BURROW_CONTROL_PLANE_URL")
		}
		c := client.NewClient(baseURL, token)
		return func(string) (*client.Client, error) { return c, nil }, nil
	}

	kubeconfig := os.Getenv("BURROW_KUBECONFIG")
	namespace := envOr("BURROW_NAMESPACE", connect.DefaultNamespace)
	strict := truthy(os.Getenv("BURROW_MCP_REQUIRE_SCOPED"))
	var mu sync.Mutex
	cache := map[string]*client.Client{}
	return func(kubeContext string) (*client.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := cache[kubeContext]; ok {
			return c, nil
		}
		opts, err := connectOptions(kubeContext, kubeconfig, namespace, strict, stderr)
		if err != nil {
			return nil, err
		}
		// Route through the same kubeconfig transport the CLI uses, so MCP and the CLI share one
		// seam (ADR-0045). The transport stays credential-free here: it reads the burrowd API token
		// from the install Secret over the human's proxy, holding no cluster-operating credential
		// of its own (ADR-0005).
		c, err := connect.KubeconfigTransport{Options: opts}.Connect(ctx)
		if err != nil {
			return nil, err
		}
		cache[kubeContext] = c
		return c, nil
	}, nil
}

// connectOptions builds the auto-connect options for a requested kube context, applying the
// BURROW_KUBECONFIG > scoped-per-handle > ambient precedence (BURROW_CONTROL_PLANE_URL is handled
// earlier, in clientFactory). An explicit BURROW_KUBECONFIG is the operator's deliberate choice and
// is used unchanged (allowed even in strict mode); otherwise it defaults to the scoped, burrowd-only
// agent kubeconfig recorded for the matching handle (ADR-0038). It fails closed: a recorded scoped
// credential whose file is missing is always an error, and in strict mode a handle with no scoped
// credential is an error too, rather than escalating to the ambient/admin kubeconfig.
func connectOptions(kubeContext, kubeconfig, namespace string, strict bool, stderr io.Writer) (connect.Options, error) {
	opts := connect.Options{Kubeconfig: kubeconfig, Namespace: namespace, Context: kubeContext}
	if kubeconfig != "" {
		return opts, nil
	}
	agentKubeconfig, agentContext, err := scopedAgentKubeconfig(kubeContext, strict, stderr)
	if err != nil {
		return connect.Options{}, err
	}
	if agentKubeconfig != "" {
		opts.Kubeconfig = agentKubeconfig
		opts.Context = agentContext
	}
	return opts, nil
}

// scopedAgentKubeconfig resolves the scoped, burrowd-only kubeconfig for a requested kube context
// (ADR-0038). It finds the local handle whose context matches and returns its recorded
// AgentKubeconfig/AgentContext when the file is present. It fails closed in two cases:
//
//   - A handle that records a scoped credential whose file is missing is ALWAYS an error (even in
//     non-strict mode): a handle that declares a scoped credential and then can't find it is a
//     misconfiguration, never a reason to silently escalate to the ambient/admin kubeconfig.
//   - In strict mode (BURROW_MCP_REQUIRE_SCOPED), a context with no scoped credential at all — an
//     empty or unregistered context, or a handle installed before the scoped credential existed — is
//     an error too, so the ambient fallback is refused.
//
// Otherwise, for a context with no scoped credential it returns "", "", nil to signal the non-strict
// ambient fallback (backward compatibility for pre-scoped-credential clusters).
func scopedAgentKubeconfig(kubeContext string, strict bool, stderr io.Writer) (kubeconfig, kubeContextOut string, err error) {
	env, found := lookupByContext(kubeContext)
	if !found || env.AgentKubeconfig == "" {
		if strict {
			return "", "", fmt.Errorf("BURROW_MCP_REQUIRE_SCOPED is set but no scoped agent credential is available for context %q; run \"burrow install\"/\"burrow upgrade\" to mint one, or unset BURROW_MCP_REQUIRE_SCOPED.", kubeContext)
		}
		return "", "", nil
	}
	if _, err := os.Stat(env.AgentKubeconfig); err != nil {
		return "", "", fmt.Errorf("scoped agent kubeconfig %q for environment %q is missing; run \"burrow upgrade\" (or \"burrow install\") to re-mint it. Refusing to fall back to the ambient kubeconfig.", env.AgentKubeconfig, env.Name)
	}
	return env.AgentKubeconfig, env.AgentContext, nil
}

// lookupByContext loads the local handle config and returns the environment registered for a kube
// context, and whether one exists. An empty context or a load failure reports no match, which in
// non-strict mode means the ambient fallback and in strict mode is refused by the caller.
func lookupByContext(kubeContext string) (localconfig.Environment, bool) {
	if kubeContext == "" {
		return localconfig.Environment{}, false
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return localconfig.Environment{}, false
	}
	return cfg.LookupByContext(kubeContext)
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
