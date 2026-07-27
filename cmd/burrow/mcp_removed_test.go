// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TEMPORARY — DELETE THIS FILE WITH mcp_removed.go AFTER THE NEXT RELEASE (ADR-0062 §2).

// TestMcpRedirectsToAgent pins the whole contract of the stub: every shape of the old command a
// pre-v0.12 guide might print must fail, and the failure must name `burrow agent <tool> install`
// rather than leaving the user with Cobra's "unknown command". The full old command line is
// included because it carries a flag the current surface does not have: the stub parses no flags
// precisely so that line reaches the message instead of dying on the unknown flag first.
func TestMcpRedirectsToAgent(t *testing.T) {
	configWithEnv(t)

	for _, args := range [][]string{
		{"mcp"},
		{"mcp", "claude"},
		{"mcp", "claude", "install"},
		{"mcp", "claude", "install", "--deny-kubectl"},
		{"mcp", "cursor", "install"},
	} {
		var out, errb bytes.Buffer
		err := run(context.Background(), args, &out, &errb)
		if err == nil {
			t.Errorf("%v: want an error, got success\n%s%s", args, out.String(), errb.String())
			continue
		}
		if !strings.Contains(err.Error(), "burrow agent claude install") {
			t.Errorf("%v: error does not name the replacement command: %v", args, err)
		}
		if !strings.Contains(err.Error(), "has been removed") {
			t.Errorf("%v: error does not say the command is gone: %v", args, err)
		}
	}
}

// TestMcpStubMutatesNothing confirms the stub is inert. The command it replaced wrote agent config
// files; someone re-running an old guide must not have anything happen to them beyond the message,
// so the stub holds no adapter, no file seam, and no install path at all.
func TestMcpStubMutatesNothing(t *testing.T) {
	settings := agentTempClaudeSettings(t)
	memory := agentTempClaudeMemory(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"mcp", "claude", "install"}, &out, &errb); err == nil {
		t.Fatal("want an error from the stub")
	}
	assertNoFile(t, settings)
	assertNoFile(t, memory)
}
