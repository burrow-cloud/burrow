// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
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
