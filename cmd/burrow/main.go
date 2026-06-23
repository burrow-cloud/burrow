// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command burrow is the Burrow CLI: the human-facing way to install Burrow into a
// cluster, build and push an image, and call the control plane directly (the same
// operations an agent drives over MCP). The CLI is a control-plane client; like the
// MCP server it carries no orchestration logic of its own.
//
// See docs/ARCHITECTURE.md and docs/adr/ for the design. The CLI is not implemented
// yet; this is a placeholder entry point so the module builds.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "burrow: the Burrow CLI is not implemented yet — see docs/PLAN.md")
	os.Exit(1)
}
