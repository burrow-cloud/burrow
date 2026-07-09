// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig points $BURROW_CONFIG at a temp file with the given YAML body, so tests exercise the
// local-config paths without touching the developer's real ~/.burrow/config.
func writeConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("BURROW_CONFIG", path)
}

// TestTruthy confirms BURROW_AGENT_REQUIRE_SCOPED truthiness parsing: 1/true/yes (case-insensitive,
// whitespace-trimmed) enable it; empty, 0, and anything else leave it off.
func TestTruthy(t *testing.T) {
	on := []string{"1", "true", "yes", "TRUE", "Yes", " true "}
	for _, v := range on {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false, want true", v)
		}
	}
	off := []string{"", "0", "false", "no", "off", "2", "enabled"}
	for _, v := range off {
		if truthy(v) {
			t.Errorf("truthy(%q) = true, want false", v)
		}
	}
}

// TestEmitJSON confirms emitJSON writes indented JSON.
func TestEmitJSON(t *testing.T) {
	var b bytes.Buffer
	if err := emitJSON(&b, map[string]string{"app": "web"}); err != nil {
		t.Fatalf("emitJSON: %v", err)
	}
	got := b.String()
	if !strings.Contains(got, "\n  \"app\": \"web\"") {
		t.Errorf("output = %q, want indented JSON", got)
	}
}

// TestBareRootPrintsOrientation confirms the bare invocation prints the agent orientation (the
// discovery surface, ADR-0049 §5) and does not error.
func TestBareRootPrintsOrientation(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(t.Context(), nil, &out, &errb); err != nil {
		t.Fatalf("run(): %v", err)
	}
	combined := out.String() + errb.String()
	if !strings.Contains(combined, "read-only control channel") {
		t.Errorf("bare output = %q, want the orientation text", combined)
	}
}
