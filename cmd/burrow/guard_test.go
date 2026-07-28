// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// cannedGuardrails answers /v1/guard with one disposition, so a test can assert what `guard list`
// adds around it.
func cannedGuardrails(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"guardrails": []map[string]any{
		{"code": "app.deploy", "disposition": "allow", "description": "deploy a new release of an application"},
	}})
}

// TestGuardListReportsAbsentCapabilities asserts the operator sees both halves of the boundary in
// one place ([ADR-0065](../../docs/adr/0065-what-belongs-on-the-agent-surface.md) §7): the
// dispositions this CLI can change with `guard set`, and the capabilities the agent binary does
// not carry at all, which it cannot. The second half is what a human is handed when the agent
// relays "removing an add-on is not something I can do".
func TestGuardListReportsAbsentCapabilities(t *testing.T) {
	out, _, err := runCLI(t, cannedGuardrails, "guard", "list")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "app.deploy") {
		t.Errorf("guard list dropped the dispositions: %q", out)
	}
	for _, want := range []string{"Absent from burrow-agent", "addon remove", "burrow addon remove <name>", "guard set"} {
		if !strings.Contains(out, want) {
			t.Errorf("guard list output is missing %q:\n%s", want, out)
		}
	}
}

// TestGuardListJSONSeparatesTheTwoGroups pins the machine-readable shape: a denied verb and an
// absent verb are different answers, so they are different keys. It is the same report
// `burrow-agent guard` prints, from the same catalogue.
func TestGuardListJSONSeparatesTheTwoGroups(t *testing.T) {
	out, _, err := runCLI(t, cannedGuardrails, "guard", "list", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var got agentsurface.GuardReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("guard list --json is not a guard report: %v\n%s", err, out)
	}
	if len(got.Guardrails) != 1 || got.Guardrails[0].Code != "app.deploy" {
		t.Errorf("guardrails = %+v, want the one canned disposition", got.Guardrails)
	}
	var found bool
	for _, c := range got.AbsentCapabilities {
		if c.Path == "addon remove" {
			found = true
			if c.Why == "" || c.Who == "" || c.Command == "" {
				t.Errorf("`addon remove` is reported without why/who/command: %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("absent_capabilities does not name `addon remove`: %s", out)
	}
}
