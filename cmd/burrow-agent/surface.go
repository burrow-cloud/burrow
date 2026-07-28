// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// commandPaths walks a command tree and returns every command path it registers, space-separated
// with the root's own name omitted (e.g. "addon install"). Cobra's generated `help` and
// `completion` commands are not part of Burrow's surface and are skipped.
//
// Both readers of the agent surface go through this: the surface-guard test
// (agent_surface_guard_test.go) compares it against the closed allow-list, and absentCapabilities
// subtracts it from the capability catalogue. Neither reads a copy of the registration code, so
// the answer is what the binary actually ships.
func commandPaths(root *cobra.Command) []string {
	var paths []string
	var walk func(c *cobra.Command, prefix string)
	walk = func(c *cobra.Command, prefix string) {
		for _, sub := range c.Commands() {
			switch sub.Name() {
			case "help", "completion":
				continue
			}
			path := strings.TrimSpace(prefix + " " + sub.Name())
			paths = append(paths, path)
			walk(sub, path)
		}
	}
	walk(root, "")
	sort.Strings(paths)
	return paths
}

// absentCapabilities reports the capabilities this binary does not carry, derived by subtracting
// the command paths it actually registers from the catalogue in internal/agentsurface
// ([ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §7).
//
// It is derived rather than listed because an absent verb is otherwise a DEAD END, and a dead end
// is what pushes an agent to route around the control channel entirely — reaching for `kubectl` or
// a shell, the failure ADR-0021 says Burrow cannot close from the inside (ADR-0065 §5). A verb
// that is absent and LEGIBLE is instead a refusal the agent can relay: "removing an add-on is not
// something I can do, and here is who can." A hand-maintained second list would eventually stop
// describing this binary and quietly turn that refusal back into a dead end; subtraction cannot.
func absentCapabilities(root *cobra.Command) []agentsurface.Capability {
	return agentsurface.AbsentFrom(commandPaths(root))
}
