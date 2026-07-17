// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestMarksNonTTYAreClean asserts that against a non-terminal writer (a bytes.Buffer, what piped
// output, logs, and CI capture) okMark/failMark return the plain ✓/✗ glyph with no ANSI escape
// byte. Color must appear only on a real terminal, so it must never leak into captured output.
func TestMarksNonTTYAreClean(t *testing.T) {
	var buf bytes.Buffer
	cases := []struct {
		name  string
		got   string
		glyph string
	}{
		{"okMark", okMark(&buf), "✓"},
		{"failMark", failMark(&buf), "✗"},
	}
	for _, c := range cases {
		if !strings.Contains(c.got, c.glyph) {
			t.Errorf("%s(non-tty) = %q, want it to contain %q", c.name, c.got, c.glyph)
		}
		if strings.ContainsRune(c.got, '\x1b') {
			t.Errorf("%s(non-tty) = %q, must not contain an ANSI escape byte", c.name, c.got)
		}
		if c.got != c.glyph {
			t.Errorf("%s(non-tty) = %q, want exactly %q (no decoration off a terminal)", c.name, c.got, c.glyph)
		}
	}
}

// TestAdvisoryNonTTYIsPlainLabel asserts that off a terminal (a bytes.Buffer, what piped output and CI
// capture) note/warning degrade to exactly the plain "Note: " / "Warning: " label — no ⚠️ emoji and no
// ANSI escape byte — so the marker never leaks into captured output while the label still carries the
// meaning (issue #271).
func TestAdvisoryNonTTYIsPlainLabel(t *testing.T) {
	var buf bytes.Buffer
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"note", note(&buf), "Note: "},
		{"warning", warning(&buf), "Warning: "},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s(non-tty) = %q, want exactly %q (plain label off a terminal)", c.name, c.got, c.want)
		}
		if strings.ContainsRune(c.got, '\x1b') {
			t.Errorf("%s(non-tty) = %q, must not contain an ANSI escape byte", c.name, c.got)
		}
		if strings.Contains(c.got, warnGlyph) {
			t.Errorf("%s(non-tty) = %q, must not contain the ⚠️ marker off a terminal", c.name, c.got)
		}
	}
}

// TestAdvisoryComposesAheadOfMessage asserts the helper is a prefix a caller prepends to its message,
// so an advisory line reads "Note: <message>" — the shape the CLI prints and tests assert on.
func TestAdvisoryComposesAheadOfMessage(t *testing.T) {
	var buf bytes.Buffer
	got := note(&buf) + "metrics-server was not detected."
	if want := "Note: metrics-server was not detected."; got != want {
		t.Errorf("note(non-tty)+msg = %q, want %q", got, want)
	}
}

// TestAdvisoryMarkerDoesNotPolluteJSON guards the invariant that the ⚠️ marker belongs only on the
// human text stream: emit's --json path encodes the structured value and drops the human string, so a
// note built with a possibly-decorated marker can never reach JSON output.
func TestAdvisoryMarkerDoesNotPolluteJSON(t *testing.T) {
	var buf bytes.Buffer
	type payload struct {
		App string `json:"app"`
	}
	human := note(&buf) + "a caution the human sees but JSON must not."
	if err := emit(&buf, true, payload{App: "web"}, human); err != nil {
		t.Fatalf("emit(json) error: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, warnGlyph) || strings.Contains(out, "Note:") || strings.Contains(out, "caution") {
		t.Errorf("JSON output %q must carry none of the human advisory text", out)
	}
	var got payload
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("JSON output is not valid JSON: %v\n%s", err, out)
	}
	if got.App != "web" {
		t.Errorf("decoded app = %q, want %q", got.App, "web")
	}
}
