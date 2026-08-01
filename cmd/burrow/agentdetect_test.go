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

// TestDetectByConfigDirectoryNotPath confirms detection is by the tool's own config directory. A
// name on $PATH may be a different program; a directory the tool created is far better evidence.
func TestDetectByConfigDirectoryNotPath(t *testing.T) {
	home := t.TempDir()
	orig := agentHomeDir
	agentHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { agentHomeDir = orig })

	if got := detectCodingAgents(); len(got) != 0 {
		t.Fatalf("detected %v in an empty home, want nothing", got)
	}
	// A FILE at the config path is not a config directory and must not count.
	if err := os.WriteFile(filepath.Join(home, ".codex"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := detectCodingAgents(); len(got) != 0 {
		t.Fatalf("detected %v from a plain file, want nothing", got)
	}

	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := detectCodingAgents()
	if len(got) != 1 || got[0].Tool != "claude" {
		t.Fatalf("detected %v, want Claude Code alone", got)
	}
	if !claudeCodeDetected() {
		t.Error("claudeCodeDetected() = false with ~/.claude present")
	}
}

// TestDetectionTableRecordsTheKnownAgents confirms the table carries the four config directories the
// triage recorded, so nobody re-derives the paths later, and that only Claude Code is wireable today.
func TestDetectionTableRecordsTheKnownAgents(t *testing.T) {
	want := map[string]string{
		"Claude Code": ".claude",
		"Codex":       ".codex",
		"Cursor":      ".cursor",
		"Windsurf":    filepath.Join(".codeium", "windsurf"),
	}
	if len(codingAgents) != len(want) {
		t.Fatalf("detection table has %d rows, want %d", len(codingAgents), len(want))
	}
	wireable := 0
	for _, a := range codingAgents {
		if got := want[a.Display]; got != a.ConfigDir {
			t.Errorf("%s config dir = %q, want %q", a.Display, a.ConfigDir, got)
		}
		if a.wireable() {
			wireable++
		}
	}
	if wireable != 1 {
		t.Errorf("%d wireable tools, want exactly 1 (only Claude Code is supported today)", wireable)
	}
}

// TestLoginOffersDetectedClaudeAndDefaultsToYes confirms the offer defaults to yes: an empty answer
// wires Claude Code, and the prompt shows Y as the capitalized default.
func TestLoginOffersDetectedClaudeAndDefaultsToYes(t *testing.T) {
	f := stubAuth(t, authContexts(), true)
	f.installClaudeConfigDir(t)

	var out bytes.Buffer
	// Other, first context, then an empty line at the wiring offer.
	if err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n1\n\n"), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Found Claude Code. Restrict it to the safe burrow-agent CLI? (Y/n)") {
		t.Errorf("the offer is missing or does not default to yes:\n%s", got)
	}
	settings, _ := claudeSettingsPath()
	if countRule(claudeAllow(t, settings), agentAllowRule) != 1 {
		t.Errorf("pressing return at the offer did not wire Claude Code:\n%s", got)
	}
	if countRule(claudeDeny(t, settings), agentDenyRule) != 1 {
		t.Errorf("the deny rule was not written:\n%s", got)
	}
}

// TestLoginDecliningLeavesAWorkingInstall confirms saying no writes nothing to the third-party
// settings file, still records the target, and names the explicit command for later.
func TestLoginDecliningLeavesAWorkingInstall(t *testing.T) {
	f := stubAuth(t, authContexts(), true)
	f.installClaudeConfigDir(t)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n1\nn\n"), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	settings, _ := claudeSettingsPath()
	if _, err := os.Stat(settings); err == nil {
		t.Error("declining still wrote to the agent's settings file")
	}
	if got := loadAuthConfig(t).CurrentTarget; got != "kind-dev" {
		t.Errorf("active target = %q, want the login to have stood regardless of the offer", got)
	}
	if !strings.Contains(out.String(), "burrow agent claude install") {
		t.Errorf("declining did not name the explicit command:\n%s", out.String())
	}
}

// TestLoginNonInteractiveOnlyTellsYou confirms the distinction that decides the behaviour is
// interactive versus not: a run with no terminal prints the pointer and asks nothing.
func TestLoginNonInteractiveOnlyTellsYou(t *testing.T) {
	f := stubAuth(t, authContexts(), false)
	f.installClaudeConfigDir(t)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{kubeContext: "kind-dev"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "(Y/n)") || strings.Contains(got, "(y/N)") {
		t.Errorf("a non-interactive run prompted:\n%s", got)
	}
	if !strings.Contains(got, "burrow agent claude install") {
		t.Errorf("a non-interactive run did not print the pointer:\n%s", got)
	}
	settings, _ := claudeSettingsPath()
	if _, err := os.Stat(settings); err == nil {
		t.Error("a non-interactive run wrote to the agent's settings file")
	}
}

// TestLoginNothingDetectedStatesWhatIsSupported confirms the no-detection branch: a statement of
// what is supported and where to ask for more, and the single y/N escape hatch for a working install
// in a non-standard location. Deliberately not a picker with one actionable entry.
func TestLoginNothingDetectedStatesWhatIsSupported(t *testing.T) {
	stubAuth(t, authContexts(), true)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n1\n\n"), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "No coding agent was detected") {
		t.Errorf("output does not state that nothing was found:\n%s", got)
	}
	if !strings.Contains(got, "Claude Code") || !strings.Contains(got, agentIssuesURL) {
		t.Errorf("output does not name what is supported and where to request more:\n%s", got)
	}
	if !strings.Contains(got, "Wire Claude Code anyway? (y/N)") {
		t.Errorf("the escape hatch is missing or does not default to no:\n%s", got)
	}
	// The empty answer took the default, which here is NO.
	settings, _ := claudeSettingsPath()
	if _, err := os.Stat(settings); err == nil {
		t.Error("the y/N offer defaulted to yes; it must default to no")
	}
}

// TestLoginNothingDetectedAnywayWiresOnYes confirms the escape hatch works for the case it exists
// for: a working install detection missed.
func TestLoginNothingDetectedAnywayWiresOnYes(t *testing.T) {
	stubAuth(t, authContexts(), true)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n1\ny\n"), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	settings, _ := claudeSettingsPath()
	if countRule(claudeAllow(t, settings), agentAllowRule) != 1 {
		t.Errorf("answering yes did not wire Claude Code:\n%s", out.String())
	}
}

// TestLoginNamesAnUnwireableDetectedTool confirms someone who plainly has a coding agent installed is
// not told nothing was found, even when Burrow cannot wire it yet.
func TestLoginNamesAnUnwireableDetectedTool(t *testing.T) {
	f := stubAuth(t, authContexts(), true)
	if err := os.MkdirAll(filepath.Join(f.home, ".cursor"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n1\n\n"), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	if !strings.Contains(out.String(), "Found Cursor, which Burrow has no built-in wiring for yet.") {
		t.Errorf("output does not name the detected but unwireable tool:\n%s", out.String())
	}
}

// TestAuthStatusReportsAnUnwiredAgent confirms status carries the one actionable line about an
// installed agent that is not restricted to burrow-agent, and stays quiet once it is wired.
func TestAuthStatusReportsAnUnwiredAgent(t *testing.T) {
	f := stubAuth(t, authContexts(), false)
	f.installClaudeConfigDir(t)

	var out bytes.Buffer
	if err := runAuthStatus(&out, authStatusOpts{}); err != nil {
		t.Fatalf("runAuthStatus: %v", err)
	}
	if !strings.Contains(out.String(), "burrow agent claude install") {
		t.Errorf("status does not report the unwired agent:\n%s", out.String())
	}

	if err := (claudeAgentTool{}).install(&bytes.Buffer{}); err != nil {
		t.Fatalf("wiring Claude Code: %v", err)
	}
	out.Reset()
	if err := runAuthStatus(&out, authStatusOpts{}); err != nil {
		t.Fatalf("runAuthStatus: %v", err)
	}
	if strings.Contains(out.String(), "not restricted to burrow-agent") {
		t.Errorf("status still nags after the agent was wired:\n%s", out.String())
	}
}
