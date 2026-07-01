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
	var out bytes.Buffer
	if err := run(context.Background(), []string{"mcp"}, &out, &out); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	for _, want := range []string{
		"Connect Burrow to your AI agent",
		"claude    Claude Code", "cursor    Cursor", "codex     Codex", "copilot   Copilot",
		"burrow mcp <tool>", "burrow mcp <tool> install",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("overview missing %q:\n%s", want, out.String())
		}
	}
	assertNoFile(t, cursor)
}

func TestMcpUnknownTool(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"mcp", "bogus"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("err = %v, want an unknown-tool error", err)
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

func cliToolCases() []cliToolCase {
	return []cliToolCase{
		{arg: "claude", display: "Claude Code", addCommand: "claude mcp add --scope user burrow -- burrow-mcp"},
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
