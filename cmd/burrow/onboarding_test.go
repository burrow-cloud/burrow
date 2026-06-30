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

// TestHelpShowsCommandGroups confirms `burrow --help` renders the intent-based groups along the
// golden path, each with its commands (ADR-0037).
func TestHelpShowsCommandGroups(t *testing.T) {
	// A present config keeps the first-run banner out of the way so this asserts the groups only.
	cfgPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfgPath, []byte("apiVersion: burrow.dev/v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	t.Setenv("BURROW_CONFIG", cfgPath)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out, &errb); err != nil {
		t.Fatalf("--help: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Get started:", "install", "upgrade", "cluster", "config",
		"Environments:", "env",
		"Operate:", "app", "addon",
		"Govern:", "guard", "audit",
		"completion", "version",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("--help missing %q\n%s", want, s)
		}
	}
}

// TestFirstRunBannerWhenConfigAbsent confirms the help leads with the install banner when there
// is no client-side config (a brand-new user), and not once the config exists (ADR-0037).
func TestFirstRunBannerWhenConfigAbsent(t *testing.T) {
	const bannerLead = "Burrow is not set up yet."

	// Absent config: the banner leads.
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "missing"))
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out, &errb); err != nil {
		t.Fatalf("--help (absent config): %v", err)
	}
	if !strings.Contains(out.String(), bannerLead) {
		t.Errorf("first-run banner should appear when the config is absent\n%s", out.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), bannerLead) {
		t.Errorf("the banner should lead the help output\n%s", out.String())
	}

	// Present config: no banner.
	cfgPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfgPath, []byte("apiVersion: burrow.dev/v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	t.Setenv("BURROW_CONFIG", cfgPath)
	out.Reset()
	errb.Reset()
	if err := run(context.Background(), []string{"--help"}, &out, &errb); err != nil {
		t.Fatalf("--help (present config): %v", err)
	}
	if strings.Contains(out.String(), bannerLead) {
		t.Errorf("first-run banner should not appear once the config exists\n%s", out.String())
	}
}

// TestSubcommandHelpHasNoFirstRunBanner confirms the banner is scoped to the root help and does
// not leak into a subcommand's help even on first run.
func TestSubcommandHelpHasNoFirstRunBanner(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "missing"))
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "--help"}, &out, &errb); err != nil {
		t.Fatalf("app --help: %v", err)
	}
	if strings.Contains(out.String(), "Burrow is not set up yet.") {
		t.Errorf("subcommand help should not carry the first-run banner\n%s", out.String())
	}
}

// TestCompletionCommandExists confirms the built-in shell-completion command is present and emits
// a script for every supported shell (ADR-0037); the README documents the per-shell one-liners.
// Cobra registers the completion command during Execute, so it is driven through run rather than
// looked up on a freshly built command tree.
func TestCompletionCommandExists(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var out, errb bytes.Buffer
		if err := run(context.Background(), []string{"completion", shell}, &out, &errb); err != nil {
			t.Errorf("completion %s: %v\n%s", shell, err, errb.String())
		}
		if out.Len() == 0 {
			t.Errorf("completion %s produced no script", shell)
		}
	}
}
