// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// [ADR-0060](../../docs/adr/0060-cluster-lifecycle-command-group.md) moved install and upgrade
// under `burrow cluster`; the top-level spellings survive only as deprecated, hidden aliases that
// print a migration hint. So a message that tells someone to run `burrow upgrade` sends them at a
// command that answers with `Command "upgrade" is deprecated, use "burrow cluster upgrade".` —
// Burrow instructing a user to do a thing and then scolding them for doing it.
//
// The failure is invisible in review: each site is one plausible-looking backtick string, written
// correctly at the time, and nothing about it says the command moved. It reappeared across roughly
// a hundred sites in the tree for exactly that reason. So the surface is checked wholesale rather
// than site by site, in two directions.

// deprecatedSpelling matches the top-level spellings ADR-0060 retired. It cannot match
// `burrow cluster install` / `burrow cluster upgrade`: those have `cluster` where this wants the
// verb.
var deprecatedSpelling = regexp.MustCompile(`burrow (install|upgrade)\b`)

// TestHelpSurfaceNamesClusterSpelling walks the whole built command tree — every command's usage
// line, summary, long description, examples, deprecation notice, and every flag's help — and
// asserts none of it names a retired spelling. This is the surface a user actually reads, composed
// the way cobra composes it, so text inherited or assembled at construction time is covered rather
// than only the literals a source scan can see.
func TestHelpSurfaceNamesClusterSpelling(t *testing.T) {
	var walk func(cmd *cobra.Command, path string)
	check := func(where, text string) {
		if m := deprecatedSpelling.FindString(text); m != "" {
			t.Errorf("%s names the deprecated spelling %q; ADR-0060 moved it under `burrow cluster`\n%s", where, m, text)
		}
	}
	walk = func(cmd *cobra.Command, path string) {
		path = strings.TrimSpace(path + " " + cmd.Name())
		check(path+" (use)", cmd.Use)
		check(path+" (short)", cmd.Short)
		check(path+" (long)", cmd.Long)
		check(path+" (example)", cmd.Example)
		check(path+" (deprecated notice)", cmd.Deprecated)
		cmd.Flags().VisitAll(func(f *pflag.Flag) { check(path+" (--"+f.Name+")", f.Usage) })
		for _, sub := range cmd.Commands() {
			walk(sub, path)
		}
	}
	walk(newRootCmd(), "")
}

// TestNoDeprecatedSpellingInSourceStrings scans every non-test Go file in the module for a string
// literal naming a retired spelling. Help text is only part of the problem: the report that started
// this came from a runtime hint (`burrow version`), and the same wording lives in errors the control
// plane returns, in join output, and in the fail-closed messages `burrow-agent` prints — none of
// which the command tree above can reach.
//
// Comments are deliberately out of scope here: the alias itself has to be described somewhere, and
// `newInstallAliasCmd`, `newUpgradeAliasCmd`, and the root command's wiring all name the old
// spelling on purpose. What the scan CANNOT see is a message assembled from pieces
// (`"burrow " + verb`); that is the accepted gap, and the help-surface walk above covers the
// composed case for everything cobra renders.
func TestNoDeprecatedSpellingInSourceStrings(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "vendor" || name == "node_modules" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				s = lit.Value
			}
			if m := deprecatedSpelling.FindString(s); m != "" {
				t.Errorf("%s: string names the deprecated spelling %q; ADR-0060 moved it under `burrow cluster`\n\t%s",
					fset.Position(lit.Pos()), m, s)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning module source: %v", err)
	}
}
