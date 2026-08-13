// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
)

// statementCmd is a bare command with a reader on stdin, for exercising readStatement without
// standing up the whole `addon sql` command.
func statementCmd(stdin string) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(stdin))
	return cmd
}

// TestReadStatementSources covers ADR-0087 §1's three ways of supplying a statement, and the one
// case that is refused rather than resolved: naming two of them. Which one won would be invisible in
// the output, and the output is a statement somebody ran against a live database.
func TestReadStatementSources(t *testing.T) {
	t.Run("-c wins when it is the only one", func(t *testing.T) {
		got, err := readStatement(statementCmd(""), "select 1", "")
		if err != nil || got != "select 1" {
			t.Fatalf("readStatement = (%q, %v)", got, err)
		}
	})

	t.Run("stdin when nothing else is named", func(t *testing.T) {
		got, err := readStatement(statementCmd("select 2\n"), "", "")
		if err != nil || strings.TrimSpace(got) != "select 2" {
			t.Fatalf("readStatement = (%q, %v)", got, err)
		}
	})

	t.Run("a file", func(t *testing.T) {
		path := t.TempDir() + "/q.sql"
		if err := os.WriteFile(path, []byte("select 3\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		got, err := readStatement(statementCmd(""), "", path)
		if err != nil || strings.TrimSpace(got) != "select 3" {
			t.Fatalf("readStatement = (%q, %v)", got, err)
		}
	})

	t.Run("-c and --file together are refused", func(t *testing.T) {
		if _, err := readStatement(statementCmd(""), "select 1", "/tmp/q.sql"); err == nil {
			t.Fatal("naming two sources was accepted; which one ran would be invisible")
		}
	})

	t.Run("nothing at all is refused rather than run", func(t *testing.T) {
		_, err := readStatement(statementCmd("   \n"), "", "")
		if err == nil {
			t.Fatal("an empty statement was accepted")
		}
		if !strings.Contains(err.Error(), "-c") {
			t.Errorf("error %q does not say how to supply a statement", err)
		}
	})

	t.Run("a terminal with no statement refuses instead of blocking", func(t *testing.T) {
		orig := stdinIsTerminal
		t.Cleanup(func() { stdinIsTerminal = orig })
		stdinIsTerminal = func(io.Reader) bool { return true }
		if _, err := readStatement(statementCmd(""), "", ""); err == nil {
			t.Fatal("a bare invocation on a terminal was accepted; it would have read stdin forever")
		}
	})
}

// TestFormatSQLResultTruncationIsLoud pins ADR-0087 §7's truncation on the surface a human reads: a
// short answer nobody was told about is the failure the whole shape exists to avoid, so the headline
// says it was cut off and the body names the limit to raise.
func TestFormatSQLResultTruncationIsLoud(t *testing.T) {
	out := formatSQLResult("web", client.SQLResult{
		Environment: "staging",
		Columns:     []string{"id"},
		Rows:        [][]*string{{ptr("1")}, {ptr("2")}},
		RowCount:    2,
		Truncated:   true,
		RowLimit:    2,
	}, false)
	head, _, _ := strings.Cut(out, "\n")
	if !strings.Contains(head, "cut off") {
		t.Errorf("headline %q does not say the result was cut off", head)
	}
	if !strings.Contains(out, "addon.sql_rows") {
		t.Errorf("output does not name the limit to raise:\n%s", out)
	}
	if !strings.Contains(out, "--env staging") {
		t.Errorf("the hint does not name the environment the result came from:\n%s", out)
	}
}

// TestFormatSQLResultNullIsNotEmpty asserts a NULL and an empty string render differently. They are
// different answers and the table has to say which.
func TestFormatSQLResultNullIsNotEmpty(t *testing.T) {
	out := formatSQLResult("web", client.SQLResult{
		Columns:  []string{"a", "b"},
		Rows:     [][]*string{{nil, ptr("")}},
		RowCount: 1,
	}, false)
	if !strings.Contains(out, "NULL") {
		t.Errorf("a NULL did not render as NULL:\n%s", out)
	}
}

// TestFormatSQLResultDatabaseErrorReadsAsAnOutcome asserts the database's own message and SQLSTATE
// are what a human sees, unmodified (ADR-0087 §4) — not a Burrow paraphrase, and not a CLI failure.
func TestFormatSQLResultDatabaseErrorReadsAsAnOutcome(t *testing.T) {
	out := formatSQLResult("web", client.SQLResult{
		Error: &client.SQLError{Message: `relation "users" does not exist`, SQLState: "42P01", Hint: "check the schema"},
	}, false)
	for _, want := range []string{"42P01", `relation "users" does not exist`, "check the schema"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not carry %q:\n%s", want, out)
		}
	}
}

// TestFormatSQLResultNoRowSet asserts a statement that returned no rows reports its command tag and
// what it changed, rather than an empty table standing in for "it worked".
func TestFormatSQLResultNoRowSet(t *testing.T) {
	out := formatSQLResult("web", client.SQLResult{Command: "UPDATE 3", RowsAffected: 3}, false)
	if !strings.Contains(out, "UPDATE 3") || !strings.Contains(out, "3 row(s) affected") {
		t.Errorf("output does not report the command tag and rows affected:\n%s", out)
	}
}

func ptr(s string) *string { return &s }
