// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import "io"

// ANSI escape codes for the success/failure glyphs. Kept as small unexported constants so the
// style helpers stay dependency-free (no color module): raw escapes, applied only on a terminal.
const (
	ansiGreen = "\033[32m"
	ansiRed   = "\033[31m"
	ansiReset = "\033[0m"
)

// The glyphs Burrow marks ready/success and failure with: the U+2713 check and U+2717 cross, which
// align in a terminal better than the ✅/❌ emoji and are the CLI-standard.
const (
	okGlyph   = "✓"
	failGlyph = "✗"
)

// okMark renders the success glyph for w: a green ✓ when w is a real terminal, otherwise the plain
// glyph with no escape codes so piped output and logs stay clean. TTY detection reuses the
// isTerminal seam (apply.go), so the mark colors exactly when the progress animation does.
func okMark(w io.Writer) string {
	if isTerminal(w) {
		return ansiGreen + okGlyph + ansiReset
	}
	return okGlyph
}

// failMark renders the failure glyph for w: a red ✗ on a real terminal, otherwise the plain glyph
// with no escape codes, matching okMark's TTY gating.
func failMark(w io.Writer) string {
	if isTerminal(w) {
		return ansiRed + failGlyph + ansiReset
	}
	return failGlyph
}
