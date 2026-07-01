// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Burrow is driven by an AI agent talking to its MCP server (ADR-0002/0003), so getting connected
// means pointing a coding agent at the `burrow-mcp` stdio server. `burrow mcp` does that without the
// user hand-editing a config file. It previews by default and mutates only when the user appends
// `install`, so it never surprises them: `burrow mcp <tool>` shows exactly what will change, and
// `burrow mcp <tool> install` applies it (idempotently, backing up any file it edits).

// runCommand runs an external command with its output wired to the terminal. It is a package var so
// a test can fake the agent-CLI invocations without a real CLI on PATH.
var runCommand = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// commandSucceeds runs an external command and reports whether it exited zero, discarding its
// output. It is the presence-check seam (e.g. `claude mcp get burrow`) so an adapter can tell
// whether Burrow is already configured without mutating anything; a test fakes it.
var commandSucceeds = func(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	return cmd.Run() == nil
}

// mcpLookPath resolves a tool binary on PATH. It is a package var so a test can force the
// found/not-found branches without depending on what is installed on the machine.
var mcpLookPath = exec.LookPath

// cursorConfigPath resolves Cursor's MCP config file. Cursor has no CLI, so it is edited directly;
// the path is a package var so a test can point it at a temp dir and exercise the real
// create/merge/backup logic safely.
var cursorConfigPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}

// cursorConfigDisplay is the tilde form shown in help and messages, independent of where the seam
// actually resolves (a temp dir in tests), so the user always sees the familiar path.
const cursorConfigDisplay = "~/.cursor/mcp.json"

// mcpTryPrompt is appended after any successful install, giving the user a concrete first thing to
// ask their agent. It leads with a blank line so it sits below the success line. No em-dashes.
const mcpTryPrompt = "\nThen open your agent and try:\n" +
	"  \"Deploy ghcr.io/me/app:1.4 and serve it at example.com over HTTPS.\"\n"

// mcpOverview is what bare `burrow mcp` prints: what it does, the supported tools, and how to
// preview then apply. No em-dashes: it is user-facing CLI output.
const mcpOverview = "Connect Burrow to your AI agent so it can operate your cluster.\n\n" +
	"Supported tools:\n" +
	"  claude    Claude Code\n" +
	"  cursor    Cursor\n" +
	"  codex     Codex\n" +
	"  copilot   Copilot\n\n" +
	"Preview what will be added:\n" +
	"  burrow mcp <tool>\n\n" +
	"Apply it:\n" +
	"  burrow mcp <tool> install\n"

// mcpTool is one coding-agent adapter: it can render a preview of what connecting it entails and
// apply that change. Each adapter is small and seam-isolated so it is unit-testable without a real
// agent or a real home directory.
type mcpTool interface {
	preview() string
	install(w io.Writer) error
}

// mcpTools maps the tool argument to its adapter. Keep it in sync with the supported-tools list in
// mcpOverview and the valid-tools error below.
var mcpTools = map[string]mcpTool{
	"claude":  cliTool{key: "claude", bin: "claude", display: "Claude Code", addArgs: []string{"mcp", "add", "--scope", "user", "burrow", "--", "burrow-mcp"}},
	"cursor":  cursorTool{},
	"codex":   cliTool{key: "codex", bin: "codex", display: "Codex", addArgs: []string{"mcp", "add", "burrow", "--", "burrow-mcp"}},
	"copilot": cliTool{key: "copilot", bin: "copilot", display: "Copilot", addArgs: []string{"mcp", "add", "burrow", "--", "burrow-mcp"}},
}

const mcpValidTools = "claude, cursor, codex, copilot"

func newMcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp [tool] [install]",
		Short: "Connect Burrow to your AI agent",
		Long: "Connect Burrow to your AI agent so it can operate your cluster.\n\n" +
			"Preview what a tool needs with `burrow mcp <tool>`, then apply it with\n" +
			"`burrow mcp <tool> install`. The change is idempotent, so a second run is safe, and any\n" +
			"file it edits is backed up first. Supported tools: " + mcpValidTools + ".",
		Example: "  # See what connecting Claude Code will add\n" +
			"  burrow mcp claude\n\n" +
			"  # Apply it\n" +
			"  burrow mcp claude install",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMcp(args, cmd.OutOrStdout())
		},
	}
	return cmd
}

// runMcp routes the positional args: none prints the overview, one previews a tool, and two applies
// it when the second arg is the literal `install`. It mutates nothing except on the two-arg install
// path.
func runMcp(args []string, w io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(w, mcpOverview)
		return nil
	}
	tool, ok := mcpTools[args[0]]
	if !ok {
		return fmt.Errorf("unknown tool %q; valid tools are %s", args[0], mcpValidTools)
	}
	if len(args) == 1 {
		fmt.Fprint(w, tool.preview())
		return nil
	}
	if args[1] != "install" {
		return fmt.Errorf("unknown argument %q; to apply, run `burrow mcp %s install`", args[1], args[0])
	}
	return tool.install(w)
}

// --- cliTool: agents with a native `mcp` CLI (claude, codex, copilot) ---

// cliTool is the generic adapter for a coding agent that ships an `mcp` subcommand of the identical
// shape: `<bin> mcp get burrow` reports whether Burrow is configured, and `<bin> mcp add ... burrow
// -- burrow-mcp` adds it. Only the binary, its display name, and the exact add args differ (claude
// needs `--scope user`), so one adapter parameterized by those three fields covers all three tools.
type cliTool struct {
	key     string   // the `burrow mcp <key>` argument, matching the binary name
	bin     string   // the CLI binary to run and look up on PATH
	display string   // the human name shown in messages, e.g. "Claude Code"
	addArgs []string // the exact args to `bin` that add Burrow, e.g. mcp add [--scope user] burrow -- burrow-mcp
}

// addCommand renders the full add invocation as a copy-pasteable command line for previews and the
// not-on-PATH hint.
func (t cliTool) addCommand() string {
	return t.bin + " " + strings.Join(t.addArgs, " ")
}

// configured reports whether the agent already has a `burrow` MCP server. It is only meaningful when
// the CLI is on PATH, so it gates on LookPath first.
func (t cliTool) configured() bool {
	if _, err := mcpLookPath(t.bin); err != nil {
		return false
	}
	return commandSucceeds(t.bin, "mcp", "get", "burrow")
}

func (t cliTool) preview() string {
	if t.configured() {
		return fmt.Sprintf("Burrow is already configured in %s. Nothing to do.\n", t.display)
	}
	return fmt.Sprintf("Connect Burrow to %s.\n\n"+
		"This will run:\n"+
		"  %s\n\n"+
		"burrow-mcp is a stdio MCP server that uses your kubeconfig and active environment, so no extra config is needed.\n\n"+
		"Run `burrow mcp %s install` to apply.\n", t.display, t.addCommand(), t.key)
}

func (t cliTool) install(w io.Writer) error {
	if _, err := mcpLookPath(t.bin); err != nil {
		fmt.Fprintf(w, "%s CLI (%s) not found on PATH. Install it, or run this yourself:\n  %s\n", t.display, t.bin, t.addCommand())
		return nil
	}
	// `mcp add` errors if a server named burrow already exists, so pre-check and no-op instead,
	// keeping a repeat run idempotent.
	if commandSucceeds(t.bin, "mcp", "get", "burrow") {
		fmt.Fprintf(w, "Burrow is already configured in %s. Nothing to do.\n", t.display)
		return nil
	}
	if err := runCommand(t.bin, t.addArgs...); err != nil {
		return fmt.Errorf("running %s: %w", t.addCommand(), err)
	}
	fmt.Fprintf(w, "Added Burrow to %s. Restart %s to pick it up.\n", t.display, t.display)
	fmt.Fprint(w, mcpTryPrompt)
	return nil
}

// --- cursor: ~/.cursor/mcp.json (no CLI, edited directly) ---

type cursorTool struct{}

func (cursorTool) preview() string {
	if cursorConfigured() {
		return fmt.Sprintf("Burrow is already configured in Cursor (%s). Nothing to do.\n", cursorConfigDisplay)
	}
	return "Connect Burrow to Cursor.\n\n" +
		"This will add to " + cursorConfigDisplay + ":\n" +
		"  {\n" +
		"    \"mcpServers\": {\n" +
		"      \"burrow\": {\n" +
		"        \"command\": \"burrow-mcp\"\n" +
		"      }\n" +
		"    }\n" +
		"  }\n\n" +
		"Your other MCP servers are preserved, and the file is backed up first.\n\n" +
		"Run `burrow mcp cursor install` to apply.\n"
}

// cursorConfigured reports whether ~/.cursor/mcp.json already lists a `burrow` server.
func cursorConfigured() bool {
	path, err := cursorConfigPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	root := map[string]any{}
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	servers, _ := root["mcpServers"].(map[string]any)
	_, ok := servers["burrow"]
	return ok
}

func (cursorTool) install(w io.Writer) error {
	path, err := cursorConfigPath()
	if err != nil {
		return err
	}
	root := map[string]any{}
	existed := false
	if data, err := os.ReadFile(path); err == nil {
		existed = true
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return fmt.Errorf("parsing %s: %w", cursorConfigDisplay, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", cursorConfigDisplay, err)
	}

	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, ok := servers["burrow"]; ok {
		fmt.Fprintf(w, "Burrow is already configured in Cursor (%s). Nothing to do.\n", cursorConfigDisplay)
		return nil
	}
	servers["burrow"] = map[string]any{"command": "burrow-mcp"}
	root["mcpServers"] = servers

	if existed {
		if err := backupFile(path); err != nil {
			return err
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", cursorConfigDisplay, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", cursorConfigDisplay, err)
	}
	fmt.Fprintf(w, "Added Burrow to Cursor (%s). Restart Cursor to pick it up.\n", cursorConfigDisplay)
	fmt.Fprint(w, mcpTryPrompt)
	return nil
}

// backupFile copies path to path+".bak" before an edit, so a mistaken merge is always recoverable.
func backupFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := os.WriteFile(path+".bak", data, 0o644); err != nil {
		return fmt.Errorf("writing backup %s: %w", path+".bak", err)
	}
	return nil
}
