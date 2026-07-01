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
// a test can fake the claude-CLI invocation without a real `claude` on PATH.
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

// cursorConfigPath and codexConfigPath resolve the per-tool config files. They are package vars so a
// test can point them at a temp dir and exercise the real create/merge/backup logic safely.
var cursorConfigPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}

var codexConfigPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// The tilde forms shown in help and messages, independent of where the seams actually resolve (a
// temp dir in tests), so the user always sees the familiar path.
const (
	cursorConfigDisplay = "~/.cursor/mcp.json"
	codexConfigDisplay  = "~/.codex/config.toml"
)

// mcpTryPrompt is appended after any successful install, giving the user a concrete first thing to
// ask their agent. It leads with a blank line so it sits below the success line. No em-dashes.
const mcpTryPrompt = "\nThen open your agent and try:\n" +
	"  \"Deploy ghcr.io/me/app:1.4 and serve it at example.com over HTTPS.\"\n"

// mcpOverview is what bare `burrow mcp` prints: what it does, the supported tools, and how to
// preview then apply. No em-dashes: it is user-facing CLI output.
const mcpOverview = "Connect Burrow to your AI agent so it can operate your cluster.\n\n" +
	"Supported tools:\n" +
	"  claude   Claude Code\n" +
	"  cursor   Cursor\n" +
	"  codex    Codex\n\n" +
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
	"claude": claudeTool{},
	"cursor": cursorTool{},
	"codex":  codexTool{},
}

const mcpValidTools = "claude, cursor, codex"

func newMcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp [tool] [install]",
		Short: "Connect Burrow to your AI agent",
		Long: "Connect Burrow to your AI agent so it can operate your cluster.\n\n" +
			"Preview what a tool needs with `burrow mcp <tool>`, then apply it with\n" +
			"`burrow mcp <tool> install`. The file-based tools are backed up before any edit and\n" +
			"the change is idempotent, so a second run is safe. Supported tools: " + mcpValidTools + ".",
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

// --- claude: the Claude Code CLI ---

type claudeTool struct{}

const claudeAlreadyConfigured = "Burrow is already configured in Claude Code. Nothing to do.\n"

// claudeConfigured reports whether the claude CLI already has a `burrow` MCP server. It is only
// meaningful when `claude` is on PATH; callers gate on LookPath first.
func claudeConfigured() bool {
	return commandSucceeds("claude", "mcp", "get", "burrow")
}

func (claudeTool) preview() string {
	if _, err := mcpLookPath("claude"); err == nil && claudeConfigured() {
		return claudeAlreadyConfigured
	}
	return "Connect Burrow to Claude Code.\n\n" +
		"This will run:\n" +
		"  claude mcp add --scope user burrow -- burrow-mcp\n\n" +
		"burrow-mcp is a stdio MCP server that uses your kubeconfig and active environment, so no extra config is needed.\n\n" +
		"Run `burrow mcp claude install` to apply.\n"
}

func (claudeTool) install(w io.Writer) error {
	if _, err := mcpLookPath("claude"); err != nil {
		fmt.Fprint(w, "Claude Code CLI (claude) not found on PATH. Install it, or run this yourself:\n"+
			"  claude mcp add --scope user burrow -- burrow-mcp\n")
		return nil
	}
	// `claude mcp add` errors if a server named burrow already exists, so pre-check and no-op
	// instead, matching the idempotent behavior of the file-based tools.
	if claudeConfigured() {
		fmt.Fprint(w, claudeAlreadyConfigured)
		return nil
	}
	if err := runCommand("claude", "mcp", "add", "--scope", "user", "burrow", "--", "burrow-mcp"); err != nil {
		return fmt.Errorf("running claude mcp add: %w", err)
	}
	fmt.Fprint(w, "Added Burrow to Claude Code. Restart Claude Code (or run /mcp) to pick it up.\n")
	fmt.Fprint(w, mcpTryPrompt)
	return nil
}

// --- cursor: ~/.cursor/mcp.json ---

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

// --- codex: ~/.codex/config.toml ---

type codexTool struct{}

// codexBlock is the TOML table appended for Burrow. A plain text append keeps the dependency graph
// free of a TOML parser; the append is guarded by an idempotent header check.
const codexBlock = "[mcp_servers.burrow]\ncommand = \"burrow-mcp\"\n"

const codexHeader = "[mcp_servers.burrow]"

func (codexTool) preview() string {
	if codexConfigured() {
		return fmt.Sprintf("Burrow is already configured in Codex (%s). Nothing to do.\n", codexConfigDisplay)
	}
	return "Connect Burrow to Codex.\n\n" +
		"This will add to " + codexConfigDisplay + ":\n" +
		"  [mcp_servers.burrow]\n" +
		"  command = \"burrow-mcp\"\n\n" +
		"Your other MCP servers are preserved, and the file is backed up first.\n\n" +
		"Run `burrow mcp codex install` to apply.\n"
}

// codexConfigured reports whether ~/.codex/config.toml already declares the burrow MCP table.
func codexConfigured() bool {
	path, err := codexConfigPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), codexHeader)
}

func (codexTool) install(w io.Writer) error {
	path, err := codexConfigPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", codexConfigDisplay, err)
	}
	if strings.Contains(string(data), codexHeader) {
		fmt.Fprintf(w, "Burrow is already configured in Codex (%s). Nothing to do.\n", codexConfigDisplay)
		return nil
	}

	if existed {
		if err := backupFile(path); err != nil {
			return err
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	var b strings.Builder
	b.Write(data)
	// Separate the appended table from existing content with a blank line, unless the file is
	// empty or already ends in one.
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n\n") {
		if strings.HasSuffix(string(data), "\n") {
			b.WriteString("\n")
		} else {
			b.WriteString("\n\n")
		}
	}
	b.WriteString(codexBlock)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", codexConfigDisplay, err)
	}
	fmt.Fprintf(w, "Added Burrow to Codex (%s). Restart Codex to pick it up.\n", codexConfigDisplay)
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
