// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runCLIPrompted is runCLIStdin with the terminal seam forced ON, which is the interactive path: no
// --stdin, the value typed rather than piped. The reader is still an in-memory one, so readToken
// takes its plain-read branch instead of the hidden-input one, which needs a real *os.File and so a
// real TTY. What is under test here is that the command asks for the value at all rather than taking
// it from argv; the echo-off reading itself belongs to readToken.
func runCLIPrompted(t *testing.T, typed string, h http.HandlerFunc, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	origTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = origTerm })

	srv := httptest.NewServer(h)
	defer srv.Close()
	var out, errb bytes.Buffer
	root := newRootCmd()
	root.SetArgs(append(append([]string{}, args...), "--control-plane", srv.URL, "--token", "x"))
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(typed))
	err = root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// TestSecretSet proves `secret set` carries the VALUE in the POST body to burrowd's
// control-plane API (ADR-0029) — not in the path or query, where the access log would see it —
// and that burrowd, not the CLI, writes the Secret. The value is typed at the prompt, which is the
// default path now that it has no argv form (issue #425).
func TestSecretSet(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	var gotBody map[string]any
	out, _, err := runCLIPrompted(t, "sk_test_x\n", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// The response carries the app and KEY only — never the value.
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "STRIPE_KEY"})
	}, "app", "secret", "set", "web", "STRIPE_KEY")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/apps/web/secrets" {
		t.Errorf("request = %s %s, want POST /v1/apps/web/secrets", gotMethod, gotPath)
	}
	// The value must be in the BODY, never the path or query (the access log records path+query).
	if strings.Contains(gotPath, "sk_test_x") || strings.Contains(gotQuery, "sk_test_x") {
		t.Errorf("value leaked into the request line: path=%q query=%q", gotPath, gotQuery)
	}
	if gotBody["key"] != "STRIPE_KEY" || gotBody["value"] != "sk_test_x" {
		t.Errorf("body = %#v, want key=STRIPE_KEY value=sk_test_x", gotBody)
	}
	if nr, _ := gotBody["no_restart"].(bool); nr {
		t.Errorf("no_restart = true, want false by default")
	}
	if !strings.Contains(out, "set secret STRIPE_KEY on web") {
		t.Errorf("output = %q", out)
	}
}

func TestSecretSetNoRestart(t *testing.T) {
	var gotBody map[string]any
	out, _, err := runCLIStdin(t, "V\n", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "K"})
	}, "app", "secret", "set", "web", "K", "--stdin", "--no-restart")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if nr, _ := gotBody["no_restart"].(bool); !nr {
		t.Errorf("no_restart = %#v, want true", gotBody["no_restart"])
	}
	if !strings.Contains(out, "lands on next deploy") {
		t.Errorf("output = %q, want a no-restart note", out)
	}
}

// TestSecretSetRoundTripsAMultiLineValue covers a PEM private key — the credential that made the
// argument form untenable rather than merely unsafe — through BOTH ways a value can arrive. Neither
// one puts it in argv, so neither writes it to shell history or shows it in `ps` (issue #425).
func TestSecretSetRoundTripsAMultiLineValue(t *testing.T) {
	const pem = "-----BEGIN PRIVATE KEY-----\nline1\nline2\n-----END PRIVATE KEY-----"
	handler := func(got *map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(got)
			_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "GITHUB_APP_KEY"})
		}
	}

	t.Run("piped", func(t *testing.T) {
		var gotBody map[string]any
		out, _, err := runCLIStdin(t, pem+"\n", handler(&gotBody),
			"app", "secret", "set", "web", "GITHUB_APP_KEY", "--stdin")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if gotBody["key"] != "GITHUB_APP_KEY" {
			t.Errorf("body = %#v, want the KEY taken from the argument", gotBody)
		}
		if gotBody["value"] != pem {
			t.Errorf("value = %q, want the multi-line value read from standard input", gotBody["value"])
		}
		if !strings.Contains(out, "set secret GITHUB_APP_KEY on web") {
			t.Errorf("output = %q", out)
		}
	})

	t.Run("prompted", func(t *testing.T) {
		var gotBody map[string]any
		if _, _, err := runCLIPrompted(t, pem+"\n", handler(&gotBody),
			"app", "secret", "set", "web", "GITHUB_APP_KEY"); err != nil {
			t.Fatalf("run: %v", err)
		}
		if gotBody["value"] != pem {
			t.Errorf("value = %q, want the multi-line value read at the prompt", gotBody["value"])
		}
	})
}

// TestSecretSetRefusesAValueInTheArgument is the security half of issue #425. `KEY=VALUE` used to be
// the ordinary way to call this, and it wrote the value into the shell's history file and into `ps`
// for every other user on the machine. It is not warned about and it is not quietly accepted as a
// key with an `=` in it: it is refused, before anything is sent, with the replacement in the message
// — because a warning leaves the value already written to the history file that the warning is
// about.
func TestSecretSetRefusesAValueInTheArgument(t *testing.T) {
	for _, tc := range []struct{ name, arg string }{
		{"plain", "STRIPE_KEY=sk_test_x"},
		// A DSN carries `=` inside the value, which is why splitting on it was ambiguous as well as
		// unsafe. It is refused the same way rather than split somewhere surprising.
		{"value containing =", "DATABASE_URL=postgres://u:p@h/db?opt=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runCLIPrompted(t, "typed\n", func(w http.ResponseWriter, _ *http.Request) {
				t.Error("the control plane was called for an invocation that put a value in argv")
				_ = json.NewEncoder(w).Encode(map[string]any{})
			}, "app", "secret", "set", "web", tc.arg)
			if err == nil {
				t.Fatal("KEY=VALUE succeeded; the value must have no argument form at all")
			}
			// The refusal has to be actionable: name the KEY-only spelling and the piped alternative,
			// and say why, or it reads as the CLI being difficult.
			for _, want := range []string{"shell history", "process table", "--stdin"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "sk_test_x") || strings.Contains(err.Error(), "postgres://u:p@h") {
				t.Errorf("the refusal echoed the value back: %v", err)
			}
		})
	}
}

// TestSecretSetNonInteractiveWithoutStdinFails is the acceptance the prompt would otherwise break: a
// CI job or a script has no terminal to prompt at, and a command that read standard input anyway
// would hang the build with no output. It fails instead, naming the flag that makes it work.
func TestSecretSetNonInteractiveWithoutStdinFails(t *testing.T) {
	_, _, err := runCLIStdin(t, "", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the control plane was called for an invalid invocation")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}, "app", "secret", "set", "web", "STRIPE_KEY")
	if err == nil || !strings.Contains(err.Error(), "--stdin") {
		t.Errorf("err = %v, want a refusal that points at --stdin", err)
	}
}

// TestSecretSetHelpWarnsTheAgentOff pins the one warning that must survive the help rewrite (issue
// #425 §3 cut the rest). It is the only place a person is told that pasting a value to an agent
// keeps it in the conversation and re-sends it on every later tool call — and the help must no
// longer teach the KEY=VALUE form it now refuses.
func TestSecretSetHelpWarnsTheAgentOff(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "secret", "set", "--help"}, &out, &errb); err != nil {
		t.Fatalf("secret set --help: %v", err)
	}
	help := out.String()
	for _, want := range []string{
		"NEVER paste a secret value into an agent prompt",
		"re-sent on every later tool call",
		"shell history",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("the help lost %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "KEY=VALUE") {
		t.Errorf("the help still teaches the form the command refuses:\n%s", help)
	}
}

func TestSecretListShowsKeysNotValues(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/apps/web/secrets" {
			t.Errorf("request = %s %s, want GET /v1/apps/web/secrets", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []string{"DATABASE_URL", "STRIPE_KEY"}})
	}, "app", "secret", "list", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "DATABASE_URL") || !strings.Contains(out, "STRIPE_KEY") {
		t.Errorf("output = %q, want the keys", out)
	}
}

func TestSecretUnset(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "STRIPE_KEY"})
	}, "app", "secret", "unset", "web", "STRIPE_KEY", "--no-restart")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/apps/web/secrets/STRIPE_KEY" {
		t.Errorf("request = %s %s, want DELETE /v1/apps/web/secrets/STRIPE_KEY", gotMethod, gotPath)
	}
	if gotQuery != "no_restart=true" {
		t.Errorf("query = %q, want no_restart=true", gotQuery)
	}
}
