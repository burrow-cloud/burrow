// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow-mcp is the Burrow MCP server: the thin, agent-neutral control surface
// that exposes Burrow's tools to any MCP client and translates tool calls into
// control-plane API calls (ADR-0003). It holds no cluster-operating credentials (ADR-0005);
// in self-host it reaches the in-cluster control plane through the scoped, burrowd-only agent
// kubeconfig `burrow install` mints (ADR-0038) and the Kubernetes API-server proxy (ADR-0014),
// falling back to the developer's ambient kubeconfig when a handle records no scoped credential.
// It speaks MCP over stdio, so an agent launches it as a subprocess.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
//     requested one (ADR-0038 phase 2), so the agent's reachable credential is confined to burrowd.
//  4. the ambient kubeconfig — the fallback for a context with no matching handle or scoped file.
//
// So an agent can launch burrow-mcp with no configuration beyond kubectl access, and a registered
// environment reaches only burrowd. The proxy-path factory is concurrency-safe and caches one
// client per context (an empty context is the current kubeconfig context).
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
	var mu sync.Mutex
	cache := map[string]*client.Client{}
	return func(kubeContext string) (*client.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := cache[kubeContext]; ok {
			return c, nil
		}
		c, err := connect.Client(ctx, connectOptions(kubeContext, kubeconfig, namespace, stderr))
		if err != nil {
			return nil, err
		}
		cache[kubeContext] = c
		return c, nil
	}, nil
}

// connectOptions builds the auto-connect options for a requested kube context, applying the
// BURROW_KUBECONFIG > scoped-per-handle > ambient precedence (BURROW_CONTROL_PLANE_URL is handled
// earlier, in clientFactory). With an explicit BURROW_KUBECONFIG it uses that unchanged; otherwise
// it defaults to the scoped, burrowd-only agent kubeconfig recorded for the handle whose context
// matches, falling back to the ambient kubeconfig (ADR-0038 phase 2).
func connectOptions(kubeContext, kubeconfig, namespace string, stderr io.Writer) connect.Options {
	opts := connect.Options{Kubeconfig: kubeconfig, Namespace: namespace, Context: kubeContext}
	if kubeconfig == "" {
		if agentKubeconfig, agentContext, ok := scopedAgentKubeconfig(kubeContext, stderr); ok {
			opts.Kubeconfig = agentKubeconfig
			opts.Context = agentContext
		}
	}
	return opts
}

// scopedAgentKubeconfig resolves the scoped, burrowd-only kubeconfig for a requested kube context
// (ADR-0038 phase 2): it finds the local handle whose context matches and returns its recorded
// AgentKubeconfig/AgentContext when the file is present. It reports false (fall back to ambient) for
// an empty or unregistered context, a handle with no scoped credential (a cluster installed before
// phase 1, or a context joined out of band), or a recorded-but-missing file — printing a brief note
// only in that last case, since `burrow install` re-creates the credential.
func scopedAgentKubeconfig(kubeContext string, stderr io.Writer) (kubeconfig, kubeContextOut string, ok bool) {
	if kubeContext == "" {
		return "", "", false
	}
	cfg, err := localconfig.Load()
	if err != nil {
		return "", "", false
	}
	env, found := cfg.LookupByContext(kubeContext)
	if !found || env.AgentKubeconfig == "" {
		return "", "", false
	}
	if _, err := os.Stat(env.AgentKubeconfig); err != nil {
		fmt.Fprintf(stderr, "burrow-mcp: scoped agent kubeconfig %s is missing; using the ambient kubeconfig (run \"burrow install\" to re-create it)\n", env.AgentKubeconfig)
		return "", "", false
	}
	return env.AgentKubeconfig, env.AgentContext, true
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
