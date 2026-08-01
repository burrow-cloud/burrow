// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Detecting an installed coding agent, and offering to wire it, is the tail of `burrow auth login`
// and a one-line report in `burrow auth status` (issue #413). `burrow install` already prints the
// pointer at the end of the self-hosted path; `auth login` is the symmetric moment on the other
// path, and it is the first command anyone runs.
//
// It ASKS rather than assumes, because it writes to a THIRD-PARTY tool's configuration file
// (~/.claude/settings.json), not one of ours. Silently editing it because somebody logged in is a
// different act from writing our own credential, however good the intent. Declining is a fine
// answer and leaves a working install; `burrow agent <tool> install` stays the explicit command for
// re-running it, for a tool installed later, and for anyone who said no.
//
// Worth stating plainly: the deny rule the wiring writes is a COOPERATIVE control, not a security
// boundary. It stops an agent that respects its settings file, which covers accidents and honest
// mistakes. It does not stop one that ignores it — and such an agent would be running `burrow` with
// the HUMAN's credential, which is why the two credentials being distinct (ADR-0038) is the control
// that actually holds.

// agentHomeDir resolves the home directory the detection table is rooted at. It is a package var so
// a test points it at a temp dir and never reads (or reports on) the real ~/.
var agentHomeDir = os.UserHomeDir

// codingAgent is one row of the detection table. Tool is the `burrow agent <tool>` name and is set
// only for a tool Burrow has built-in wiring for; ConfigDir is relative to the home directory.
type codingAgent struct {
	Tool      string
	Display   string
	ConfigDir string
}

// wired reports whether Burrow ships built-in wiring for this tool.
func (a codingAgent) wireable() bool { return a.Tool != "" }

// codingAgents is the detection table. Detection is by CONFIG DIRECTORY rather than by a name on
// $PATH: a name on $PATH may be an entirely different program (`codex` in particular is an ordinary
// enough word for a collision to be real), while the config directory is created by the tool itself
// and is far better evidence.
//
// Only Claude Code is actionable today (see agentOverview); the rest are recorded so the table has
// somewhere obvious to grow and so nobody re-derives the paths later.
var codingAgents = []codingAgent{
	{Tool: "claude", Display: "Claude Code", ConfigDir: ".claude"},
	{Display: "Codex", ConfigDir: ".codex"},
	{Display: "Cursor", ConfigDir: ".cursor"},
	{Display: "Windsurf", ConfigDir: filepath.Join(".codeium", "windsurf")},
}

// detectCodingAgents returns the table rows whose config directory is present, in table order. A
// home directory that cannot be resolved detects nothing rather than failing: detection is an
// offer, and losing it must never fail the command it is attached to.
func detectCodingAgents() []codingAgent {
	home, err := agentHomeDir()
	if err != nil {
		return nil
	}
	var found []codingAgent
	for _, a := range codingAgents {
		if info, err := os.Stat(filepath.Join(home, a.ConfigDir)); err == nil && info.IsDir() {
			found = append(found, a)
		}
	}
	return found
}

// claudeCodeDetected reports whether Claude Code's config directory is present.
func claudeCodeDetected() bool {
	for _, a := range detectCodingAgents() {
		if a.Tool == "claude" {
			return true
		}
	}
	return false
}

// claudeCodeWired reports whether Claude Code already carries the permission rules that keep it on
// the guarded path. It asks the same helper `burrow agent claude` uses, so the two cannot disagree
// about what "wired" means. The kubectl deny is opt-in, so it is not part of the test.
func claudeCodeWired() bool {
	t := claudeAgentTool{}
	return claudePermsApplied(t.allowRules(), t.denyRules())
}

// agentUnwiredStatusLine is the one actionable line `burrow auth status` prints when a coding agent
// is present and not restricted to burrow-agent. It is one line and appears only there, not on
// every command: noise is what gets skimmed past, and status is where a person looks when asking
// whether their setup is right.
const agentUnwiredStatusLine = "Claude Code is installed but not restricted to burrow-agent. Wire it with:\n" +
	"  burrow agent claude install"

// agentNoneDetectedNotice is what `burrow auth login` prints when detection finds nothing to wire:
// what is supported, how to wire it later, and where to ask for another tool. It is a statement, not
// a question — a "which agent do you use?" picker would carry exactly one actionable entry today and
// would make people navigate past dead ends to reach it. The picker earns its place once there are
// two or more supported tools. No em-dashes: it is user-facing output.
const agentNoneDetectedNotice = "No coding agent was detected. Burrow has built-in wiring for Claude Code today; wire it any\n" +
	"time with `burrow agent claude install`.\n" +
	"Using another agent? Request support: " + agentIssuesURL + "\n"

// offerAgentWiring is the tail of a successful `burrow auth login`: it detects an installed coding
// agent and offers to restrict it to burrow-agent, defaulting to YES because that is the setup
// nearly everyone wants and the moment they are asking for it.
//
// When nothing is detected it states what is supported rather than asking an unanswerable question,
// then offers `Wire Claude Code anyway? (y/N)` defaulted to NO — the one case a picker was really
// for, a working install in a non-standard location that detection misses. It is worth one keystroke
// and does not require asking everyone a question most of them cannot usefully answer.
//
// Nothing here can fail the login that preceded it: a wiring error is reported and swallowed, since
// the target is already recorded and the install is already working.
func offerAgentWiring(p *prompter, out io.Writer, interactive bool) {
	fmt.Fprintln(out)
	if claudeCodeDetected() {
		offerDetectedClaude(p, out, interactive)
		return
	}
	// Name a detected tool Burrow cannot wire yet, so the notice below does not read as "we looked
	// and found nothing" to someone who plainly has one installed.
	for _, a := range detectCodingAgents() {
		if !a.wireable() {
			fmt.Fprintf(out, "Found %s, which Burrow has no built-in wiring for yet.\n", a.Display)
		}
	}
	fmt.Fprint(out, agentNoneDetectedNotice)
	if !interactive {
		return
	}
	yes, err := p.confirm("Wire Claude Code anyway? (y/N): ", false)
	if err != nil || !yes {
		return
	}
	wireClaude(out)
}

// offerDetectedClaude handles the detected-Claude-Code branch: already wired says so and asks
// nothing, a non-interactive run prints the pointer without prompting (an interactive command may
// reasonably offer and default to yes; a non-interactive one may only tell you), and an interactive
// run asks with yes as the default.
func offerDetectedClaude(p *prompter, out io.Writer, interactive bool) {
	if claudeCodeWired() {
		fmt.Fprintln(out, "Claude Code is already restricted to burrow-agent.")
		return
	}
	if !interactive {
		fmt.Fprintf(out, "Found Claude Code. Restrict it to the safe burrow-agent CLI with:\n  burrow agent claude install\n")
		return
	}
	yes, err := p.confirm("Found Claude Code. Restrict it to the safe burrow-agent CLI? (Y/n): ", true)
	if err != nil {
		fmt.Fprintf(out, "Could not read an answer, so nothing was changed. Wire it with `burrow agent claude install`.\n")
		return
	}
	if !yes {
		fmt.Fprintln(out, "Left Claude Code as it is. Wire it any time with `burrow agent claude install`.")
		return
	}
	wireClaude(out)
}

// wireClaude applies the Claude Code wiring and reports a failure without failing the caller. The
// login it follows has already succeeded and the target is already recorded, so a settings file that
// cannot be written is a thing to say, not a reason to unwind.
func wireClaude(out io.Writer) {
	if err := (claudeAgentTool{}).install(out); err != nil {
		fmt.Fprintf(out, "%scould not wire Claude Code: %v\nWire it later with `burrow agent claude install`.\n", warning(out), err)
	}
}
