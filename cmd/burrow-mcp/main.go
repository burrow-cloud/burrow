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
	c, err := controlPlaneClient(ctx)
	if err != nil {
		return err
	}
	return burrowmcp.Serve(ctx, c, version)
}

// controlPlaneClient builds a control-plane client. With BURROW_CONTROL_PLANE_URL set it
// talks to that URL directly (e.g. an ingress) using BURROW_API_TOKEN. Otherwise it
// auto-connects through the Kubernetes API-server proxy using the ambient kubeconfig and
// reads the token from the install Secret (ADR-0014) — so an agent can launch burrow-mcp
// with no configuration beyond kubectl access.
func controlPlaneClient(ctx context.Context) (*client.Client, error) {
	if baseURL := os.Getenv("BURROW_CONTROL_PLANE_URL"); baseURL != "" {
		token := os.Getenv("BURROW_API_TOKEN")
		if token == "" {
			return nil, errors.New("BURROW_API_TOKEN is required with BURROW_CONTROL_PLANE_URL")
		}
		return client.NewClient(baseURL, token), nil
	}
	return connect.Client(ctx, connect.Options{
		Kubeconfig: os.Getenv("BURROW_KUBECONFIG"),
		Namespace:  envOr("BURROW_NAMESPACE", connect.DefaultNamespace),
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
