// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The operator's two verbs (cloud ADR-0060). What is asserted here is the shape a person meets:
// which call each command makes, and that the unlock does not ask for a confirmation on top of
// itself.

// TestLockAndUnlockCallTheRightEndpoints: `lock` PUTs, `unlock` DELETEs, and the add-on form
// addresses the instance rather than the app path.
func TestLockAndUnlockCallTheRightEndpoints(t *testing.T) {
	cases := []struct {
		args   []string
		method string
		path   string
	}{
		{[]string{"lock", "web"}, http.MethodPut, "/v1/apps/web/lock"},
		{[]string{"unlock", "web"}, http.MethodDelete, "/v1/apps/web/lock"},
		{[]string{"lock", "addon", "burrow-postgres"}, http.MethodPut, "/v1/addons/burrow-postgres/lock"},
		{[]string{"unlock", "addon", "burrow-postgres"}, http.MethodDelete, "/v1/addons/burrow-postgres/lock"},
	}
	for _, tc := range cases {
		var gotMethod, gotPath string
		_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{
				"subject": "app", "name": "web", "environment": "prod", "locked": true, "changed": true,
			})
		}, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if gotMethod != tc.method || gotPath != tc.path {
			t.Errorf("%v sent %s %s, want %s %s", tc.args, gotMethod, gotPath, tc.method, tc.path)
		}
	}
}

// TestUnlockTakesNoConfirmFlag: the unlock IS the deliberate act, so it does not also ask to be
// confirmed. `--confirm` catches a command nobody read; this one has no purpose other than to permit
// destruction, and stacking a confirmation on it would make the pair ceremony — which is what gets
// switched off. Deleting a locked app still takes both an unlock and a confirmed delete.
func TestUnlockTakesNoConfirmFlag(t *testing.T) {
	for _, path := range [][]string{{"unlock"}, {"unlock", "addon"}, {"lock"}, {"lock", "addon"}} {
		cmd, _, err := newRootCmd().Find(path)
		if err != nil {
			t.Fatalf("finding %v: %v", path, err)
		}
		if f := cmd.Flags().Lookup("confirm"); f != nil {
			t.Errorf("`burrow %s` has a --confirm flag; the act itself is the confirmation", strings.Join(path, " "))
		}
	}
}

// TestLockedRefusalReachesThePerson: the control plane's refusal text, with the unlock command in
// it, is what the CLI prints. A client that summarized it into "request failed" would throw away the
// one sentence that says what to do next.
func TestLockedRefusalReachesThePerson(t *testing.T) {
	_, _, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": `app "web" in environment prod is locked, so deleting this app is refused. ` +
				"Remove it with `burrow unlock web --env prod`, then run this again.",
			"code": "locked",
		})
	}, "app", "delete", "web", "--confirm")
	if err == nil {
		t.Fatal("a locked refusal did not fail the command")
	}
	if !strings.Contains(err.Error(), "burrow unlock web --env prod") {
		t.Errorf("the error a person sees is %q; it does not name the unlock command", err)
	}
}
