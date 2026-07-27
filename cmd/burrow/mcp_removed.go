// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"

	"github.com/spf13/cobra"
)

// TEMPORARY — DELETE THIS FILE AFTER THE NEXT RELEASE.
//
// The MCP server is gone from the tree: the `mcp` package, the `burrow-mcp` binary, and the real
// `burrow mcp` command group were removed by
// [ADR-0062](../../docs/adr/0062-remove-the-mcp-server.md). Its §2 keeps a stub for at least one
// release so `burrow mcp ...` exits naming its replacement instead of Cobra's "unknown command":
// anyone reaching it is following a guide written before v0.12 dropped the server, and the useful
// answer is where to go, not that the word was not recognized.
//
// Once a release has shipped carrying this message, delete the file and the registration in
// main.go, and let Cobra's ordinary unknown-command error take over.

// mcpRemovedMessage is the whole value of the stub: it names the replacement command concretely
// enough to run. No em-dashes; it is user-facing output.
const mcpRemovedMessage = "`burrow mcp` has been removed: Burrow no longer ships an MCP server.\n\n" +
	"Wire your agent to burrow-agent, its scoped control channel, instead:\n" +
	"  burrow agent claude install\n\n" +
	"Preview what a tool needs first with `burrow agent <tool>`, or run `burrow agent` for the\n" +
	"supported tools."

// newMcpRemovedCmd builds the stub. It parses no flags and accepts any arguments so that a whole
// command line copied from an old guide (`burrow mcp claude install --deny-kubectl`) reaches the
// message rather than failing earlier on an unknown flag. It is hidden: the command is not part of
// the surface, it only answers for one.
func newMcpRemovedCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "mcp",
		Short:              "Removed: use `burrow agent <tool> install`",
		Hidden:             true,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New(mcpRemovedMessage)
		},
	}
}
