// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow-mcp is the Burrow MCP server: the thin, agent-neutral control
// surface that exposes Burrow's tools to any MCP client and translates tool calls
// into control-plane calls. It holds NO cluster credentials and contains no
// orchestration logic — it is the remote control, not the engine.
//
// See docs/ARCHITECTURE.md and docs/adr/ for the design. The MCP server is not
// implemented yet; this is a placeholder entry point so the module builds.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "burrow-mcp: the Burrow MCP server is not implemented yet — see docs/PLAN.md")
	os.Exit(1)
}
