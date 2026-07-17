// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import "io"

// ANSI escape codes for the success/failure glyphs and the advisory label. Kept as small unexported
// constants so the style helpers stay dependency-free (no color module): raw escapes, applied only on
// a terminal.
const (
	ansiGreen = "\033[32m"
	ansiRed   = "\033[31m"
	ansiBold  = "\033[1m"
	ansiReset = "\033[0m"
)

// The glyphs Burrow marks ready/success and failure with: the U+2713 check and U+2717 cross, which
// align in a terminal better than the ✅/❌ emoji and are the CLI-standard. warnGlyph is the U+26A0
// warning sign that precedes an advisory Note/Warning label so a caution catches the eye alongside
// the ✓/✗ marks (issue #271).
const (
	okGlyph   = "✓"
	failGlyph = "✗"
	warnGlyph = "⚠️"
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

// note renders the label an advisory note carries, so a caution — a billable resource, an
// off-by-default notification, an ignored flag — stands out from ordinary output alongside the ✓/✗
// marks (issue #271). On a real terminal it is the ⚠️ warning sign and a bold "Note:"; off a terminal
// (piped output, logs, CI) it degrades to the plain "Note:" label with no emoji or escape codes, so
// nothing leaks into captured output. Gated on the isTerminal seam like okMark/failMark. Callers
// append the message text, e.g. fmt.Fprintln(w, note(w)+"metrics-server was not detected.").
func note(w io.Writer) string { return advisory(w, "Note:") }

// warning is note's louder sibling: the same ⚠️ marker with a bold "Warning:" label, for a recoverable
// problem a command chose to continue past (a credential it could not mint, an insecure flag) rather
// than a purely informational caution. Same TTY gating and plain-text fallback as note.
func warning(w io.Writer) string { return advisory(w, "Warning:") }

// advisory renders the shared advisory prefix for w — the ⚠️ marker and a bold label on a terminal,
// the plain label off one — with a trailing space so the caller's message follows directly.
func advisory(w io.Writer, label string) string {
	if isTerminal(w) {
		return warnGlyph + "  " + ansiBold + label + ansiReset + " "
	}
	return label + " "
}
