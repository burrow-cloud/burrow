// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow-mcp is the Burrow MCP server: the thin, agent-neutral control surface
// that exposes Burrow's tools to any MCP client and translates tool calls into
// control-plane API calls (ADR-0003). It holds no cluster-operating credentials (ADR-0005);
// in self-host it reaches the in-cluster control plane through the developer's kubeconfig
// and the Kubernetes API-server proxy (ADR-0014), just like the CLI. It speaks MCP over
// stdio, so an agent launches it as a subprocess.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
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
	clientFor, err := clientFactory(ctx)
	if err != nil {
		return err
	}
	return burrowmcp.Serve(ctx, clientFor, contextLister(), version)
}

// clientFactory builds the per-context control-plane client factory the MCP server uses to target
// one environment per call (ADR-0035). With BURROW_CONTROL_PLANE_URL set it talks to that URL
// directly (e.g. an ingress) using BURROW_API_TOKEN; a direct URL names exactly one control plane,
// so the per-call context does not apply and every call uses it. Otherwise it auto-connects through
// the Kubernetes API-server proxy using the ambient kubeconfig, building a client per context and
// reading each cluster's own install-Secret token (ADR-0014) — so an agent can launch burrow-mcp
// with no configuration beyond kubectl access. The proxy-path factory is concurrency-safe and
// caches one client per context (an empty context is the current kubeconfig context).
func clientFactory(ctx context.Context) (burrowmcp.ClientForContext, error) {
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
		c, err := connect.Client(ctx, connect.Options{
			Kubeconfig: kubeconfig,
			Namespace:  namespace,
			Context:    kubeContext,
		})
		if err != nil {
			return nil, err
		}
		cache[kubeContext] = c
		return c, nil
	}, nil
}

// contextLister lists the kubeconfig contexts the agent can target (ADR-0035), reusing the same
// helper as `burrow context list` and backing the burrow_contexts tool. It reads the ambient
// kubeconfig (or BURROW_KUBECONFIG) and contacts no cluster.
func contextLister() burrowmcp.ContextLister {
	kubeconfig := os.Getenv("BURROW_KUBECONFIG")
	return func() ([]connect.Context, error) {
		return connect.Contexts(kubeconfig)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
