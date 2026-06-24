// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow-mcp is the Burrow MCP server: the thin, agent-neutral control surface
// that exposes Burrow's tools to any MCP client and translates tool calls into
// control-plane API calls (ADR-0003). It holds the control-plane API token but NO
// cluster credentials — those live only in the control plane (ADR-0005). It speaks MCP
// over stdio, so an agent launches it as a subprocess.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/burrow-cloud/burrow/client"
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
	baseURL := os.Getenv("BURROW_CONTROL_PLANE_URL")
	if baseURL == "" {
		return errors.New("BURROW_CONTROL_PLANE_URL is required (the control-plane API base URL, e.g. http://burrowd:8080)")
	}
	token := os.Getenv("BURROW_API_TOKEN")
	if token == "" {
		return errors.New("BURROW_API_TOKEN is required (the bearer token to authenticate to the control plane)")
	}

	c := client.NewClient(baseURL, token)
	return burrowmcp.Serve(context.Background(), c, version)
}
