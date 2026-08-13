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

// These tests cover ADR-0082's operator-CLI surface: the listing that says what an instance can be
// told, and the gate a reduction goes through.
//
// The one that carries the record is TestStandbyReductionNoticeNamesTheApps. §2 borrows ADR-0064
// §2's reasoning deliberately: a person about to break something should see WHAT, and "2 apps are
// affected" is a number to nod at where "api and web lose the read address" is a sentence that makes
// somebody stop.

// addonConfigServer serves an instance's current shape, the apps the notice enumerates, and records
// the configuration calls it receives — so a test can assert that an aborted confirmation never
// reaches the API at all.
func addonConfigServer(t *testing.T, standbys, storage string, attachedApps []string, calls *[]map[string]any) *httptest.Server {
	t.Helper()
	attached := map[string]bool{}
	for _, a := range attachedApps {
		attached[a] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/addons/settings":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"addon": "postgres", "environment": "prod", "instance": "burrow-postgres",
				"settings": []map[string]any{
					{"setting": "standbys", "value": standbys, "description": "standby pods beside the primary", "consequence": "adding the first one restarts every attached app"},
					{"setting": "storage", "value": storage, "description": "the data volume's size", "consequence": "CANNOT BE UNDONE"},
				},
			})
		case r.URL.Path == "/v1/addons/config":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*calls = append(*calls, body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"addon": "postgres", "environment": "prod", "instance": "burrow-postgres",
				"setting": body["setting"], "from": standbys, "to": body["value"], "changed": true,
				"read_address": map[string]any{"action": "withdrawn", "apps": attachedApps},
			})
		case r.URL.Path == "/v1/apps":
			rows := make([]map[string]any, 0, len(attachedApps))
			for _, a := range attachedApps {
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

// execAddonConfig drives one `addon config postgres …` invocation with an explicit stdin and
// interactive-terminal flag, returning stdout, stderr, and the RunE error.
func execAddonConfig(t *testing.T, baseURL, stdin string, terminal bool, args ...string) (string, string, error) {
	t.Helper()
	isolateConfig(t)
	origTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return terminal }
	t.Cleanup(func() { stdinIsTerminal = origTerm })

	var out, errb bytes.Buffer
	cmd := newAddonConfigCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append(args, "--control-plane", baseURL, "--token", "tok"))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// TestAddonConfigListsSettingsAndConsequences is ADR-0082 §1's bare listing. The consequence is on
// the page rather than in `--help`, because this is where somebody is looking at a current value and
// deciding whether to change it.
func TestAddonConfigListsSettingsAndConsequences(t *testing.T) {
	var calls []map[string]any
	srv := addonConfigServer(t, "0", "20Gi", nil, &calls)

	stdout, _, err := execAddonConfig(t, srv.URL, "", true, "postgres")
	if err != nil {
		t.Fatalf("addon config postgres: %v", err)
	}
	for _, want := range []string{"standbys", "storage", "20Gi", "burrow-postgres", "CANNOT BE UNDONE"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the listing does not mention %q:\n%s", want, stdout)
		}
	}
	if len(calls) != 0 {
		t.Errorf("the listing changed something: %v", calls)
	}
}

// TestStandbyReductionNoticeNamesTheApps: the gate prints what goes and asks for the instance's name,
// and a wrong answer changes nothing.
func TestStandbyReductionNoticeNamesTheApps(t *testing.T) {
	var calls []map[string]any
	srv := addonConfigServer(t, "1", "20Gi", []string{"api", "web"}, &calls)

	_, stderr, err := execAddonConfig(t, srv.URL, "no\n", true, "postgres", "standbys", "0")
	if err == nil {
		t.Fatal("a reduction proceeded on a wrong answer")
	}
	for _, want := range []string{"burrow-postgres", "api", "web", "read address"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the notice does not mention %q:\n%s", want, stderr)
		}
	}
	if len(calls) != 0 {
		t.Errorf("an aborted reduction still reached the control plane: %v", calls)
	}
}

// TestStandbyReductionProceedsOnTheTypedName: the gate is a gate, not a wall.
func TestStandbyReductionProceedsOnTheTypedName(t *testing.T) {
	var calls []map[string]any
	srv := addonConfigServer(t, "1", "20Gi", []string{"api"}, &calls)

	stdout, _, err := execAddonConfig(t, srv.URL, "burrow-postgres\n", true, "postgres", "standbys", "0")
	if err != nil {
		t.Fatalf("addon config postgres standbys 0: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want exactly one configuration call", calls)
	}
	// The typed name is the confirmation, so the call carries it forward — the server holds the
	// reduction too, and a client that satisfied its own prompt and not the server's would refuse a
	// change the operator had just approved.
	if calls[0]["confirm"] != true {
		t.Errorf("the confirmed reduction was sent unconfirmed: %v", calls[0])
	}
	if !strings.Contains(stdout, "withdrew the read address") {
		t.Errorf("the result does not say what happened to the read address:\n%s", stdout)
	}
}

// TestStandbyReductionRefusesOffATerminal: a script that never said it was taking capacity away
// cannot reach the change at all — the same posture `--delete-data` takes.
func TestStandbyReductionRefusesOffATerminal(t *testing.T) {
	var calls []map[string]any
	srv := addonConfigServer(t, "2", "20Gi", []string{"api"}, &calls)

	_, _, err := execAddonConfig(t, srv.URL, "", false, "postgres", "standbys", "1")
	if err == nil {
		t.Fatal("a reduction proceeded with no terminal to confirm at and no --confirm")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("the refusal names no way out: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("a refused reduction reached the control plane: %v", calls)
	}
}

// TestStandbyGrowthAsksNothing is the other half of ADR-0082 §2. Adding capacity breaks nothing that
// exists, so the prompt is absent rather than answered — and it stays absent off a terminal, where a
// prompt would be a refusal.
func TestStandbyGrowthAsksNothing(t *testing.T) {
	var calls []map[string]any
	srv := addonConfigServer(t, "0", "20Gi", []string{"api"}, &calls)

	_, stderr, err := execAddonConfig(t, srv.URL, "", false, "postgres", "standbys", "1")
	if err != nil {
		t.Fatalf("addon config postgres standbys 1: %v", err)
	}
	if len(calls) != 1 || calls[0]["confirm"] != false {
		t.Errorf("calls = %v, want one unconfirmed call: growing does not consult the confirmation", calls)
	}
	if strings.Contains(stderr, "Type the instance's name") {
		t.Errorf("growing printed a confirmation prompt:\n%s", stderr)
	}
}

// TestStorageGrowthAsksNothing: a volume shrink is a refusal the server makes, so the client has no
// prompt of its own to print for storage at all — including for a smaller size, which goes to the
// server to be refused with both sizes named rather than being second-guessed here.
func TestStorageGrowthAsksNothing(t *testing.T) {
	var calls []map[string]any
	srv := addonConfigServer(t, "0", "20Gi", nil, &calls)

	if _, stderr, err := execAddonConfig(t, srv.URL, "", false, "postgres", "storage", "50Gi"); err != nil {
		t.Fatalf("addon config postgres storage 50Gi: %v", err)
	} else if strings.Contains(stderr, "Type the instance's name") {
		t.Errorf("growing a volume printed a confirmation prompt:\n%s", stderr)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want one configuration call", calls)
	}
}

// TestAddonConfigIsAbsentFromTheAgentSurface is ADR-0082 §4. It is not merely missing from the agent
// binary: `guard` REPORTS it, with what it is and who runs it, because a verb that is absent and
// illegible is the dead end ADR-0021 says pushes an agent to reach for kubectl instead.
func TestAddonConfigIsAbsentFromTheAgentSurface(t *testing.T) {
	if _, onSurface := agentsurface.AgentSurface()["addon config"]; onSurface {
		t.Fatal("`addon config` is on the agent surface; ADR-0082 §4 keeps it off, because it provisions hardware")
	}
	for _, c := range agentsurface.AbsentFromAgentSurface(false) {
		if c.Path != "addon config" {
			continue
		}
		if c.Command == "" || c.Why == "" {
			t.Errorf("the absent capability says nothing an agent could relay: %+v", c)
		}
		return
	}
	t.Error("`addon config` is absent from the agent binary and not reported by `guard`, so the agent meets an unknown command with nothing to hand a human")
}
