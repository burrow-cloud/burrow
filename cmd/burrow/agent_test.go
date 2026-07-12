// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentTempClaudeMemory points the CLAUDE.md seam at a fresh temp file and returns its path. Nothing
// exists at the path until a test writes it, so a preview and a first install exercise the create
// branch without touching the real ~/.claude/CLAUDE.md.
func agentTempClaudeMemory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude", "CLAUDE.md")
	orig := claudeMemoryPath
	claudeMemoryPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { claudeMemoryPath = orig })
	return path
}

// claudeAllow reads permissions.allow from a settings file as a slice of strings.
func claudeAllow(t *testing.T, path string) []string {
	t.Helper()
	root := readCursor(t, path)
	perms, _ := root["permissions"].(map[string]any)
	raw, _ := perms["allow"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestAgentRuleStringsAreExact pins the load-bearing permission strings: the allow of the scoped
// binary and the word-boundary deny of the human CLI, plus the opt-in kubectl deny. A regression here
// would silently weaken the security boundary. There is deliberately no no-wildcard `Bash(burrow)`
// rule: a bare `burrow` only prints help, and an exact rule's match semantics against `burrow-agent`
// are undocumented while deny takes precedence, so a false match would catastrophically break wiring.
func TestAgentRuleStringsAreExact(t *testing.T) {
	cases := map[string]string{
		"allow (with args)": agentAllowRule,
		"allow (bare)":      agentAllowBareRule,
		"deny (with args)":  agentDenyRule,
		"deny kubectl":      agentDenyKubectlRule,
	}
	want := map[string]string{
		"allow (with args)": "Bash(burrow-agent *)",
		"allow (bare)":      "Bash(burrow-agent)",
		"deny (with args)":  "Bash(burrow *)", // the SPACE before * is the word boundary
		"deny kubectl":      "Bash(kubectl *)",
	}
	for name, got := range cases {
		if got != want[name] {
			t.Errorf("%s rule = %q, want %q", name, got, want[name])
		}
	}
	// The deny rule must never catch the scoped binary: `burrow-agent` has no space after `burrow`, so
	// the word-boundary `Bash(burrow *)` cannot match it.
	if strings.HasPrefix("burrow-agent", "burrow ") {
		t.Fatal("burrow-agent unexpectedly has a `burrow ` prefix; the deny would catch it")
	}
}

func TestAgentOverviewMutatesNothing(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	memory := agentTempClaudeMemory(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent"}, &out, &out); err != nil {
		t.Fatalf("agent: %v", err)
	}
	for _, want := range []string{
		"Wire your AI agent to burrow-agent",
		"claude    Claude Code",
		"burrow agent <tool>", "burrow agent <tool> install",
		"burrow-agent is a single binary on the agent's PATH",
		"Request support: https://github.com/burrow-cloud/burrow/issues/new",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("overview missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, settings)
	assertNoFile(t, memory)
}

// TestAgentUnknownToolFallback confirms a tool with no built-in wiring does not error but prints the
// wire-by-hand message naming the exact allow/deny rules, mutating nothing.
func TestAgentUnknownToolFallback(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	memory := agentTempClaudeMemory(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent", "cursor"}, &out, &out); err != nil {
		t.Fatalf("agent cursor: %v", err)
	}
	for _, want := range []string{
		`Burrow has no built-in agent wiring for "cursor" yet.`,
		"ALLOW `Bash(burrow-agent *)`",
		"DENY `Bash(burrow *)`",
		"Bash(kubectl *)",
		"Built-in wiring: claude.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("unknown-tool fallback missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, settings)
	assertNoFile(t, memory)
}

func TestAgentBadSecondArg(t *testing.T) {
	mcpTempClaudeSettings(t)
	agentTempClaudeMemory(t)
	var out bytes.Buffer
	err := run(context.Background(), []string{"agent", "claude", "bogus"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown argument") {
		t.Fatalf("err = %v, want an unknown-argument error", err)
	}
}

func TestAgentClaudePreviewMutatesNothing(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	memory := agentTempClaudeMemory(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent", "claude"}, &out, &out); err != nil {
		t.Fatalf("agent claude: %v", err)
	}
	for _, want := range []string{
		"Wire Claude Code to burrow-agent.",
		"Bash(burrow-agent *)",
		"Bash(burrow *)",
		"~/.claude/settings.json",
		"~/.claude/CLAUDE.md",
		"--deny-kubectl",
		"Run `burrow agent claude install` to apply.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("claude preview missing %q:\n%s", want, out.String())
		}
	}
	// The preview must not promise kubectl unless the flag was given.
	if strings.Contains(out.String(), "Bash(kubectl *)") {
		t.Errorf("preview should not list the kubectl rule without --deny-kubectl:\n%s", out.String())
	}
	assertNoFile(t, settings)
	assertNoFile(t, memory)
}

// TestAgentClaudeInstallWritesRules is the default path: a fresh install writes the allow and deny
// rules and the orientation block, and burrow-agent is never caught by the deny.
func TestAgentClaudeInstallWritesRules(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	memory := agentTempClaudeMemory(t)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("agent claude install: %v", err)
	}

	allow := claudeAllow(t, settings)
	if countRule(allow, "Bash(burrow-agent *)") != 1 || countRule(allow, "Bash(burrow-agent)") != 1 {
		t.Errorf("allow rules = %v, want the two burrow-agent rules", allow)
	}
	deny := claudeDeny(t, settings)
	if countRule(deny, "Bash(burrow *)") != 1 {
		t.Errorf("deny rules = %v, want exactly one Bash(burrow *)", deny)
	}
	// A bare `burrow` is intentionally NOT denied (help screen only; an exact rule risks a false match
	// on burrow-agent), so no no-wildcard rule is written.
	if countRule(deny, "Bash(burrow)") != 0 {
		t.Errorf("deny rules must not include a no-wildcard Bash(burrow): %v", deny)
	}
	// The scoped binary must never be denied: neither deny form may appear in the deny list.
	if countRule(deny, "Bash(burrow-agent *)") != 0 || countRule(deny, "Bash(burrow-agent)") != 0 {
		t.Errorf("deny rules must not catch burrow-agent: %v", deny)
	}
	if countRule(deny, "Bash(kubectl *)") != 0 {
		t.Errorf("kubectl should not be denied without the flag: %v", deny)
	}

	// The orientation block landed in CLAUDE.md.
	mem, err := os.ReadFile(memory)
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %v", err)
	}
	for _, want := range []string{agentInstructionsBegin, agentInstructionsEnd, "burrow-agent", "held_for_confirmation", "NEVER self-confirm", "major.minor.patch", "next-tag"} {
		if !strings.Contains(string(mem), want) {
			t.Errorf("CLAUDE.md missing %q:\n%s", want, string(mem))
		}
	}

	// Brand-new files get no backup.
	assertNoFile(t, settings+".bak")
	assertNoFile(t, memory+".bak")

	for _, want := range []string{
		"Wired Claude Code to burrow-agent",
		"Bash(burrow-agent *)",
		"Added a burrow-agent orientation block to ~/.claude/CLAUDE.md.",
		"Recommended:",
		"Then open your agent and try:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("install output missing %q:\n%s", want, out.String())
		}
	}
}

// TestAgentClaudeInstallDenyKubectl confirms --deny-kubectl adds the kubectl deny on top of the burrow
// denies and does not print the recommendation to enable what is already enabled.
func TestAgentClaudeInstallDenyKubectl(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	agentTempClaudeMemory(t)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent", "claude", "install", "--deny-kubectl"}, &out, &out); err != nil {
		t.Fatalf("agent claude install --deny-kubectl: %v", err)
	}
	deny := claudeDeny(t, settings)
	if countRule(deny, "Bash(burrow *)") != 1 || countRule(deny, "Bash(kubectl *)") != 1 {
		t.Errorf("deny rules = %v, want the burrow rule and kubectl", deny)
	}
	if countRule(deny, "Bash(burrow)") != 0 {
		t.Errorf("deny rules must not include a no-wildcard Bash(burrow): %v", deny)
	}
	if !strings.Contains(out.String(), "kubectl") {
		t.Errorf("output should record the kubectl deny:\n%s", out.String())
	}
	// The recommendation only nudges users who did NOT pass the flag.
	if strings.Contains(out.String(), "Recommended:") {
		t.Errorf("output should not recommend a flag that was already passed:\n%s", out.String())
	}
}

// TestAgentClaudeInstallIsIdempotent runs install twice: the second run adds no duplicate rule and
// makes no further orientation change, reporting nothing to do; it backs up the now-existing files.
func TestAgentClaudeInstallIsIdempotent(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	memory := agentTempClaudeMemory(t)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("agent claude install: %v", err)
	}

	out.Reset()
	if err := run(context.Background(), []string{"agent", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("agent claude install (2nd): %v", err)
	}
	if !strings.Contains(out.String(), "already wired to burrow-agent. Nothing to do.") {
		t.Errorf("2nd run should be a no-op:\n%s", out.String())
	}

	allow := claudeAllow(t, settings)
	if countRule(allow, "Bash(burrow-agent *)") != 1 || countRule(allow, "Bash(burrow-agent)") != 1 {
		t.Errorf("allow rules = %v, want exactly one of each after two runs", allow)
	}
	deny := claudeDeny(t, settings)
	if countRule(deny, "Bash(burrow *)") != 1 {
		t.Errorf("deny rules = %v, want exactly one Bash(burrow *) after two runs", deny)
	}
	// The orientation block appears exactly once.
	mem, err := os.ReadFile(memory)
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %v", err)
	}
	if n := strings.Count(string(mem), agentInstructionsBegin); n != 1 {
		t.Errorf("orientation block appears %d times, want exactly 1", n)
	}
}

// TestAgentClaudeInstallPreservesAndBacksUp confirms the merge keeps unrelated settings and pre-existing
// rules, preserves the user's own CLAUDE.md, and backs both originals up byte-for-byte.
func TestAgentClaudeInstallPreservesAndBacksUp(t *testing.T) {
	settings := mcpTempClaudeSettings(t)
	memory := agentTempClaudeMemory(t)
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	preSettings := `{
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
	if err := os.WriteFile(settings, []byte(preSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(memory), 0o755); err != nil {
		t.Fatal(err)
	}
	preMemory := "# My notes\n\nAlways write tests.\n"
	if err := os.WriteFile(memory, []byte(preMemory), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("agent claude install: %v", err)
	}

	// Backups preserve the originals exactly.
	if bak, err := os.ReadFile(settings + ".bak"); err != nil || string(bak) != preSettings {
		t.Errorf("settings backup = %q (err %v), want the original", string(bak), err)
	}
	if bak, err := os.ReadFile(memory + ".bak"); err != nil || string(bak) != preMemory {
		t.Errorf("memory backup = %q (err %v), want the original", string(bak), err)
	}

	// The merged settings keep the unrelated key, the pre-existing allow and deny rules, and add ours.
	root := readCursor(t, settings)
	if root["model"] != "opus" {
		t.Errorf("unrelated top-level key dropped: %#v", root)
	}
	allow := claudeAllow(t, settings)
	if countRule(allow, "Bash(docker *)") != 1 || countRule(allow, "Bash(burrow-agent *)") != 1 {
		t.Errorf("allow rules = %v, want docker preserved and burrow-agent added", allow)
	}
	deny := claudeDeny(t, settings)
	if countRule(deny, "Bash(rm -rf *)") != 1 || countRule(deny, "Bash(burrow *)") != 1 {
		t.Errorf("deny rules = %v, want the pre-existing rule preserved and burrow added", deny)
	}

	// The merged CLAUDE.md keeps the user's own note and appends the orientation block.
	mem, err := os.ReadFile(memory)
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(mem), "Always write tests.") {
		t.Errorf("user's own CLAUDE.md content dropped:\n%s", string(mem))
	}
	if !strings.Contains(string(mem), agentInstructionsBegin) {
		t.Errorf("orientation block not appended:\n%s", string(mem))
	}
}

// TestAgentClaudeInstallRefreshesStaleBlock confirms a re-run rewrites the fenced orientation region if
// its text drifted, without duplicating the block or disturbing the user's own memory.
func TestAgentClaudeInstallRefreshesStaleBlock(t *testing.T) {
	memory := agentTempClaudeMemory(t)
	mcpTempClaudeSettings(t)
	if err := os.MkdirAll(filepath.Dir(memory), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "# Notes\n\n" + agentInstructionsBegin + "\nold stale text\n" + agentInstructionsEnd + "\n\nMore notes.\n"
	if err := os.WriteFile(memory, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("agent claude install: %v", err)
	}

	mem, err := os.ReadFile(memory)
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %v", err)
	}
	if strings.Contains(string(mem), "old stale text") {
		t.Errorf("stale block text was not refreshed:\n%s", string(mem))
	}
	if n := strings.Count(string(mem), agentInstructionsBegin); n != 1 {
		t.Errorf("block appears %d times after refresh, want 1", n)
	}
	if !strings.Contains(string(mem), "More notes.") || !strings.Contains(string(mem), "# Notes") {
		t.Errorf("surrounding user memory disturbed:\n%s", string(mem))
	}
}
