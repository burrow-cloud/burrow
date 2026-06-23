// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Command burrowd is the Burrow control plane: the component that holds the
// cluster credentials, runs the deploy/rollout/rollback/logs/scale orchestration,
// enforces the guardrails, and records who deployed what. It is the product.
//
// See docs/ARCHITECTURE.md and docs/adr/ for the design. No control-plane logic
// has shipped yet; this is a placeholder entry point so the module builds.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "burrowd: the Burrow control plane is not implemented yet — see docs/PLAN.md")
	os.Exit(1)
}
