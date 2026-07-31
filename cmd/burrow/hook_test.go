// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHookSet asserts `burrow app hook set` puts the phase in the path and the argv in the body —
// one command with the phase named, rather than a command per phase (ADR-0072 §1). It calls run()
// directly rather than through runCLI, because the connection flags must sit before the --
// separator on a real command line.
func TestHookSet(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "environment": "prod", "phase": "pre-deploy",
			"command": []string{"./manage.py", "migrate"},
		})
	}))
	defer srv.Close()
	var outb, errb bytes.Buffer
	args := []string{"app", "hook", "set", "web", "--on", "pre-deploy",
		"--control-plane", srv.URL, "--token", "x", "--", "./manage.py", "migrate"}
	if err := run(context.Background(), args, &outb, &errb); err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	if gotMethod != "PUT" || gotPath != "/v1/apps/web/hooks/pre-deploy" {
		t.Errorf("request = %s %s, want PUT /v1/apps/web/hooks/pre-deploy", gotMethod, gotPath)
	}
	cmdArgs, ok := gotBody["command"].([]any)
	if !ok || len(cmdArgs) != 2 || cmdArgs[0] != "./manage.py" {
		t.Errorf("command in body = %#v, want the argv", gotBody["command"])
	}
	if out := outb.String(); !strings.Contains(out, "pre-deploy") || !strings.Contains(out, "./manage.py migrate") {
		t.Errorf("output = %q, want the phase and the command", out)
	}
}

// TestHookSetRequiresACommand asserts the CLI refuses before connecting when no command follows --.
func TestHookSetRequiresACommand(t *testing.T) {
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the control plane must not be called when no command is given")
	}, "app", "hook", "set", "web", "--on", "pre-deploy")
	if err == nil || !strings.Contains(err.Error(), "command is required after --") {
		t.Fatalf("err = %v, want a missing-command error", err)
	}
}

// TestHookSetRequiresAPhase asserts --on is required: a hook with no phase has no moment to run at,
// which is the one thing its name is supposed to say.
func TestHookSetRequiresAPhase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the control plane must not be called without a phase")
	}))
	defer srv.Close()
	var outb, errb bytes.Buffer
	args := []string{"app", "hook", "set", "web", "--control-plane", srv.URL, "--token", "x", "--", "./migrate"}
	if err := run(context.Background(), args, &outb, &errb); err == nil || !strings.Contains(err.Error(), "on") {
		t.Fatalf("err = %v, want the required --on flag to be reported", err)
	}
}

// TestHookList asserts the listing shows each configured phase with its command, and says plainly
// that nothing runs when there is none.
func TestHookList(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hooks": []map[string]any{
			{"app": "web", "environment": "prod", "phase": "pre-deploy", "command": []string{"./migrate", "up"}},
		}})
	}, "app", "hook", "list", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "pre-deploy") || !strings.Contains(out, "./migrate up") {
		t.Errorf("output = %q, want the phase and its command", out)
	}

	out, _, err = runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hooks": []map[string]any{}})
	}, "app", "hook", "list", "web")
	if err != nil {
		t.Fatalf("run (empty): %v", err)
	}
	if !strings.Contains(out, "no hooks set") {
		t.Errorf("empty output = %q, want it to say nothing runs", out)
	}
}

// TestHookUnset asserts unset deletes the phase's route and reports that the phase now runs nothing.
func TestHookUnset(t *testing.T) {
	var gotMethod, gotPath string
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "phase": "pre-rollback"})
	}, "app", "hook", "unset", "web", "--on", "pre-rollback")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/apps/web/hooks/pre-rollback" {
		t.Errorf("request = %s %s, want DELETE /v1/apps/web/hooks/pre-rollback", gotMethod, gotPath)
	}
	if !strings.Contains(out, "runs nothing") {
		t.Errorf("output = %q, want it to say the phase now runs nothing", out)
	}
}

// TestDeployReportsAFailedHook asserts a hook failure reaches the human as the deploy's failure,
// carrying the command's own output — not as a bare non-zero exit (ADR-0072 §3).
func TestDeployReportsAFailedHook(t *testing.T) {
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "deploy web: pre-deploy hook failed for web in prod: \"./migrate\" (image img:2): exit code 1. " +
				"The deploy did not happen and the running version is untouched.\noutput (combined stdout+stderr):\nrelation \"users\" does not exist\n",
			"code": "hook_failed",
		})
	}, "app", "deploy", "web", "--image", "img:2")
	if err == nil {
		t.Fatal("deploy succeeded, want the hook failure surfaced")
	}
	for _, want := range []string{"pre-deploy hook failed", "did not happen", "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err.Error(), want)
		}
	}
}
