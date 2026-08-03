// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// The install-phase reporting shared by the `burrow cluster <component> install` commands: one
// aligned status line per component, and the condensed apply detail that goes on it. It lives apart
// from any one command because more than one command installs a cluster-wide component this way
// (ingress-nginx and cert-manager; the CloudNativePG operator), and their output should not drift.

// componentCol is the width the install-phase component names are padded to so their status text
// lines up in a scannable column. It fits the longest name ("ingress-nginx", "CloudNativePG").
const componentCol = len("ingress-nginx")

// installReporter prints the install phase: one aligned status line per component, marked with the
// success glyph. On a terminal it first prints a transient in-progress line that the final line
// overwrites, so a multi-minute readiness wait is not silent; on non-terminal (captured, piped, or
// verbose) output it prints only the final lines, keeping logs clean.
type installReporter struct {
	w       io.Writer
	verbose bool
}

// working prints a transient "<name> <verb>…" line while a component's apply or wait is in flight.
// It is a no-op in verbose mode and on non-terminal writers, where no carriage-return animation runs.
func (r installReporter) working(name, verb string) {
	if r.verbose || !isTerminal(r.w) {
		return
	}
	fmt.Fprintf(r.w, "\r\033[K  · %-*s  %s…", componentCol, name, verb)
}

// done prints a component's final aligned status line, clearing any transient line first on a terminal.
func (r installReporter) done(name, status string) {
	if !r.verbose && isTerminal(r.w) {
		fmt.Fprint(r.w, "\r\033[K")
	}
	fmt.Fprintf(r.w, "  %s %-*s  %s\n", okMark(r.w), componentCol, name, status)
}

// skipped reports a component that was NOT installed, with the reason. It is distinct from done
// because "installed" and "deliberately not installed" must not look the same on a setup command's
// output: a skipped component is something the operator now knows they do not have.
func (r installReporter) skipped(name, why string) {
	if !r.verbose && isTerminal(r.w) {
		fmt.Fprint(r.w, "\r\033[K")
	}
	fmt.Fprintf(r.w, "  - %-*s  skipped: %s\n", componentCol, name, why)
}

// planRow is one component in a `burrow cluster <component> install` plan: what is being installed,
// at which version, and one line on what it is for. It is shared for the same reason installReporter
// is — more than one command applies cluster-wide components this way, and their plans should not
// drift.
//
// url is the remote manifest the component is applied from, and it is PROVENANCE rather than
// decoration: applying a remote manifest with cluster-admin is a trust decision, and an operator who
// wants to read what will be fetched must be able to reach it. It does not belong on the component's
// own line, though — a 100-character URL after a colon is the longest thing on screen and pushes the
// version, the one value a reader scans for, off the end. So it sits indented beneath its component,
// and only when the caller asks for it (issue #461).
type planRow struct {
	name    string
	version string
	detail  string
	url     string
}

// label is the scannable left-hand column: the component and its version, which is what a reader is
// looking for. The version is omitted when there is nothing meaningful to state — a component that is
// already running and reports no readable version, or one whose pinned release is not being applied
// on this run.
func (r planRow) label() string {
	if r.version == "" {
		return r.name
	}
	return r.name + " " + r.version
}

// writePlanRows prints a plan's component list, aligning every detail into one column so the versions
// stack against each other. With showURLs, each component's manifest URL follows on its own indented
// line — the provenance, kept reachable without letting it lead.
func writePlanRows(w io.Writer, showURLs bool, rows []planRow) {
	width := 0
	for _, r := range rows {
		if n := len(r.label()); n > width {
			width = n
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  - %-*s   %s\n", width, r.label(), r.detail)
		if showURLs && r.url != "" {
			fmt.Fprintf(w, "      %s\n", r.url)
		}
	}
}

// parenthesize wraps a non-empty apply detail in " (...)" for a component status line, and is empty
// when there is no detail (e.g. verbose mode, where the per-resource listing is the detail).
func parenthesize(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

// applyURLFn is the remote-manifest apply seam, defaulting to the fetch-and-server-side-apply path.
// It is a package var so a test can assert WHICH upstream artifact an install applies without the
// fetch reaching the network — the URL is the pin, and a pin that is never asserted is a constant
// nobody notices moving.
var applyURLFn = serverSideApplyURL

// applyURLDetail applies a remote manifest for one install-phase component and returns the condensed
// "N created, M configured" detail for its status line. Verbose lists every applied resource to stdout
// and returns an empty detail (the listing is the detail); non-verbose captures the apply's one-line
// summary and condenses it, so the interleaved apply counter never reaches the terminal.
func applyURLDetail(ctx context.Context, kubeconfig, kubeContext, url string, verbose bool, stdout, stderr io.Writer) (string, error) {
	if verbose {
		return "", applyURLFn(ctx, kubeconfig, kubeContext, url, true, stdout, stderr)
	}
	var buf bytes.Buffer
	if err := applyURLFn(ctx, kubeconfig, kubeContext, url, false, &buf, stderr); err != nil {
		return "", err
	}
	return applyDetail(buf.String()), nil
}

// applyDetail condenses a captured non-verbose apply summary ("✓ Applied N resource(s): X created,
// Y configured.") into the parenthetical detail for a component status line ("X created, Y
// configured"). If the expected shape is absent it falls back to the trimmed text, so a formatting
// change upstream degrades to showing the raw summary rather than an empty detail.
func applyDetail(summary string) string {
	s := strings.TrimSpace(summary)
	const marker = "resource(s): "
	if i := strings.Index(s, marker); i >= 0 {
		return strings.TrimSuffix(strings.TrimSpace(s[i+len(marker):]), ".")
	}
	return s
}
