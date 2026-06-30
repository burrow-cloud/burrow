// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/localconfig"
)

// configWithEnv points $BURROW_CONFIG at a temp file and writes one environment into it, so a test
// runs against an existing (non-first-run) config.
func configWithEnv(t *testing.T) {
	t.Helper()
	tempConfig(t)
	cfg := &localconfig.Config{Environments: []localconfig.Environment{{Name: "dev", Context: "dev"}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// noConfig points $BURROW_CONFIG at a path that does not exist, so the CLI sees a first-run user.
func noConfig(t *testing.T) {
	t.Helper()
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
}

// TestRootHelpShowsGroups confirms `burrow --help` renders the labeled command groups and lists no
// retired `system`/`context` command (ADR-0037).
func TestRootHelpShowsGroups(t *testing.T) {
	configWithEnv(t) // config present so the first-run banner is suppressed

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out, &errb); err != nil {
		t.Fatalf("help: %v\n%s", err, errb.String())
	}
	s := out.String() + errb.String()

	for _, want := range []string{
		"Get started:", "Environments:", "Operate:", "Govern:",
		"install", "upgrade", "cluster", "config", "env", "app", "addon", "guard", "audit",
		"version", "completion", "help",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("help missing %q\n%s", want, s)
		}
	}

	// The legacy system/context commands are gone: no command line should start with either.
	for _, line := range strings.Split(s, "\n") {
		if f := strings.Fields(line); len(f) > 0 && (f[0] == "system" || f[0] == "context") {
			t.Errorf("help lists a retired %q command: %q", f[0], line)
		}
	}
}

// TestFirstRunBannerWhenNoConfig confirms a first-time user (no config) gets the install banner
// ahead of both bare `burrow` and `burrow --help` (ADR-0037).
func TestFirstRunBannerWhenNoConfig(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}} {
		noConfig(t)
		var out, errb bytes.Buffer
		if err := run(context.Background(), args, &out, &errb); err != nil {
			t.Fatalf("burrow %v: %v\n%s", args, err, errb.String())
		}
		s := out.String() + errb.String()
		if !strings.Contains(s, "Burrow is not set up yet") || !strings.Contains(s, "burrow install") {
			t.Errorf("burrow %v: first-run banner missing\n%s", args, s)
		}
	}
}

// TestNoBannerWhenConfigExists confirms the banner is suppressed once a config exists, leaving the
// normal grouped help.
func TestNoBannerWhenConfigExists(t *testing.T) {
	configWithEnv(t)
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out, &errb); err != nil {
		t.Fatalf("help: %v", err)
	}
	s := out.String() + errb.String()
	if strings.Contains(s, "Burrow is not set up yet") {
		t.Errorf("banner should be suppressed once a config exists\n%s", s)
	}
	if !strings.Contains(s, "Get started:") {
		t.Errorf("normal grouped help should still render\n%s", s)
	}
}

// TestSubcommandHelpNoBanner confirms subcommand help never leads with the root first-run banner,
// even for a first-run user.
func TestSubcommandHelpNoBanner(t *testing.T) {
	noConfig(t)
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "--help"}, &out, &errb); err != nil {
		t.Fatalf("app --help: %v", err)
	}
	s := out.String() + errb.String()
	if strings.Contains(s, "Burrow is not set up yet") {
		t.Errorf("subcommand help should not show the root first-run banner\n%s", s)
	}
}

// TestCompletionCommandExists confirms Cobra's completion command is present and produces a script,
// for the shells the README documents (ADR-0037).
func TestCompletionCommandExists(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var out, errb bytes.Buffer
		if err := run(context.Background(), []string{"completion", shell}, &out, &errb); err != nil {
			t.Fatalf("completion %s: %v\n%s", shell, err, errb.String())
		}
		if out.Len() == 0 {
			t.Errorf("completion %s produced no script", shell)
		}
	}

	// It must also be visible (not hidden) in the help listing.
	configWithEnv(t)
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out, &errb); err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(out.String()+errb.String(), "completion") {
		t.Errorf("completion command should be visible in --help")
	}
}
