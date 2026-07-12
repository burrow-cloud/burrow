// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// `burrow agent <tool> install` is the human setup step that wires a coding agent to `burrow-agent`,
// the agent's scoped control channel (ADR-0049 §4). It is a subcommand of the HUMAN `burrow` admin
// CLI, run once by a person; it is distinct from `burrow-agent`, the agent's runtime binary. Its sole
// job is to write the agent's permission rules — allow the scoped `burrow-agent` binary, deny the
// human `burrow` admin CLI — plus a concise orientation so the agent knows how to use it. Like
// `burrow mcp` it previews by default and mutates only on `install`, applying idempotently and backing
// up any file it edits.

// claudeMemoryPath resolves Claude Code's user memory file (~/.claude/CLAUDE.md), where the wiring
// installs the burrow-agent orientation block that Claude Code loads into every session — the CLI
// analogue of the MCP server's always-loaded instructions (ADR-0049 §5). It is a package var so a
// test can point it at a temp dir and exercise the real create/merge/backup logic without touching
// the real home directory.
var claudeMemoryPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "CLAUDE.md"), nil
}

// claudeMemoryDisplay is the tilde form shown in help and messages, independent of where the seam
// resolves (a temp dir in tests), so the user always sees the familiar path.
const claudeMemoryDisplay = "~/.claude/CLAUDE.md"

// The exact Claude Code permission patterns the wiring writes. They are the security boundary between
// the agent and the cluster, so the strings are load-bearing and asserted verbatim in tests.
const (
	// agentAllowRule lets the agent run the scoped burrow-agent binary with any arguments. The space
	// before `*` is a word boundary, matching the shape of the deny rule below.
	agentAllowRule = "Bash(burrow-agent *)"
	// agentAllowBareRule lets the agent run a bare `burrow-agent` (no arguments) to read the
	// orientation, without a permission prompt. The bare invocation is read-only.
	agentAllowBareRule = "Bash(burrow-agent)"
	// agentDenyRule blocks the human `burrow` admin CLI in the agent's shell. The space before `*` is a
	// WORD BOUNDARY: it matches `burrow deploy …` but NOT `burrow-agent …` (there is no space after
	// `burrow` in `burrow-agent`, so the space cannot match). Deny takes absolute precedence over allow,
	// which is exactly why two distinct binaries are used — the allow of burrow-agent cannot carve a
	// hole in this deny. Every DANGEROUS `burrow` invocation takes arguments (`burrow install`,
	// `burrow guard set`, …), so this one rule covers them all. A bare `burrow` (no arguments) is
	// deliberately NOT denied: it only prints a help screen, and a no-wildcard exact rule like
	// `Bash(burrow)` is avoided because its match semantics against `burrow-agent` are undocumented and,
	// with deny taking precedence, a false prefix match would silently break the whole setup — an
	// unacceptable risk for blocking a harmless help screen.
	agentDenyRule = "Bash(burrow *)"
	// agentDenyKubectlRule blocks the agent from running kubectl directly, so every cluster change goes
	// through Burrow's guarded path rather than around it. Opt-in via --deny-kubectl: kubectl is a
	// general tool Burrow does not own, so denying it by default would be overreach.
	agentDenyKubectlRule = "Bash(kubectl *)"
)

// agentDenyKubectlRecommendation is printed after a wiring that did NOT pass --deny-kubectl: it nudges
// the user toward the fuller lockdown without forcing it. No em-dashes: it is user-facing output.
const agentDenyKubectlRecommendation = "Recommended: also block the agent from running kubectl directly so every cluster change stays on\n" +
	"Burrow's guarded, audited path. Enable it with:\n" +
	"  burrow agent claude install --deny-kubectl\n"

// agentOverview is what bare `burrow agent` prints: what it does, the supported tools, and how to
// preview then apply. No em-dashes: it is user-facing CLI output.
const agentOverview = "Wire your AI agent to burrow-agent, its scoped control channel to Burrow.\n\n" +
	"Supported tools:\n" +
	"  claude    Claude Code\n\n" +
	"Preview what will be added:\n" +
	"  burrow agent <tool>\n\n" +
	"Apply it:\n" +
	"  burrow agent <tool> install\n\n" +
	"burrow-agent is a single binary on the agent's PATH; this writes the agent's permission rules to\n" +
	"allow it and deny the human `burrow` admin CLI, keeping every cluster change on Burrow's guarded path.\n" +
	"Using another agent? Request support: " + mcpIssuesURL + "\n"

// agentUnknownToolMessage is printed for `burrow agent <tool>` when the tool has no built-in wiring:
// rather than error, it shows the exact rules to set by hand and invites a support request. The %q is
// the tool the user named; the rule placeholders follow. No em-dashes: it is user-facing output.
const agentUnknownToolMessage = "Burrow has no built-in agent wiring for %q yet.\n\n" +
	"burrow-agent is a single binary on the agent's PATH. To wire another agent by hand, set its\n" +
	"permission rules to ALLOW `%s` and DENY `%s` (and optionally `%s`), so the agent may run the\n" +
	"scoped binary but not the human `burrow` admin CLI.\n\n" +
	"Built-in wiring: claude.\n" +
	"Request support: " + mcpIssuesURL + "\n"

func newAgentCmd() *cobra.Command {
	var denyKubectl bool
	cmd := &cobra.Command{
		Use:   "agent [tool] [install]",
		Short: "Wire your AI agent to burrow-agent",
		Long: "Wire your AI agent to burrow-agent, the agent's scoped control channel to Burrow.\n\n" +
			"This is a human setup step you run once: it writes the agent's permission rules so the agent\n" +
			"may run the scoped `burrow-agent` binary but not the human `burrow` admin CLI (this command).\n" +
			"It is distinct from `burrow-agent` itself, the agent's runtime binary.\n\n" +
			"Preview what a tool needs with `burrow agent <tool>`, then apply it with `burrow agent <tool>\n" +
			"install`. The change is idempotent, so a second run is safe, and any file it edits is backed\n" +
			"up first. Supported tools: claude.",
		Example: "  # See what wiring Claude Code will add\n" +
			"  burrow agent claude\n\n" +
			"  # Apply it\n" +
			"  burrow agent claude install\n\n" +
			"  # Apply it and also block the agent from running kubectl directly\n" +
			"  burrow agent claude install --deny-kubectl",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgent(args, cmd.OutOrStdout(), denyKubectl)
		},
	}
	// --deny-kubectl additionally denies kubectl in the agent's shell so every cluster change goes
	// through Burrow's guarded, audited path. Opt-in: kubectl is a general tool Burrow does not own, so
	// blocking it by default would be overreach. No em-dashes in the help string: it is user-facing.
	cmd.Flags().BoolVar(&denyKubectl, "deny-kubectl", false,
		"Also deny kubectl in the agent's shell, keeping every cluster change on Burrow's guarded path")
	return cmd
}

// runAgent routes the positional args: none prints the overview, one previews the tool's wiring, and
// two applies it when the second arg is the literal `install`. It mutates nothing except on the
// two-arg install path. Only claude has a built-in adapter today; any other tool prints the
// wire-by-hand message.
func runAgent(args []string, w io.Writer, denyKubectl bool) error {
	if len(args) == 0 {
		fmt.Fprint(w, agentOverview)
		return nil
	}
	if args[0] != "claude" {
		fmt.Fprintf(w, agentUnknownToolMessage, args[0], agentAllowRule, agentDenyRule, agentDenyKubectlRule)
		return nil
	}
	tool := claudeAgentTool{denyKubectl: denyKubectl}
	if len(args) == 1 {
		fmt.Fprint(w, tool.preview())
		return nil
	}
	if args[1] != "install" {
		return fmt.Errorf("unknown argument %q; to apply, run `burrow agent %s install`", args[1], args[0])
	}
	return tool.install(w)
}

// claudeAgentTool wires Claude Code to burrow-agent: it merges the allow/deny permission rules into
// ~/.claude/settings.json and installs the orientation block into ~/.claude/CLAUDE.md. Both steps are
// idempotent and preserve every other setting; the file each edits is backed up first.
type claudeAgentTool struct {
	denyKubectl bool // whether the deny set also blocks kubectl (opt-in via --deny-kubectl)
}

// allowRules is the set of allow rules the wiring writes: the agent may run burrow-agent, bare or with
// arguments. Order is stable so appended rules read predictably.
func (t claudeAgentTool) allowRules() []string {
	return []string{agentAllowRule, agentAllowBareRule}
}

// denyRules is the set of deny rules the wiring writes: always the word-boundary burrow-CLI rule
// (which covers every dangerous, argument-taking `burrow` invocation while sparing burrow-agent), plus
// the kubectl rule when --deny-kubectl was passed. A bare `burrow` (help screen only) is intentionally
// left un-denied; see agentDenyRule for why a no-wildcard exact rule is avoided. Order is stable so
// appended rules read predictably.
func (t claudeAgentTool) denyRules() []string {
	rules := []string{agentDenyRule}
	if t.denyKubectl {
		rules = append(rules, agentDenyKubectlRule)
	}
	return rules
}

func (t claudeAgentTool) preview() string {
	permsDone := claudePermsApplied(t.allowRules(), t.denyRules())
	instrDone := claudeInstructionsApplied()
	if permsDone && instrDone {
		return "Claude Code is already wired to burrow-agent. Nothing to do.\n"
	}

	var b strings.Builder
	b.WriteString("Wire Claude Code to burrow-agent.\n\n")
	fmt.Fprintf(&b, "This will add to %s:\n", claudeSettingsDisplay)
	b.WriteString("  allow:\n")
	for _, r := range t.allowRules() {
		fmt.Fprintf(&b, "    %s\n", r)
	}
	b.WriteString("  deny:\n")
	for _, r := range t.denyRules() {
		fmt.Fprintf(&b, "    %s\n", r)
	}
	b.WriteString("\nThe allow rule lets the agent run the scoped burrow-agent binary; the deny rule blocks the\n")
	b.WriteString("human `burrow` admin CLI (`burrow <args>`; the space is a word boundary, so burrow-agent is\n")
	b.WriteString("spared)")
	if t.denyKubectl {
		b.WriteString(" and kubectl")
	}
	b.WriteString(", so every cluster change stays on Burrow's guarded, audited path.\n")
	if !t.denyKubectl {
		b.WriteString("\nRecommended: also pass --deny-kubectl to block the agent from running kubectl directly.\n")
	}
	fmt.Fprintf(&b, "\nIt will also add a burrow-agent orientation block to %s so the agent knows how to use it\n", claudeMemoryDisplay)
	b.WriteString("(JSON output, the outcome envelope, and the confirm flow).\n")
	b.WriteString("\nYour other settings are preserved, and any file it edits is backed up first.\n")
	b.WriteString("\nRun `burrow agent claude install` to apply.\n")
	return b.String()
}

func (t claudeAgentTool) install(w io.Writer) error {
	permsChanged, err := ensureClaudePermissionRules(t.allowRules(), t.denyRules())
	if err != nil {
		return err
	}
	instrChanged, err := ensureClaudeAgentInstructions()
	if err != nil {
		return err
	}

	if !permsChanged && !instrChanged {
		fmt.Fprint(w, "Claude Code is already wired to burrow-agent. Nothing to do.\n")
		return nil
	}

	if permsChanged {
		if t.denyKubectl {
			fmt.Fprintf(w, "Wired Claude Code to burrow-agent: allowed %s and denied the burrow admin CLI (%s) "+
				"and kubectl (%s) in %s.\n", agentAllowRule, agentDenyRule, agentDenyKubectlRule, claudeSettingsDisplay)
		} else {
			fmt.Fprintf(w, "Wired Claude Code to burrow-agent: allowed %s and denied the burrow admin CLI (%s) "+
				"in %s.\n", agentAllowRule, agentDenyRule, claudeSettingsDisplay)
		}
	}
	if instrChanged {
		fmt.Fprintf(w, "Added a burrow-agent orientation block to %s.\n", claudeMemoryDisplay)
	}
	if permsChanged && !t.denyKubectl {
		// Denied only the burrow CLI: nudge the user toward the fuller lockdown they did not opt into.
		fmt.Fprint(w, agentDenyKubectlRecommendation)
	}
	fmt.Fprint(w, mcpTryPrompt)
	return nil
}

// --- settings.json: the allow/deny permission rules ---

// claudePermsApplied reports whether ~/.claude/settings.json already lists every allow rule under
// permissions.allow and every deny rule under permissions.deny.
func claudePermsApplied(allow, deny []string) bool {
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
	perms, _ := root["permissions"].(map[string]any)
	return ruleListContains(perms, "allow", allow) && ruleListContains(perms, "deny", deny)
}

// ruleListContains reports whether perms[key] (a JSON array of rule strings) already contains every
// rule in rules.
func ruleListContains(perms map[string]any, key string, rules []string) bool {
	have := ruleSet(perms, key)
	for _, rule := range rules {
		if !have[rule] {
			return false
		}
	}
	return true
}

// ruleSet reads perms[key] into a set of the rule strings present, so membership tests do not rescan
// the slice.
func ruleSet(perms map[string]any, key string) map[string]bool {
	list, _ := perms[key].([]any)
	set := make(map[string]bool, len(list))
	for _, r := range list {
		if s, ok := r.(string); ok {
			set[s] = true
		}
	}
	return set
}

// ensureClaudePermissionRules merges the allow and deny rules into ~/.claude/settings.json's
// permissions.allow and permissions.deny, preserving every other key (unrelated permissions, other
// rules, unrelated settings) and backing up an existing file once before it writes. It adds only the
// rules not already present (so no rule is duplicated) and reports whether it changed anything,
// returning false (and writing nothing) when every rule is already there, so a repeat run is
// idempotent.
func ensureClaudePermissionRules(allow, deny []string) (bool, error) {
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

	perms, _ := root["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	allowChanged := mergeRuleList(perms, "allow", allow)
	denyChanged := mergeRuleList(perms, "deny", deny)
	if !allowChanged && !denyChanged {
		return false, nil
	}
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

// mergeRuleList adds each rule not already present in perms[key] (a JSON array of strings), preserving
// order and existing entries, and reports whether it added anything.
func mergeRuleList(perms map[string]any, key string, rules []string) bool {
	list, _ := perms[key].([]any)
	have := make(map[string]bool, len(list))
	for _, r := range list {
		if s, ok := r.(string); ok {
			have[s] = true
		}
	}
	changed := false
	for _, rule := range rules {
		if !have[rule] {
			list = append(list, rule)
			have[rule] = true
			changed = true
		}
	}
	if changed {
		perms[key] = list
	}
	return changed
}

// --- CLAUDE.md: the always-loaded orientation block (ADR-0049 §5) ---

// The markers that fence the managed orientation block in ~/.claude/CLAUDE.md, so a re-run refreshes
// exactly this region and leaves the user's own memory untouched.
const (
	agentInstructionsBegin = "<!-- BEGIN burrow-agent (managed by `burrow agent claude install`) -->"
	agentInstructionsEnd   = "<!-- END burrow-agent -->"
)

// agentInstructionsBody is the orientation the agent reads on every session: what burrow-agent is,
// that output is JSON, that code never crosses the channel, and the outcome/confirm contract. It is
// the CLI analogue of the MCP server's always-loaded instructions (ADR-0049 §5). It stays concise and
// honest (ADR-0009): no invented commands, no secret values. No em-dashes: it is user-facing prose.
const agentInstructionsBody = "## Burrow\n\n" +
	"Operate the user's apps on their Kubernetes cluster through the `burrow-agent` CLI, your scoped\n" +
	"control channel to Burrow. Run `burrow-agent` with no arguments for orientation, or `-h` on any\n" +
	"command. Do NOT run the `burrow` CLI: it is the human's admin tool and is denied.\n\n" +
	"- Every command prints JSON, so pipe, grep, and jq it (e.g. `burrow-agent logs web | jq '.lines'`).\n" +
	"- Code never travels over the control channel: build and push a container image with your own\n" +
	"  tooling, then deploy by image reference; only the reference and small metadata cross.\n" +
	"- Tag releases `major.minor.patch` (e.g. 1.4.2), never a bare git SHA or `latest`: semver is what\n" +
	"  unlocks safe auto-updates. Run `burrow-agent next-tag <app>` for the next tag to apply.\n" +
	"- A mutating verb prints an `outcome` envelope: executed, held_for_confirmation, denied, or error.\n" +
	"  When held_for_confirmation, relay it to the human and re-run with --confirm ONLY after they\n" +
	"  explicitly approve. NEVER self-confirm."

// agentInstructionsBlock is the full fenced block written to CLAUDE.md.
func agentInstructionsBlock() string {
	return agentInstructionsBegin + "\n" + agentInstructionsBody + "\n" + agentInstructionsEnd
}

// claudeInstructionsApplied reports whether ~/.claude/CLAUDE.md already contains the current
// orientation block verbatim.
func claudeInstructionsApplied() bool {
	path, err := claudeMemoryPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), agentInstructionsBlock())
}

// ensureClaudeAgentInstructions upserts the orientation block into ~/.claude/CLAUDE.md, preserving the
// user's own memory around it and backing up an existing file once before it writes. It refreshes the
// fenced region if the markers are present (so a re-run updates stale text) and appends it otherwise;
// it reports whether it changed anything, so a repeat run with identical content is a no-op.
func ensureClaudeAgentInstructions() (bool, error) {
	path, err := claudeMemoryPath()
	if err != nil {
		return false, err
	}
	content := ""
	existed := false
	if data, err := os.ReadFile(path); err == nil {
		existed = true
		content = string(data)
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("reading %s: %w", claudeMemoryDisplay, err)
	}

	next, changed := upsertInstructionsBlock(content, agentInstructionsBlock())
	if !changed {
		return false, nil
	}

	if existed {
		if err := backupFile(path); err != nil {
			return false, err
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", claudeMemoryDisplay, err)
	}
	return true, nil
}

// upsertInstructionsBlock replaces the fenced block between the markers with block if the markers are
// present, or appends block (separated by a blank line) otherwise, and reports whether the content
// changed. An already-current block is left untouched (no change), keeping the write idempotent.
func upsertInstructionsBlock(content, block string) (string, bool) {
	begin := strings.Index(content, agentInstructionsBegin)
	if begin >= 0 {
		if rel := strings.Index(content[begin:], agentInstructionsEnd); rel >= 0 {
			end := begin + rel + len(agentInstructionsEnd)
			if content[begin:end] == block {
				return content, false
			}
			return content[:begin] + block + content[end:], true
		}
	}
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return block + "\n", true
	}
	return trimmed + "\n\n" + block + "\n", true
}
