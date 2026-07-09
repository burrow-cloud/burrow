// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpTempCursorConfig points the cursor config seam at a fresh temp file and returns its path.
// Nothing exists at the path until a test writes it, so preview must not create it.
func mcpTempCursorConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cursor", "mcp.json")
	orig := cursorConfigPath
	cursorConfigPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { cursorConfigPath = orig })
	return path
}

// mcpTempOpencodeConfig points the opencode config seam at a fresh temp file and returns its path.
func mcpTempOpencodeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode", "opencode.json")
	orig := opencodeConfigPath
	opencodeConfigPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { opencodeConfigPath = orig })
	return path
}

// mcpTempClaudeSettings points the claude settings seam at a fresh temp file and returns its path.
// Nothing exists at the path until a test writes it, so a preview and a not-yet-hardened install can
// exercise the create branch without touching the real ~/.claude/settings.json.
func mcpTempClaudeSettings(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude", "settings.json")
	orig := claudeSettingsPath
	claudeSettingsPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { claudeSettingsPath = orig })
	return path
}

// fakeCLI stubs the CLI-backed adapter seams (claude/codex/copilot): whether the binary is on PATH,
// whether it reports burrow already configured, and it records every runCommand invocation. It
// restores the seams on cleanup. The fake is name-agnostic, so a test exercises one tool at a time.
func fakeCLI(t *testing.T, onPath, configured bool) *[][]string {
	t.Helper()
	var calls [][]string

	origLook, origRun, origSucceeds := mcpLookPath, runCommand, commandSucceeds
	mcpLookPath = func(string) (string, error) {
		if onPath {
			return "/usr/local/bin/agent", nil
		}
		return "", os.ErrNotExist
	}
	commandSucceeds = func(string, ...string) bool { return configured }
	runCommand = func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	t.Cleanup(func() {
		mcpLookPath = origLook
		runCommand = origRun
		commandSucceeds = origSucceeds
	})
	return &calls
}

func TestMcpOverviewMutatesNothing(t *testing.T) {
	cursor := mcpTempCursorConfig(t)
	opencode := mcpTempOpencodeConfig(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp"}, &out, &out); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	for _, want := range []string{
		// The command is deprecated (ADR-0049): it steers the user to `burrow agent` first.
		"Deprecated: use `burrow agent <tool> install` instead.",
		"burrow agent <tool> install",
		"claude    Claude Code", "cursor    Cursor", "codex     Codex", "copilot   Copilot",
		"opencode  OpenCode",
		"burrow mcp <tool>", "burrow mcp <tool> install",
		// The contribution pointer sits at the bottom.
		"Request support: https://github.com/burrow-cloud/burrow/issues/new",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("overview missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, cursor)
	assertNoFile(t, opencode)
}

// TestMcpUnknownToolFallback confirms an agent with no built-in adapter does not error but prints
// the burrow-mcp pointer, the built-in list, and the support link, mutating nothing.
func TestMcpUnknownToolFallback(t *testing.T) {
	cursor := mcpTempCursorConfig(t)
	opencode := mcpTempOpencodeConfig(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "somethingelse"}, &out, &out); err != nil {
		t.Fatalf("mcp somethingelse: %v", err)
	}
	for _, want := range []string{
		`Burrow has no built-in setup for "somethingelse" yet.`,
		"Burrow's MCP server is `burrow-mcp` (a stdio server, no arguments)",
		"Built-in setup: claude, cursor, codex, copilot, opencode.",
		"Request support: https://github.com/burrow-cloud/burrow/issues/new",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("unknown-tool fallback missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, cursor)
	assertNoFile(t, opencode)
}

// TestMcpAiderUnsupported confirms aider prints the no-MCP-support message for both preview and
// install, with no file or exec side effects.
func TestMcpAiderUnsupported(t *testing.T) {
	calls := fakeCLI(t, true, false) // exec seams present; aider must not touch them
	for _, args := range [][]string{{"mcp", "aider"}, {"mcp", "aider", "install"}} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "Aider does not support MCP servers") {
			t.Errorf("%v missing the unsupported message:\n%s", args, out.String())
		}
		if !strings.Contains(out.String(), "burrow-mcp") {
			t.Errorf("%v should still name burrow-mcp:\n%s", args, out.String())
		}
	}
	if len(*calls) != 0 {
		t.Errorf("aider should run no command, got %v", *calls)
	}
}

func TestMcpBadSecondArg(t *testing.T) {
	fakeCLI(t, true, false)
	var out bytes.Buffer
	err := run(context.Background(), []string{"mcp", "claude", "bogus"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown argument") {
		t.Fatalf("err = %v, want an unknown-argument error", err)
	}
}

// --- CLI-backed tools: claude, codex, copilot share the generic adapter ---

// cliToolCase describes a CLI-backed tool's expected surface so the shared table exercises the
// generic adapter identically for claude, codex, and copilot.
type cliToolCase struct {
	arg        string // the `burrow mcp <arg>` argument
	display    string // human name in messages
	addCommand string // the exact rendered add command line
}

// claude is intentionally absent: it has its own adapter (mcp-add plus the harden step) with
// divergent messaging, exercised by the dedicated TestMcpClaude* tests below. codex and copilot
// still share the generic cliTool, so the table keeps proving that path is untouched.
func cliToolCases() []cliToolCase {
	return []cliToolCase{
		{arg: "codex", display: "Codex", addCommand: "codex mcp add burrow -- burrow-mcp"},
		{arg: "copilot", display: "Copilot", addCommand: "copilot mcp add burrow -- burrow-mcp"},
	}
}

func TestMcpCliPreviewMutatesNothing(t *testing.T) {
	for _, tc := range cliToolCases() {
		t.Run(tc.arg, func(t *testing.T) {
			calls := fakeCLI(t, true, false) // on PATH, not yet configured
			var out bytes.Buffer
			if err := run(context.Background(), []string{"mcp", tc.arg}, &out, &out); err != nil {
				t.Fatalf("mcp %s: %v", tc.arg, err)
			}
			for _, want := range []string{
				"Connect Burrow to " + tc.display + ".",
				tc.addCommand,
				"Run `burrow mcp " + tc.arg + " install` to apply.",
			} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("%s preview missing %q:\n%s", tc.arg, want, out.String())
				}
			}
			if len(*calls) != 0 {
				t.Errorf("preview ran a command: %v", *calls)
			}
		})
	}
}

func TestMcpCliInstallInvokesAdd(t *testing.T) {
	for _, tc := range cliToolCases() {
		t.Run(tc.arg, func(t *testing.T) {
			calls := fakeCLI(t, true, false) // on PATH, not configured -> add runs
			var out bytes.Buffer
			if err := run(context.Background(), []string{"mcp", tc.arg, "install"}, &out, &out); err != nil {
				t.Fatalf("mcp %s install: %v", tc.arg, err)
			}
			if len(*calls) != 1 {
				t.Fatalf("ran %d commands, want exactly the add: %v", len(*calls), *calls)
			}
			if got := strings.Join((*calls)[0], " "); got != tc.addCommand {
				t.Errorf("add invocation = %q, want %q", got, tc.addCommand)
			}
			if !strings.Contains(out.String(), "Added Burrow to "+tc.display+".") {
				t.Errorf("missing success line:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "Then open your agent and try:") {
				t.Errorf("missing the try-prompt:\n%s", out.String())
			}
		})
	}
}

func TestMcpCliInstallAlreadyConfigured(t *testing.T) {
	for _, tc := range cliToolCases() {
		t.Run(tc.arg, func(t *testing.T) {
			calls := fakeCLI(t, true, true) // on PATH and already configured -> no add
			var out bytes.Buffer
			if err := run(context.Background(), []string{"mcp", tc.arg, "install"}, &out, &out); err != nil {
				t.Fatalf("mcp %s install: %v", tc.arg, err)
			}
			if len(*calls) != 0 {
				t.Errorf("add should not run when already configured, got %v", *calls)
			}
			if !strings.Contains(out.String(), "already configured in "+tc.display) {
				t.Errorf("missing already-configured message:\n%s", out.String())
			}
		})
	}
}

func TestMcpCliPreviewAlreadyConfigured(t *testing.T) {
	for _, tc := range cliToolCases() {
		t.Run(tc.arg, func(t *testing.T) {
			fakeCLI(t, true, true)
			var out bytes.Buffer
			if err := run(context.Background(), []string{"mcp", tc.arg}, &out, &out); err != nil {
				t.Fatalf("mcp %s: %v", tc.arg, err)
			}
			if !strings.Contains(out.String(), "already configured in "+tc.display) {
				t.Errorf("preview should report already-configured:\n%s", out.String())
			}
			if strings.Contains(out.String(), "This will run:") {
				t.Errorf("preview should not show the add command when already configured:\n%s", out.String())
			}
		})
	}
}

func TestMcpCliInstallNotOnPath(t *testing.T) {
	for _, tc := range cliToolCases() {
		t.Run(tc.arg, func(t *testing.T) {
			calls := fakeCLI(t, false, false) // not on PATH
			var out bytes.Buffer
			if err := run(context.Background(), []string{"mcp", tc.arg, "install"}, &out, &out); err != nil {
				t.Fatalf("mcp %s install: %v", tc.arg, err)
			}
			if len(*calls) != 0 {
				t.Errorf("nothing should run when the CLI is missing, got %v", *calls)
			}
			for _, want := range []string{
				tc.display + " CLI (" + tc.arg + ") not found on PATH.",
				tc.addCommand,
			} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing manual-command hint %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// --- claude: the generic mcp-add plus the burrow-CLI deny rule (harden step) ---

// claudeDeny reads permissions.deny from a settings file as a slice of strings.
func claudeDeny(t *testing.T, path string) []string {
	t.Helper()
	root := readCursor(t, path)
	perms, _ := root["permissions"].(map[string]any)
	raw, _ := perms["deny"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func countRule(rules []string, want string) int {
	n := 0
	for _, r := range rules {
		if r == want {
			n++
		}
	}
	return n
}

// TestMcpClaudeInstallAddsMcpAndHardens is the default path: install adds the MCP server AND writes
// the burrow-CLI deny rule to a fresh ~/.claude/settings.json, printing the add line, the harden
// note, and the try-prompt.
func TestMcpClaudeInstallAddsMcpAndHardens(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	calls := fakeCLI(t, true, false) // on PATH, mcp not yet configured

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("claude install: %v", err)
	}

	// The MCP add ran exactly once (the harden step writes a file, it does not exec).
	if len(*calls) != 1 {
		t.Fatalf("ran %d commands, want exactly the add: %v", len(*calls), *calls)
	}
	if got := strings.Join((*calls)[0], " "); got != "claude mcp add --scope user burrow -- burrow-mcp" {
		t.Errorf("add invocation = %q", got)
	}

	// The settings file was created with the deny rule.
	if deny := claudeDeny(t, settings); countRule(deny, "Bash(burrow *)") != 1 {
		t.Errorf("deny rules = %v, want exactly one Bash(burrow *)", deny)
	}
	if _, err := os.Stat(settings + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a backup was made for a brand-new file")
	}

	for _, want := range []string{
		"Added Burrow to Claude Code.",
		"added Bash(burrow *) to ~/.claude/settings.json",
		"see docs/HARDENING.md",
		"Then open your agent and try:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("install output missing %q:\n%s", want, out.String())
		}
	}
}

// TestMcpClaudeInstallDenyKubectlAddsBothRules confirms --deny-kubectl writes BOTH the burrow-CLI and
// the kubectl deny rules, records both in the output, and does not print the recommendation to enable
// what is already enabled.
func TestMcpClaudeInstallDenyKubectlAddsBothRules(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	fakeCLI(t, true, false) // on PATH, mcp not yet configured

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install", "--deny-kubectl"}, &out, &out); err != nil {
		t.Fatalf("claude install --deny-kubectl: %v", err)
	}

	deny := claudeDeny(t, settings)
	if countRule(deny, "Bash(burrow *)") != 1 || countRule(deny, "Bash(kubectl *)") != 1 {
		t.Errorf("deny rules = %v, want exactly one each of Bash(burrow *) and Bash(kubectl *)", deny)
	}
	if !strings.Contains(out.String(), "Bash(burrow *) and Bash(kubectl *)") {
		t.Errorf("output should record both rules were added:\n%s", out.String())
	}
	// The recommendation only nudges users who did NOT pass the flag.
	if strings.Contains(out.String(), "--deny-kubectl") {
		t.Errorf("output should not recommend a flag that was already passed:\n%s", out.String())
	}
}

// TestMcpClaudeInstallRecommendsDenyKubectl confirms the default install (no --deny-kubectl) adds only
// the burrow rule but prints the recommendation to also block kubectl.
func TestMcpClaudeInstallRecommendsDenyKubectl(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	fakeCLI(t, true, false)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("claude install: %v", err)
	}
	deny := claudeDeny(t, settings)
	if countRule(deny, "Bash(burrow *)") != 1 || countRule(deny, "Bash(kubectl *)") != 0 {
		t.Errorf("deny rules = %v, want only Bash(burrow *)", deny)
	}
	for _, want := range []string{
		"Recommended: keep every cluster change flowing through Burrow's guardrails and audit log",
		"burrow mcp claude install --deny-kubectl",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("default install missing the recommendation %q:\n%s", want, out.String())
		}
	}
}

// TestMcpClaudeInstallDenyKubectlIdempotent runs --deny-kubectl twice: the second run adds no
// duplicate of either rule and preserves the pre-existing unrelated deny/allow rules.
func TestMcpClaudeInstallDenyKubectlIdempotent(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{
  "permissions": {
    "deny": [
      "Bash(rm -rf *)"
    ],
    "allow": [
      "Bash(docker *)"
    ]
  }
}
`
	if err := os.WriteFile(settings, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeCLI(t, true, true) // mcp already configured, so only the harden step can act

	for i := 0; i < 2; i++ {
		var out bytes.Buffer
		if err := run(context.Background(), []string{"mcp", "claude", "install", "--deny-kubectl"}, &out, &out); err != nil {
			t.Fatalf("claude install --deny-kubectl (run %d): %v", i+1, err)
		}
	}

	deny := claudeDeny(t, settings)
	if countRule(deny, "Bash(burrow *)") != 1 || countRule(deny, "Bash(kubectl *)") != 1 {
		t.Errorf("deny rules = %v, want exactly one each after two runs", deny)
	}
	if countRule(deny, "Bash(rm -rf *)") != 1 {
		t.Errorf("pre-existing deny rule dropped: %v", deny)
	}
	root := readCursor(t, settings)
	perms, _ := root["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Bash(docker *)" {
		t.Errorf("allow rules dropped: %#v", perms["allow"])
	}
}

// TestMcpClaudeInstallNoHardenBeatsDenyKubectl confirms --no-harden wins over --deny-kubectl: neither
// rule is written, the settings file is never created, and the ignored-flag note is printed.
func TestMcpClaudeInstallNoHardenBeatsDenyKubectl(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	fakeCLI(t, true, false)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install", "--no-harden", "--deny-kubectl"}, &out, &out); err != nil {
		t.Fatalf("claude install --no-harden --deny-kubectl: %v", err)
	}
	assertNoFile(t, settings)
	assertNoFile(t, settings+".bak")
	if !strings.Contains(out.String(), "--deny-kubectl was ignored because hardening is off") {
		t.Errorf("expected the ignored-flag note:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Bash(kubectl *)") || strings.Contains(out.String(), "added Bash(burrow *)") {
		t.Errorf("no rule should be reported added:\n%s", out.String())
	}
}

// TestMcpClaudeInstallPreservesAndBacksUp confirms the harden merge keeps unrelated settings and a
// pre-existing deny rule, and backs the original up byte-for-byte.
func TestMcpClaudeInstallPreservesAndBacksUp(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{
  "model": "opus",
  "permissions": {
    "deny": [
      "Bash(rm -rf *)"
    ],
    "allow": [
      "Bash(docker *)"
    ]
  }
}
`
	if err := os.WriteFile(settings, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeCLI(t, true, false)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("claude install: %v", err)
	}

	// The backup preserves the original exactly.
	bak, err := os.ReadFile(settings + ".bak")
	if err != nil {
		t.Fatalf("expected a .bak of the pre-existing file: %v", err)
	}
	if string(bak) != pre {
		t.Errorf("backup content = %q, want the original", string(bak))
	}

	// The merged file keeps the unrelated key, the allow rule, and the pre-existing deny rule, and
	// adds the burrow deny rule.
	root := readCursor(t, settings)
	if root["model"] != "opus" {
		t.Errorf("unrelated top-level key dropped: %#v", root)
	}
	perms, _ := root["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Bash(docker *)" {
		t.Errorf("allow rules dropped: %#v", perms["allow"])
	}
	deny := claudeDeny(t, settings)
	if countRule(deny, "Bash(rm -rf *)") != 1 || countRule(deny, "Bash(burrow *)") != 1 {
		t.Errorf("deny rules = %v, want the pre-existing rule preserved and burrow added", deny)
	}
}

// TestMcpClaudeInstallIsIdempotent runs install twice with the MCP server already present so the
// harden step is the only work: the first run adds the rule, the second is a no-op with no duplicate.
func TestMcpClaudeInstallIsIdempotent(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	fakeCLI(t, true, true) // mcp already configured, so only the harden step can act

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("claude install: %v", err)
	}
	if !strings.Contains(out.String(), "added Bash(burrow *) to ~/.claude/settings.json") {
		t.Errorf("first run should report the harden note:\n%s", out.String())
	}

	out.Reset()
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("claude install (2nd): %v", err)
	}
	if !strings.Contains(out.String(), "Burrow is already configured in Claude Code. Nothing to do.") {
		t.Errorf("2nd run should be a no-op:\n%s", out.String())
	}
	if deny := claudeDeny(t, settings); countRule(deny, "Bash(burrow *)") != 1 {
		t.Errorf("deny rules = %v, want exactly one Bash(burrow *) after two runs", deny)
	}
}

// TestMcpClaudeInstallNoHarden confirms --no-harden does the MCP add but never touches the settings
// file.
func TestMcpClaudeInstallNoHarden(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	calls := fakeCLI(t, true, false)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install", "--no-harden"}, &out, &out); err != nil {
		t.Fatalf("claude install --no-harden: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("ran %d commands, want exactly the add: %v", len(*calls), *calls)
	}
	if !strings.Contains(out.String(), "Added Burrow to Claude Code.") {
		t.Errorf("missing the add success line:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Bash(burrow *)") {
		t.Errorf("--no-harden should not mention the deny rule:\n%s", out.String())
	}
	assertNoFile(t, settings)
	assertNoFile(t, settings+".bak")
}

// TestMcpClaudePreviewShowsBothParts confirms the preview shows the MCP add command and the harden
// step (with the --no-harden opt-out), mutating nothing.
func TestMcpClaudePreviewShowsBothParts(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	fakeCLI(t, true, false) // on PATH, not yet configured

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude"}, &out, &out); err != nil {
		t.Fatalf("mcp claude: %v", err)
	}
	for _, want := range []string{
		"Connect Burrow to Claude Code.",
		"claude mcp add --scope user burrow -- burrow-mcp",
		"Bash(burrow *)",
		"~/.claude/settings.json",
		"--no-harden",
		"Run `burrow mcp claude install` to apply.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("claude preview missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, settings)
}

// TestMcpClaudePreviewNoHarden confirms the preview reflects --no-harden: the add still shows, the
// harden step is called off.
func TestMcpClaudePreviewNoHarden(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	fakeCLI(t, true, false)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "--no-harden"}, &out, &out); err != nil {
		t.Fatalf("mcp claude --no-harden: %v", err)
	}
	if !strings.Contains(out.String(), "claude mcp add --scope user burrow -- burrow-mcp") {
		t.Errorf("preview should still show the add command:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Hardening is off (--no-harden)") {
		t.Errorf("preview should note hardening is off:\n%s", out.String())
	}
	assertNoFile(t, settings)
}

// TestMcpClaudePreviewBothConfigured confirms that once the MCP server and the deny rule are both
// present, the preview reports nothing to do and mutates nothing.
func TestMcpClaudePreviewBothConfigured(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"permissions":{"deny":["Bash(burrow *)"]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeCLI(t, true, true) // mcp already configured

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude"}, &out, &out); err != nil {
		t.Fatalf("mcp claude: %v", err)
	}
	if !strings.Contains(out.String(), "Burrow is already configured in Claude Code. Nothing to do.") {
		t.Errorf("preview should report nothing to do:\n%s", out.String())
	}
	if _, err := os.Stat(settings + ".bak"); !os.IsNotExist(err) {
		t.Errorf("preview must not back up or write anything")
	}
}

// --- cursor: the file-merge adapter (no CLI) ---

func TestMcpCursorPreviewMutatesNothing(t *testing.T) {
	cursor := mcpTempCursorConfig(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "cursor"}, &out, &out); err != nil {
		t.Fatalf("mcp cursor: %v", err)
	}
	for _, want := range []string{
		"Connect Burrow to Cursor.",
		"This will add to ~/.cursor/mcp.json:",
		"\"command\": \"burrow-mcp\"",
		"Run `burrow mcp cursor install` to apply.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("cursor preview missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, cursor)
}

func TestMcpCursorInstallCreatesMergesAndIsIdempotent(t *testing.T) {
	cursor := mcpTempCursorConfig(t)

	// First install: creates the file with burrow, no backup (nothing pre-existed).
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "cursor", "install"}, &out, &out); err != nil {
		t.Fatalf("cursor install: %v", err)
	}
	if !strings.Contains(out.String(), "Added Burrow to Cursor") {
		t.Errorf("missing success line:\n%s", out.String())
	}
	if _, err := os.Stat(cursor + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a backup was made for a brand-new file")
	}
	if got := cursorBurrowCommand(t, cursor); got != "burrow-mcp" {
		t.Errorf("burrow command = %q, want burrow-mcp", got)
	}

	// Second install: idempotent, and it backs up the now-existing file.
	out.Reset()
	if err := run(context.Background(), []string{"mcp", "cursor", "install"}, &out, &out); err != nil {
		t.Fatalf("cursor install (2nd): %v", err)
	}
	if !strings.Contains(out.String(), "already configured in Cursor") {
		t.Errorf("2nd run should be idempotent:\n%s", out.String())
	}
}

func TestMcpCursorInstallPreservesOtherServersAndBacksUp(t *testing.T) {
	cursor := mcpTempCursorConfig(t)
	if err := os.MkdirAll(filepath.Dir(cursor), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{
  "mcpServers": {
    "other": {
      "command": "other-mcp"
    }
  },
  "someOtherKey": true
}
`
	if err := os.WriteFile(cursor, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "cursor", "install"}, &out, &out); err != nil {
		t.Fatalf("cursor install: %v", err)
	}

	// Backup preserves the original content exactly.
	bak, err := os.ReadFile(cursor + ".bak")
	if err != nil {
		t.Fatalf("expected a .bak of the pre-existing file: %v", err)
	}
	if string(bak) != pre {
		t.Errorf("backup content = %q, want the original", string(bak))
	}

	// The merged file keeps the other server and top-level key, and adds burrow.
	root := readCursor(t, cursor)
	servers, _ := root["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Errorf("the other server was dropped: %#v", servers)
	}
	if _, ok := servers["burrow"]; !ok {
		t.Errorf("burrow was not added: %#v", servers)
	}
	if root["someOtherKey"] != true {
		t.Errorf("a sibling top-level key was dropped: %#v", root)
	}
}

// --- opencode: the file-config adapter (mcp key, local stdio server) ---

func TestMcpOpencodePreviewMutatesNothing(t *testing.T) {
	opencode := mcpTempOpencodeConfig(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "opencode"}, &out, &out); err != nil {
		t.Fatalf("mcp opencode: %v", err)
	}
	for _, want := range []string{
		"Connect Burrow to OpenCode.",
		"This will add to ~/.config/opencode/opencode.json:",
		"\"type\": \"local\"",
		"\"command\": [\"burrow-mcp\"]",
		"Run `burrow mcp opencode install` to apply.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("opencode preview missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, opencode)
}

func TestMcpOpencodeInstallCreatesMergesAndIsIdempotent(t *testing.T) {
	opencode := mcpTempOpencodeConfig(t)

	// First install: creates the file with the burrow local server, no backup.
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "opencode", "install"}, &out, &out); err != nil {
		t.Fatalf("opencode install: %v", err)
	}
	if !strings.Contains(out.String(), "Added Burrow to OpenCode") {
		t.Errorf("missing success line:\n%s", out.String())
	}
	if _, err := os.Stat(opencode + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a backup was made for a brand-new file")
	}
	burrow := opencodeBurrowServer(t, opencode)
	if burrow["type"] != "local" || burrow["enabled"] != true {
		t.Errorf("burrow server = %#v, want type=local enabled=true", burrow)
	}
	cmd, _ := burrow["command"].([]any)
	if len(cmd) != 1 || cmd[0] != "burrow-mcp" {
		t.Errorf("burrow command = %#v, want [burrow-mcp]", burrow["command"])
	}
	// A fresh file carries OpenCode's schema pointer.
	if readCursor(t, opencode)["$schema"] != "https://opencode.ai/config.json" {
		t.Errorf("fresh config missing the $schema pointer")
	}

	// Second install: idempotent, and it backs up the now-existing file.
	out.Reset()
	if err := run(context.Background(), []string{"mcp", "opencode", "install"}, &out, &out); err != nil {
		t.Fatalf("opencode install (2nd): %v", err)
	}
	if !strings.Contains(out.String(), "already configured in OpenCode") {
		t.Errorf("2nd run should be idempotent:\n%s", out.String())
	}
}

func TestMcpOpencodeInstallPreservesOtherServersAndBacksUp(t *testing.T) {
	opencode := mcpTempOpencodeConfig(t)
	if err := os.MkdirAll(filepath.Dir(opencode), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{
  "$schema": "https://opencode.ai/config.json",
  "theme": "dark",
  "mcp": {
    "other": {
      "type": "local",
      "command": ["other-mcp"],
      "enabled": true
    }
  }
}
`
	if err := os.WriteFile(opencode, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "opencode", "install"}, &out, &out); err != nil {
		t.Fatalf("opencode install: %v", err)
	}

	// Backup preserves the original content exactly.
	bak, err := os.ReadFile(opencode + ".bak")
	if err != nil {
		t.Fatalf("expected a .bak of the pre-existing file: %v", err)
	}
	if string(bak) != pre {
		t.Errorf("backup content = %q, want the original", string(bak))
	}

	// The merged file keeps the other server, the $schema, and the sibling key, and adds burrow.
	root := readCursor(t, opencode)
	servers, _ := root["mcp"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Errorf("the other server was dropped: %#v", servers)
	}
	if _, ok := servers["burrow"]; !ok {
		t.Errorf("burrow was not added: %#v", servers)
	}
	if root["theme"] != "dark" || root["$schema"] != "https://opencode.ai/config.json" {
		t.Errorf("a sibling top-level key was dropped: %#v", root)
	}
}

// --- helpers ---

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no file at %s, but it exists (err=%v)", path, err)
	}
}

func readCursor(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	root := map[string]any{}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return root
}

func cursorBurrowCommand(t *testing.T, path string) string {
	t.Helper()
	root := readCursor(t, path)
	servers, _ := root["mcpServers"].(map[string]any)
	burrow, _ := servers["burrow"].(map[string]any)
	cmd, _ := burrow["command"].(string)
	return cmd
}

func opencodeBurrowServer(t *testing.T, path string) map[string]any {
	t.Helper()
	root := readCursor(t, path)
	servers, _ := root["mcp"].(map[string]any)
	burrow, _ := servers["burrow"].(map[string]any)
	if burrow == nil {
		t.Fatalf("no burrow server in %s: %#v", path, root)
	}
	return burrow
}
