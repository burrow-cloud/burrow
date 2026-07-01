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

// mcpTempConfigs points the cursor and codex config seams at fresh temp files and returns their
// paths. Nothing exists at either path until a test writes it, so preview must not create them.
func mcpTempConfigs(t *testing.T) (cursor, codex string) {
	t.Helper()
	dir := t.TempDir()
	cursor = filepath.Join(dir, "cursor", "mcp.json")
	codex = filepath.Join(dir, "codex", "config.toml")

	origCursor, origCodex := cursorConfigPath, codexConfigPath
	cursorConfigPath = func() (string, error) { return cursor, nil }
	codexConfigPath = func() (string, error) { return codex, nil }
	t.Cleanup(func() {
		cursorConfigPath = origCursor
		codexConfigPath = origCodex
	})
	return cursor, codex
}

// fakeClaude stubs the claude-CLI seams: whether `claude` is on PATH, whether it reports burrow
// already configured, and it records every runCommand invocation. It restores the seams on cleanup.
func fakeClaude(t *testing.T, onPath, configured bool) *[][]string {
	t.Helper()
	var calls [][]string

	origLook, origRun, origSucceeds := mcpLookPath, runCommand, commandSucceeds
	mcpLookPath = func(string) (string, error) {
		if onPath {
			return "/usr/local/bin/claude", nil
		}
		return "", os.ErrNotExist
	}
	commandSucceeds = func(name string, args ...string) bool {
		return configured
	}
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
	cursor, codex := mcpTempConfigs(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp"}, &out, &out); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	for _, want := range []string{
		"Connect Burrow to your AI agent",
		"claude   Claude Code", "cursor   Cursor", "codex    Codex",
		"burrow mcp <tool>", "burrow mcp <tool> install",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("overview missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, cursor)
	assertNoFile(t, codex)
}

func TestMcpUnknownTool(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"mcp", "bogus"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("err = %v, want an unknown-tool error", err)
	}
}

func TestMcpBadSecondArg(t *testing.T) {
	fakeClaude(t, true, false)
	var out bytes.Buffer
	err := run(context.Background(), []string{"mcp", "claude", "bogus"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown argument") {
		t.Fatalf("err = %v, want an unknown-argument error", err)
	}
}

// --- claude preview + install ---

func TestMcpClaudePreviewMutatesNothing(t *testing.T) {
	calls := fakeClaude(t, true, false) // on PATH, not yet configured
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude"}, &out, &out); err != nil {
		t.Fatalf("mcp claude: %v", err)
	}
	for _, want := range []string{
		"Connect Burrow to Claude Code.",
		"claude mcp add --scope user burrow -- burrow-mcp",
		"Run `burrow mcp claude install` to apply.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("claude preview missing %q:\n%s", want, out.String())
		}
	}
	if len(*calls) != 0 {
		t.Errorf("preview ran a command: %v", *calls)
	}
}

func TestMcpClaudeInstallInvokesAdd(t *testing.T) {
	calls := fakeClaude(t, true, false) // on PATH, not configured -> add runs
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("mcp claude install: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("ran %d commands, want exactly the add: %v", len(*calls), *calls)
	}
	got := strings.Join((*calls)[0], " ")
	if got != "claude mcp add --scope user burrow -- burrow-mcp" {
		t.Errorf("add invocation = %q", got)
	}
	if !strings.Contains(out.String(), "Added Burrow to Claude Code.") {
		t.Errorf("missing success line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Then open your agent and try:") {
		t.Errorf("missing the try-prompt:\n%s", out.String())
	}
}

func TestMcpClaudeInstallAlreadyConfigured(t *testing.T) {
	calls := fakeClaude(t, true, true) // on PATH and already configured -> no add
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("mcp claude install: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("add should not run when already configured, got %v", *calls)
	}
	if !strings.Contains(out.String(), "already configured in Claude Code") {
		t.Errorf("missing already-configured message:\n%s", out.String())
	}
}

func TestMcpClaudePreviewAlreadyConfigured(t *testing.T) {
	fakeClaude(t, true, true)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude"}, &out, &out); err != nil {
		t.Fatalf("mcp claude: %v", err)
	}
	if !strings.Contains(out.String(), "already configured in Claude Code") {
		t.Errorf("preview should report already-configured:\n%s", out.String())
	}
	if strings.Contains(out.String(), "This will run:") {
		t.Errorf("preview should not show the add command when already configured:\n%s", out.String())
	}
}

func TestMcpClaudeInstallNotOnPath(t *testing.T) {
	calls := fakeClaude(t, false, false) // not on PATH
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &out); err != nil {
		t.Fatalf("mcp claude install: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("nothing should run when claude is missing, got %v", *calls)
	}
	for _, want := range []string{
		"Claude Code CLI (claude) not found on PATH.",
		"claude mcp add --scope user burrow -- burrow-mcp",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing manual-command hint %q:\n%s", want, out.String())
		}
	}
}

// --- cursor preview + install ---

func TestMcpCursorPreviewMutatesNothing(t *testing.T) {
	cursor, _ := mcpTempConfigs(t)
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
	cursor, _ := mcpTempConfigs(t)

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
	cursor, _ := mcpTempConfigs(t)
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

// --- codex preview + install ---

func TestMcpCodexPreviewMutatesNothing(t *testing.T) {
	_, codex := mcpTempConfigs(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "codex"}, &out, &out); err != nil {
		t.Fatalf("mcp codex: %v", err)
	}
	for _, want := range []string{
		"Connect Burrow to Codex.",
		"This will add to ~/.codex/config.toml:",
		"[mcp_servers.burrow]",
		"command = \"burrow-mcp\"",
		"Run `burrow mcp codex install` to apply.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("codex preview missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, codex)
}

func TestMcpCodexInstallAppendsIdempotentBackup(t *testing.T) {
	_, codex := mcpTempConfigs(t)
	if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := "[other]\nkey = \"value\"\n"
	if err := os.WriteFile(codex, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "codex", "install"}, &out, &out); err != nil {
		t.Fatalf("codex install: %v", err)
	}
	if !strings.Contains(out.String(), "Added Burrow to Codex") {
		t.Errorf("missing success line:\n%s", out.String())
	}

	// Backup made of the pre-existing file.
	bak, err := os.ReadFile(codex + ".bak")
	if err != nil || string(bak) != pre {
		t.Errorf("backup = %q err=%v, want the original", string(bak), err)
	}

	// The appended table sits after the existing content, separated by a blank line.
	got, err := os.ReadFile(codex)
	if err != nil {
		t.Fatal(err)
	}
	want := pre + "\n[mcp_servers.burrow]\ncommand = \"burrow-mcp\"\n"
	if string(got) != want {
		t.Errorf("codex file =\n%q\nwant\n%q", string(got), want)
	}

	// Second run is idempotent and does not append again.
	out.Reset()
	if err := run(context.Background(), []string{"mcp", "codex", "install"}, &out, &out); err != nil {
		t.Fatalf("codex install (2nd): %v", err)
	}
	if !strings.Contains(out.String(), "already configured in Codex") {
		t.Errorf("2nd run should be idempotent:\n%s", out.String())
	}
	again, _ := os.ReadFile(codex)
	if string(again) != want {
		t.Errorf("idempotent run changed the file:\n%q", string(again))
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
