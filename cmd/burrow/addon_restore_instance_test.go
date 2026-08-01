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

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// These tests cover ADR-0066 §4's operator-CLI surface: the typed confirmation, and what it shows.
//
// The one that matters most is TestRestoreInstanceNoticeNamesEveryApp. A COUNT IS NOT CONSENT — the
// blast radius of a physical restore is every app on the instance, and the notice has to list them,
// because the number is something to nod at and the names are what make somebody stop.

// restoreInstanceServer serves what the notice reads (the apps in the environment and their secret
// KEY names) and records the restore calls it receives, so a test can assert that a refused
// confirmation never reaches the API.
func restoreInstanceServer(t *testing.T, apps, attachedApps []string, calls *[]map[string]any) *httptest.Server {
	t.Helper()
	attached := map[string]bool{}
	for _, a := range attachedApps {
		attached[a] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/addons/restore-instance":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*calls = append(*calls, body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"addon": "postgres", "environment": "prod", "instance": "burrow-postgres",
				"target": "backup b1", "safety_backup": "b9",
				"apps": attachedApps, "reconnected": attachedApps,
			})
		case r.URL.Path == "/v1/apps":
			rows := make([]map[string]any, 0, len(apps))
			for _, a := range apps {
				rows = append(rows, map[string]any{"app": a})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apps": rows})
		case strings.HasSuffix(r.URL.Path, "/secrets"):
			app := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/apps/"), "/secrets")
			keys := []string{"API_KEY"}
			if attached[app] {
				keys = []string{"DATABASE_URL"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// execRestoreInstance drives `addon restore-instance` with an explicit stdin and interactive-terminal
// flag, returning stdout, stderr, and the RunE error.
func execRestoreInstance(t *testing.T, baseURL, stdin string, terminal bool, args ...string) (string, string, error) {
	t.Helper()
	isolateConfig(t)
	origTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return terminal }
	t.Cleanup(func() { stdinIsTerminal = origTerm })

	var out, errb bytes.Buffer
	cmd := newAddonRestoreInstanceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append(args, "--control-plane", baseURL, "--token", "tok"))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// TestRestoreInstanceNoticeNamesEveryApp asserts the typed confirmation in the shape ADR-0064 §2
// established for --delete-data: a warning-styled notice naming the instance, the point it goes back
// to, EVERY app on it, what happens to the current data, and the safety backup taken first — and it
// proceeds only once the instance's name is typed back.
func TestRestoreInstanceNoticeNamesEveryApp(t *testing.T) {
	var calls []map[string]any
	srv := restoreInstanceServer(t, []string{"api", "static", "web"}, []string{"api", "web"}, &calls)

	out, errb, err := execRestoreInstance(t, srv.URL, "burrow-postgres\n", true, "postgres", "--backup", "b1", "--confirm")
	if err != nil {
		t.Fatalf("addon restore-instance with the name typed back: %v (stderr: %s)", err, errb)
	}
	for _, want := range []string{
		"REWINDS the whole postgres instance \"burrow-postgres\"",
		"backup b1",
		// The apps BY NAME, not counted.
		"2 attached apps (api, web)",
		"All of them are rewound together",
		"data volume is DESTROYED",
		"physical backup of the current state is written to the object store FIRST",
		"Type the instance's name (burrow-postgres) to proceed",
	} {
		if !strings.Contains(errb, want) {
			t.Errorf("the restore-instance notice is missing %q:\n%s", want, errb)
		}
	}
	// The notice and the prompt go to stderr, so a --json run keeps a machine-readable stdout.
	if strings.Contains(out, "Type the instance's name") {
		t.Errorf("the typed-name prompt must not land on stdout:\n%s", out)
	}
	if len(calls) != 1 || calls[0]["backup"] != "b1" {
		t.Fatalf("expected one restore carrying the named backup, got %v", calls)
	}
}

// TestRestoreInstanceWrongNameRefuses: a typed name that is not the instance's aborts, says nothing
// was restored, and no restore reaches the API.
func TestRestoreInstanceWrongNameRefuses(t *testing.T) {
	var calls []map[string]any
	srv := restoreInstanceServer(t, []string{"web"}, []string{"web"}, &calls)

	for _, typed := range []string{"postgres\n", "\n", ""} { // a near-miss, an empty line, and EOF
		_, _, err := execRestoreInstance(t, srv.URL, typed, true, "postgres", "--latest", "--confirm")
		if err == nil {
			t.Fatalf("typing %q should abort the restore", typed)
		}
		if !strings.Contains(err.Error(), "nothing was restored") {
			t.Errorf("the abort should say nothing was restored, got: %v", err)
		}
	}
	if len(calls) != 0 {
		t.Errorf("an aborted restore must not reach the API, got %v", calls)
	}
}

// TestRestoreInstanceNonInteractiveRefuses is the other half of ADR-0064 §2's gate: with no terminal
// to type into it refuses rather than proceeding, and --confirm — which satisfies a guardrail — is
// deliberately not the acknowledgement.
func TestRestoreInstanceNonInteractiveRefuses(t *testing.T) {
	var calls []map[string]any
	srv := restoreInstanceServer(t, []string{"web"}, []string{"web"}, &calls)

	_, _, err := execRestoreInstance(t, srv.URL, "", false, "postgres", "--latest", "--confirm")
	if err == nil {
		t.Fatal("restore-instance without a terminal and without the acknowledgement flag must refuse")
	}
	for _, want := range []string{"--acknowledge-data-loss", "interactive terminal", "burrow-postgres", "burrow addon restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, err)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused restore must not reach the API: %v", calls)
	}
}

// TestRestoreInstanceNeedsExactlyOneTarget refuses zero and refuses several, in the CLI as well as on
// the server — so the typed-name prompt is never printed for an invocation that was going to be
// refused anyway. Asking someone to type a database's name and then telling them the flags were wrong
// is a bad way to spend the one moment they are reading carefully.
func TestRestoreInstanceNeedsExactlyOneTarget(t *testing.T) {
	var calls []map[string]any
	srv := restoreInstanceServer(t, []string{"web"}, []string{"web"}, &calls)

	for _, args := range [][]string{
		{"postgres", "--confirm"},
		{"postgres", "--latest", "--backup", "b1", "--confirm"},
	} {
		_, errb, err := execRestoreInstance(t, srv.URL, "burrow-postgres\n", true, args...)
		if err == nil {
			t.Fatalf("%v should be refused", args)
		}
		if strings.Contains(errb, "Type the instance's name") {
			t.Errorf("%v printed the typed-name prompt before refusing the flags:\n%s", args, errb)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused invocation must not reach the API: %v", calls)
	}
}

// TestRestoreInstanceIsAbsentFromTheAgentSurface pins ADR-0065 §2's tier 1 for this verb: it is not
// compiled into `burrow-agent`, and `guard` reports it with what it is and who can run it — so the
// agent relays a refusal it can explain rather than failing with `unknown command`.
//
// The structural absence is the boundary. The typed confirmation is a legibility device for humans
// and adds nothing against an agent, so this test exists to keep the absence from being relaxed on
// the grounds that "there's a confirmation anyway".
func TestRestoreInstanceIsAbsentFromTheAgentSurface(t *testing.T) {
	var found *agentsurface.Capability
	for _, c := range agentsurface.AbsentFromAgentSurface() {
		if c.Path == "addon restore-instance" {
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal("`addon restore-instance` is not reported as absent from burrow-agent")
	}
	if _, onSurface := agentsurface.AgentSurface()["addon restore-instance"]; onSurface {
		t.Fatal("`addon restore-instance` is on the agent surface: rewinding every app's database is not an agent operation")
	}
	if !strings.Contains(found.Why, "tier 1") {
		t.Errorf("the reason does not name the tier that holds it back: %q", found.Why)
	}
	if !strings.Contains(found.Command, "burrow addon restore-instance") {
		t.Errorf("the report does not hand a human a command to run: %q", found.Command)
	}
}

// TestRestoreInstanceRefusesBeforeThePromptOnBadInput keeps everything refusable without the control
// plane in front of the prompt. Asking somebody to type a database's name back and then telling them
// the add-on was wrong, or the timestamp unparseable, is a bad way to spend the one moment they are
// reading carefully.
func TestRestoreInstanceRefusesBeforeThePromptOnBadInput(t *testing.T) {
	var calls []map[string]any
	srv := restoreInstanceServer(t, []string{"web"}, []string{"web"}, &calls)

	for _, args := range [][]string{
		{"logs", "--latest", "--confirm"},                    // an add-on with no repository at all
		{"postgres", "--to-time", "yesterday", "--confirm"},  // not an instant
		{"postgres", "--to-time", "2026-08-01", "--confirm"}, // a date is not an RFC3339 instant
	} {
		_, errb, err := execRestoreInstance(t, srv.URL, "burrow-postgres\n", true, args...)
		if err == nil {
			t.Fatalf("%v should be refused", args)
		}
		if strings.Contains(errb, "Type the instance's name") {
			t.Errorf("%v printed the typed-name prompt before refusing:\n%s", args, errb)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused invocation must not reach the API: %v", calls)
	}
}
