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

// `burrow mcp` connected a coding agent to Burrow's `burrow-mcp` stdio server. It is DEPRECATED
// (ADR-0049): the agent's control channel is now the scoped `burrow-agent` binary, wired with
// `burrow agent <tool> install`, and `burrow-mcp` is no longer shipped in releases. The command is
// kept in-tree, hidden from help, and non-breaking — it still previews by default and mutates only
// on `install` — but it steers the user to `burrow agent` and should not be relied on.

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

// opencodeConfigPath resolves OpenCode's config file. OpenCode's `mcp add` only wires remote
// (URL) servers, so a local stdio server like burrow-mcp is configured through the file; the path is
// a package var so a test can point it at a temp dir.
var opencodeConfigPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode", "opencode.json"), nil
}

// claudeSettingsPath resolves Claude Code's user settings file, where connecting Claude Code also
// writes the deny rule that stops the agent running the `burrow` CLI directly (see the harden step).
// It is a package var so a test can point it at a temp dir and exercise the real create/merge/backup
// logic without touching the real home directory.
var claudeSettingsPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// The tilde forms shown in help and messages, independent of where the seams actually resolve (a
// temp dir in tests), so the user always sees the familiar path.
const (
	cursorConfigDisplay   = "~/.cursor/mcp.json"
	opencodeConfigDisplay = "~/.config/opencode/opencode.json"
	claudeSettingsDisplay = "~/.claude/settings.json"
)

// claudeDenyRule is the exact Claude Code permission pattern that blocks the agent from running the
// burrow CLI in its shell. The space before `*` is a word boundary: it matches `burrow guard set …`
// but not a differently named tool like `burrowctl`. docs/HARDENING.md documents the same string.
const claudeDenyRule = "Bash(burrow *)"

// claudeDenyKubectlRule blocks the agent from running kubectl directly, so every cluster change goes
// through Burrow's guarded path rather than around it. It is opt-in (--deny-kubectl): kubectl is a
// general tool Burrow does not own, so denying it by default would be overreach.
const claudeDenyKubectlRule = "Bash(kubectl *)"

// claudeHardenNote is printed after an install that added only the burrow-CLI deny rule, pointing the
// user at the fuller lockdown (kubectl/helm) they may also want. No em-dashes: it is user-facing output.
const claudeHardenNote = "Blocked the agent from running the burrow CLI directly (added Bash(burrow *) to " +
	"~/.claude/settings.json). To also stop it bypassing Burrow with kubectl or helm, see docs/HARDENING.md.\n"

// claudeHardenKubectlNote replaces claudeHardenNote when --deny-kubectl was passed: it records that
// both the burrow-CLI and the kubectl deny rules were added, keeping every cluster change on Burrow's
// guarded, audited path. No em-dashes: it is user-facing output.
const claudeHardenKubectlNote = "Blocked the agent from running the burrow CLI and kubectl directly (added " +
	"Bash(burrow *) and Bash(kubectl *) to ~/.claude/settings.json), so every cluster change flows through " +
	"Burrow's guardrails and audit log.\n"

// claudeDenyKubectlRecommendation is printed after the (burrow-only) harden note when --deny-kubectl
// was NOT passed: it nudges the user toward the fuller lockdown without forcing it. No em-dashes.
const claudeDenyKubectlRecommendation = "Recommended: keep every cluster change flowing through Burrow's " +
	"guardrails and audit log by also blocking the agent from running kubectl directly. Enable it with:\n" +
	"  burrow mcp claude install --deny-kubectl\n"

// claudeDenyKubectlIgnoredNote is printed when both --no-harden and --deny-kubectl were passed:
// --no-harden wins (Burrow manages no permissions), so --deny-kubectl has nothing to act on. No em-dashes.
const claudeDenyKubectlIgnoredNote = "Note: --deny-kubectl was ignored because hardening is off " +
	"(--no-harden means you manage permissions yourself).\n"

// mcpTryPrompt is appended after any successful install, giving the user a concrete first thing to
// ask their agent. It leads with a blank line so it sits below the success line. No em-dashes.
const mcpTryPrompt = "\nThen open your agent and try:\n" +
	"  \"Deploy ghcr.io/me/app:1.4 and serve it at example.com over HTTPS.\"\n"

// mcpOverview is what bare `burrow mcp` prints: what it does, the supported tools, and how to
// preview then apply. No em-dashes: it is user-facing CLI output.
const mcpOverview = "Deprecated: use `burrow agent <tool> install` instead.\n\n" +
	"The agent's control channel is now the scoped `burrow-agent` binary, not the `burrow-mcp`\n" +
	"server, which is no longer shipped in releases. Wire your agent with:\n" +
	"  burrow agent <tool> install\n\n" +
	"This deprecated command configures the retired MCP server. Supported tools:\n" +
	"  claude    Claude Code\n" +
	"  cursor    Cursor\n" +
	"  codex     Codex\n" +
	"  copilot   Copilot\n" +
	"  opencode  OpenCode\n\n" +
	"Preview what would be added:\n" +
	"  burrow mcp <tool>\n\n" +
	"Apply it (deprecated):\n" +
	"  burrow mcp <tool> install\n\n" +
	"Using another agent? Request support: " + mcpIssuesURL + "\n"

// mcpIssuesURL is where a user whose agent has no built-in setup can request first-class support.
const mcpIssuesURL = "https://github.com/burrow-cloud/burrow/issues/new"

// mcpUnknownToolMessage is printed for `burrow mcp <tool>` when the tool has no built-in adapter:
// rather than error, it points the user at burrow-mcp (any MCP-capable tool can use it) and invites
// a support request. The %q is the tool the user named. No em-dashes: it is user-facing output.
const mcpUnknownToolMessage = "Burrow has no built-in setup for %q yet.\n\n" +
	"Burrow's MCP server is `burrow-mcp` (a stdio server, no arguments) that any MCP-capable tool " +
	"can use; add it to that tool's MCP config.\n\n" +
	"Built-in setup: " + mcpBuiltinTools + ".\n" +
	"Request support: " + mcpIssuesURL + "\n"

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
	"claude":   claudeTool{cli: cliTool{key: "claude", bin: "claude", display: "Claude Code", addArgs: []string{"mcp", "add", "--scope", "user", "burrow", "--", "burrow-mcp"}}, harden: true},
	"cursor":   cursorTool{},
	"codex":    cliTool{key: "codex", bin: "codex", display: "Codex", addArgs: []string{"mcp", "add", "burrow", "--", "burrow-mcp"}},
	"copilot":  cliTool{key: "copilot", bin: "copilot", display: "Copilot", addArgs: []string{"mcp", "add", "burrow", "--", "burrow-mcp"}},
	"opencode": opencodeTool{},
	"aider":    aiderTool{},
}

// mcpBuiltinTools lists the tools Burrow can set up first-class (aider is recognized but has no MCP
// support, so it is not "setup"). It feeds the help text and the unknown-tool message.
const mcpBuiltinTools = "claude, cursor, codex, copilot, opencode"

func newMcpCmd() *cobra.Command {
	var noHarden bool
	var denyKubectl bool
	cmd := &cobra.Command{
		Use:   "mcp [tool] [install]",
		Short: "Deprecated: use `burrow agent <tool> install` instead",
		Long: "Deprecated: connect your AI agent with `burrow agent <tool> install` instead.\n\n" +
			"The agent's control channel is now the scoped `burrow-agent` binary, not the `burrow-mcp`\n" +
			"server, which is no longer shipped in releases. This command still previews by default and\n" +
			"applies on `install`, backing up any file it edits, but it configures the retired MCP server\n" +
			"and should not be relied on. Supported tools: " + mcpBuiltinTools + ".",
		Example: "  # Preferred: wire Claude Code to burrow-agent\n" +
			"  burrow agent claude install\n\n" +
			"  # Deprecated MCP path (kept for now)\n" +
			"  burrow mcp claude install",
		// Cobra prints this to stderr on every use, steering the user to the replacement. Hidden keeps
		// it out of the main help listing, retiring MCP from the recommended path.
		Deprecated: "use `burrow agent <tool> install` instead; burrow-mcp is no longer shipped.",
		Hidden:     true,
		Args:       cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMcp(args, cmd.OutOrStdout(), noHarden, denyKubectl)
		},
	}
	// Connecting Claude Code also denies the burrow CLI in its shell so the agent cannot bypass the
	// control-plane guardrails; --no-harden opts out for users who manage their own permissions. The
	// flag is a no-op for every other tool. No em-dashes in the help string: it is user-facing.
	cmd.Flags().BoolVar(&noHarden, "no-harden", false,
		"For Claude Code, skip adding the burrow CLI deny rule to ~/.claude/settings.json (manage permissions yourself)")
	// --deny-kubectl additionally denies kubectl in the agent's shell so every cluster change goes
	// through Burrow's guarded, audited path. Opt-in: kubectl is a general tool Burrow does not own, so
	// blocking it by default would be overreach. --no-harden wins over it (it skips all Burrow denies).
	cmd.Flags().BoolVar(&denyKubectl, "deny-kubectl", false,
		"For Claude Code, also add the kubectl deny rule to ~/.claude/settings.json (keep every cluster change on Burrow's guarded path)")
	return cmd
}

// runMcp routes the positional args: none prints the overview, one previews a tool, and two applies
// it when the second arg is the literal `install`. It mutates nothing except on the two-arg install
// path.
func runMcp(args []string, w io.Writer, noHarden, denyKubectl bool) error {
	if len(args) == 0 {
		fmt.Fprint(w, mcpOverview)
		return nil
	}
	tool, ok := mcpTools[args[0]]
	if !ok {
		// An agent Burrow has no adapter for is not an error: burrow-mcp is a plain stdio server any
		// MCP-capable tool can use, so point the user at it and invite a support request.
		fmt.Fprintf(w, mcpUnknownToolMessage, args[0])
		return nil
	}
	// Claude's install can also harden Claude Code's settings (the burrow-CLI deny rule, plus kubectl
	// when --deny-kubectl is set); --no-harden turns all of it off. These flags are no-ops for every
	// other tool, so they only touch the claude adapter.
	if ct, ok := tool.(claudeTool); ok {
		ct.harden = !noHarden
		ct.denyKubectl = denyKubectl
		tool = ct
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
		"burrow-mcp is a stdio MCP server that uses the scoped, burrowd-only credential Burrow mints at install and your active environment, so no extra config is needed and the agent reaches only the control plane.\n\n"+
		"Run `burrow mcp %s install` to apply.\n", t.display, t.addCommand(), t.key)
}

// addOutcome is the result of the idempotent mcp-add step, so a composing adapter (claude) can add
// its own steps and messaging around it rather than duplicating the CLI dance.
type addOutcome int

const (
	addMissingCLI addOutcome = iota // the agent CLI is not on PATH (a manual hint was written)
	addAlready                      // Burrow was already configured; nothing ran
	addDone                         // the add command ran and succeeded
)

// ensureAdded performs the idempotent `mcp add` step and reports what happened, writing only the
// not-on-PATH hint itself (the caller owns the success/already messaging so it can compose extra
// steps). `mcp add` errors if a server named burrow already exists, so it pre-checks and no-ops.
func (t cliTool) ensureAdded(w io.Writer) (addOutcome, error) {
	if _, err := mcpLookPath(t.bin); err != nil {
		fmt.Fprintf(w, "%s CLI (%s) not found on PATH. Install it, or run this yourself:\n  %s\n", t.display, t.bin, t.addCommand())
		return addMissingCLI, nil
	}
	if commandSucceeds(t.bin, "mcp", "get", "burrow") {
		return addAlready, nil
	}
	if err := runCommand(t.bin, t.addArgs...); err != nil {
		return addDone, fmt.Errorf("running %s: %w", t.addCommand(), err)
	}
	return addDone, nil
}

func (t cliTool) install(w io.Writer) error {
	outcome, err := t.ensureAdded(w)
	if err != nil {
		return err
	}
	switch outcome {
	case addAlready:
		fmt.Fprintf(w, "Burrow is already configured in %s. Nothing to do.\n", t.display)
	case addDone:
		fmt.Fprintf(w, "Added Burrow to %s. Restart %s to pick it up.\n", t.display, t.display)
		fmt.Fprint(w, mcpTryPrompt)
	}
	return nil
}

// --- claude: cliTool mcp-add plus the burrow-CLI deny rule in ~/.claude/settings.json ---

// claudeTool connects Claude Code and, by default, hardens it: on top of the generic `mcp add` (via
// the composed cliTool), it merges the `Bash(burrow *)` deny rule into the user settings so the agent
// cannot run the burrow CLI directly and shell around the control-plane guardrails (e.g. `burrow guard
// set`). The harden step is idempotent and preserves every other setting; `--no-harden` turns it off.
type claudeTool struct {
	cli         cliTool // the generic mcp-add adapter, reused rather than duplicated
	harden      bool    // whether install also writes the deny rules (default true; --no-harden turns off)
	denyKubectl bool    // whether hardening also denies kubectl (opt-in via --deny-kubectl)
}

// hardenRules is the set of deny rules the harden step writes: always the burrow-CLI rule, plus the
// kubectl rule when --deny-kubectl was passed. Order is stable so appended rules read predictably.
func (t claudeTool) hardenRules() []string {
	rules := []string{claudeDenyRule}
	if t.denyKubectl {
		rules = append(rules, claudeDenyKubectlRule)
	}
	return rules
}

func (t claudeTool) preview() string {
	mcpDone := t.cli.configured()
	hardenDone := claudeHardened(t.hardenRules())
	// Both parts settled (harden off counts the harden part as settled): nothing to do.
	if mcpDone && (!t.harden || hardenDone) {
		return fmt.Sprintf("Burrow is already configured in %s. Nothing to do.\n", t.cli.display)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Connect Burrow to %s.\n\n", t.cli.display)

	if mcpDone {
		b.WriteString("The MCP server is already configured.\n\n")
	} else {
		fmt.Fprintf(&b, "This will run:\n  %s\n\n", t.cli.addCommand())
	}

	if t.harden {
		if hardenDone {
			if t.denyKubectl {
				fmt.Fprintf(&b, "The %s and %s deny rules are already in %s.\n\n", claudeDenyRule, claudeDenyKubectlRule, claudeSettingsDisplay)
			} else {
				fmt.Fprintf(&b, "The %s deny rule is already in %s.\n\n", claudeDenyRule, claudeSettingsDisplay)
			}
		} else if t.denyKubectl {
			fmt.Fprintf(&b, "It will also add %s and %s to %s deny rules, so the agent cannot run the\n"+
				"burrow CLI or kubectl directly (either would let it bypass the control-plane guardrails), keeping\n"+
				"every cluster change on Burrow's guarded, audited path. Pass --no-harden to skip this and manage\n"+
				"permissions yourself.\n\n", claudeDenyRule, claudeDenyKubectlRule, claudeSettingsDisplay)
		} else {
			fmt.Fprintf(&b, "It will also add %s to %s deny rules, so the agent cannot run the burrow CLI\n"+
				"directly (which would let it bypass the control-plane guardrails). Pass --no-harden to skip\n"+
				"this and manage permissions yourself.\n\n"+
				"Recommended: also pass --deny-kubectl to block the agent from running kubectl directly, so every\n"+
				"cluster change flows through Burrow's guardrails and audit log.\n\n", claudeDenyRule, claudeSettingsDisplay)
		}
	} else {
		fmt.Fprintf(&b, "Hardening is off (--no-harden), so the %s deny rule will not be added.\n\n", claudeDenyRule)
		if t.denyKubectl {
			b.WriteString("--deny-kubectl is ignored because hardening is off.\n\n")
		}
	}

	b.WriteString("burrow-mcp is a stdio MCP server that uses the scoped, burrowd-only credential Burrow mints at install and your active environment, so no extra config is needed and the agent reaches only the control plane.\n\n")
	fmt.Fprintf(&b, "Run `burrow mcp %s install` to apply.\n", t.cli.key)
	return b.String()
}

func (t claudeTool) install(w io.Writer) error {
	outcome, err := t.cli.ensureAdded(w)
	if err != nil {
		return err
	}

	// --no-harden wins over --deny-kubectl: it skips every Burrow-managed deny, so there is nothing for
	// --deny-kubectl to act on. Say so rather than silently dropping the flag.
	if !t.harden && t.denyKubectl {
		fmt.Fprint(w, claudeDenyKubectlIgnoredNote)
	}

	hardenChanged := false
	if t.harden {
		if hardenChanged, err = ensureClaudeDenyRules(t.hardenRules()); err != nil {
			return err
		}
	}

	// Nothing to do: the server was already present and the harden part is settled (off or already
	// there). The not-on-PATH case is not "nothing to do" (ensureAdded wrote a manual hint).
	if outcome == addAlready && !hardenChanged {
		fmt.Fprintf(w, "Burrow is already configured in %s. Nothing to do.\n", t.cli.display)
		return nil
	}

	if outcome == addDone {
		fmt.Fprintf(w, "Added Burrow to %s. Restart %s to pick it up.\n", t.cli.display, t.cli.display)
	}
	if hardenChanged {
		if t.denyKubectl {
			fmt.Fprint(w, claudeHardenKubectlNote)
		} else {
			// Denied only the burrow CLI: nudge the user toward the fuller lockdown they did not opt into.
			fmt.Fprint(w, claudeHardenNote)
			fmt.Fprint(w, claudeDenyKubectlRecommendation)
		}
	}
	if outcome == addDone || hardenChanged {
		fmt.Fprint(w, mcpTryPrompt)
	}
	return nil
}

// claudeHardened reports whether ~/.claude/settings.json already denies every rule in rules.
func claudeHardened(rules []string) bool {
	path, err := claudeSettingsPath()
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
	return claudeDenyPresent(root, rules)
}

// claudeDenySet reads permissions.deny into a set of the rule strings present, so membership tests do
// not rescan the slice.
func claudeDenySet(root map[string]any) map[string]bool {
	perms, _ := root["permissions"].(map[string]any)
	deny, _ := perms["deny"].([]any)
	set := make(map[string]bool, len(deny))
	for _, r := range deny {
		if s, ok := r.(string); ok {
			set[s] = true
		}
	}
	return set
}

// claudeDenyPresent reports whether every rule in rules is already listed under permissions.deny.
func claudeDenyPresent(root map[string]any, rules []string) bool {
	have := claudeDenySet(root)
	for _, rule := range rules {
		if !have[rule] {
			return false
		}
	}
	return true
}

// ensureClaudeDenyRules merges each rule into ~/.claude/settings.json's permissions.deny, preserving
// every other key (other permissions, allow rules, unrelated settings) and backing up an existing
// file once before it writes. It adds only the rules not already present (so no rule is duplicated)
// and reports whether it changed anything, returning false (and writing nothing) when every rule is
// already there, so a repeat install is idempotent.
func ensureClaudeDenyRules(rules []string) (bool, error) {
	path, err := claudeSettingsPath()
	if err != nil {
		return false, err
	}
	root := map[string]any{}
	existed := false
	if data, err := os.ReadFile(path); err == nil {
		existed = true
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return false, fmt.Errorf("parsing %s: %w", claudeSettingsDisplay, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("reading %s: %w", claudeSettingsDisplay, err)
	}

	have := claudeDenySet(root)
	var toAdd []string
	for _, rule := range rules {
		if !have[rule] {
			toAdd = append(toAdd, rule)
		}
	}
	if len(toAdd) == 0 {
		return false, nil
	}

	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	deny, _ := perms["deny"].([]any)
	for _, rule := range toAdd {
		deny = append(deny, rule)
	}
	perms["deny"] = deny
	root["permissions"] = perms

	if existed {
		if err := backupFile(path); err != nil {
			return false, err
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encoding %s: %w", claudeSettingsDisplay, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", claudeSettingsDisplay, err)
	}
	return true, nil
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

// --- opencode: ~/.config/opencode/opencode.json (mcp add wires only remote servers) ---

type opencodeTool struct{}

func (opencodeTool) preview() string {
	if opencodeConfigured() {
		return fmt.Sprintf("Burrow is already configured in OpenCode (%s). Nothing to do.\n", opencodeConfigDisplay)
	}
	return "Connect Burrow to OpenCode.\n\n" +
		"This will add to " + opencodeConfigDisplay + ":\n" +
		"  {\n" +
		"    \"mcp\": {\n" +
		"      \"burrow\": {\n" +
		"        \"type\": \"local\",\n" +
		"        \"command\": [\"burrow-mcp\"],\n" +
		"        \"enabled\": true\n" +
		"      }\n" +
		"    }\n" +
		"  }\n\n" +
		"Your other MCP servers are preserved, and the file is backed up first.\n\n" +
		"Run `burrow mcp opencode install` to apply.\n"
}

// opencodeConfigured reports whether opencode.json already lists a `burrow` server under `mcp`.
func opencodeConfigured() bool {
	path, err := opencodeConfigPath()
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
	servers, _ := root["mcp"].(map[string]any)
	_, ok := servers["burrow"]
	return ok
}

func (opencodeTool) install(w io.Writer) error {
	path, err := opencodeConfigPath()
	if err != nil {
		return err
	}
	root := map[string]any{}
	existed := false
	if data, err := os.ReadFile(path); err == nil {
		existed = true
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return fmt.Errorf("parsing %s: %w", opencodeConfigDisplay, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", opencodeConfigDisplay, err)
	}

	servers, _ := root["mcp"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, ok := servers["burrow"]; ok {
		fmt.Fprintf(w, "Burrow is already configured in OpenCode (%s). Nothing to do.\n", opencodeConfigDisplay)
		return nil
	}
	servers["burrow"] = map[string]any{
		"type":    "local",
		"command": []string{"burrow-mcp"},
		"enabled": true,
	}
	root["mcp"] = servers
	// A fresh config carries OpenCode's schema pointer so the editor validates it; an existing file
	// keeps whatever it already declares.
	if _, ok := root["$schema"]; !ok {
		root["$schema"] = "https://opencode.ai/config.json"
	}

	if existed {
		if err := backupFile(path); err != nil {
			return err
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", opencodeConfigDisplay, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", opencodeConfigDisplay, err)
	}
	fmt.Fprintf(w, "Added Burrow to OpenCode (%s). Restart OpenCode to pick it up.\n", opencodeConfigDisplay)
	fmt.Fprint(w, mcpTryPrompt)
	return nil
}

// --- aider: no MCP support ---

// aiderMessage is printed for both preview and install: Aider has no MCP support, so there is
// nothing to configure. It still names burrow-mcp for anyone running an MCP bridge. No em-dashes.
const aiderMessage = "Aider does not support MCP servers, so there is nothing to install here. " +
	"Burrow's MCP server is `burrow-mcp` (a stdio server); if you run an MCP bridge for Aider, " +
	"point it at that.\n"

type aiderTool struct{}

func (aiderTool) preview() string { return aiderMessage }

func (aiderTool) install(w io.Writer) error {
	fmt.Fprint(w, aiderMessage)
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
