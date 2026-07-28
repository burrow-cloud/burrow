// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package agentsurface

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCatalogueEntriesAreComplete asserts every entry carries what a reader needs, because the
// point of reporting an absent capability is that the agent can EXPLAIN it (ADR-0065 §7). An entry
// with no `what` or no `who` degrades the report back to the dead end it exists to replace: the
// agent would learn only that something is missing, not what it was or who to ask.
func TestCatalogueEntriesAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range catalogue {
		if c.Path == "" {
			t.Fatalf("catalogue entry with empty path: %+v", c)
		}
		if seen[c.Path] {
			t.Errorf("catalogue lists %q twice; each capability appears once, tagged with the surface that carries it", c.Path)
		}
		seen[c.Path] = true
		if c.Surface != Agent && c.Surface != Operator {
			t.Errorf("%q has surface %q, want %q or %q", c.Path, c.Surface, Agent, Operator)
		}
		if strings.TrimSpace(c.What) == "" {
			t.Errorf("%q has no `what`; an agent relaying this refusal needs to say what the capability is", c.Path)
		}
		if c.Surface != Operator {
			continue
		}
		if strings.TrimSpace(c.Why) == "" {
			t.Errorf("%q is held back from the agent binary with no `why`; the reason is what makes the "+
				"refusal legible rather than arbitrary (ADR-0065 §7)", c.Path)
		}
		if strings.TrimSpace(c.Command) == "" {
			t.Errorf("%q has no operator command; \"here is who can\" needs something a human can actually run", c.Path)
		}
		if !strings.HasPrefix(c.Command, "burrow ") {
			t.Errorf("%q names operator command %q, want a `burrow ...` invocation", c.Path, c.Command)
		}
	}
}

// TestTier1CapabilitiesAreReported pins the capabilities ADR-0065 and issue #337 name explicitly.
// They are the cases that set the rule, so they are asserted by name rather than left to whatever
// the catalogue happens to contain.
func TestTier1CapabilitiesAreReported(t *testing.T) {
	want := []string{
		"addon remove",
		"addon remove --delete-data",
		"addon detach",
		"addon restore",
		"guard set",
		"secret set",
		"env add",
		"cluster install",
		"cluster upgrade",
		"cluster bootstrap",
		"join",
		"provider add",
		"registry login",
		"agent install",
	}
	got := map[string]Capability{}
	for _, c := range AbsentFromAgentSurface() {
		got[c.Path] = c
	}
	for _, path := range want {
		c, ok := got[path]
		if !ok {
			t.Errorf("%q is not reported as absent from the agent binary; an agent that tries it gets "+
				"`unknown command` and no account of who can run it (ADR-0065 §7)", path)
			continue
		}
		if c.Who != WhoOperator {
			t.Errorf("%q reports who = %q, want the operator answer %q", path, c.Who, WhoOperator)
		}
	}
}

// TestAbsentFromSubtractsRegisteredPaths is the property that keeps the report in step with the
// binary without a second edit: absence is computed by subtracting what a command tree actually
// registers, so a verb dropped from the binary starts being reported the moment it stops being
// registered.
func TestAbsentFromSubtractsRegisteredPaths(t *testing.T) {
	all := []string{}
	for _, c := range catalogue {
		all = append(all, c.Path)
	}
	if got := AbsentFrom(all); len(got) != 0 {
		t.Errorf("AbsentFrom(every path) reported %d absent capabilities, want none", len(got))
	}

	// Drop one agent capability, as if it had been taken out of the binary.
	var without []string
	for _, p := range all {
		if p != "addon backup" {
			without = append(without, p)
		}
	}
	var found *Capability
	for _, c := range AbsentFrom(without) {
		if c.Path == "addon backup" {
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatal("a capability the tree no longer registers was not reported as absent; the report would " +
			"go stale silently and the verb would be a dead end again")
	}
	if found.Who == "" {
		t.Error("an unregistered capability was reported with no `who`; every absent capability must name " +
			"who can perform it instead")
	}
}

// TestAgentSurfaceIsTheAgentHalf asserts the two halves of the catalogue partition it: what
// AgentSurface offers as the closed allow-list is exactly what AbsentFromAgentSurface does not
// report. One table, two readers, no overlap and no gap.
func TestAgentSurfaceIsTheAgentHalf(t *testing.T) {
	surface := AgentSurface()
	for _, c := range AbsentFromAgentSurface() {
		if _, ok := surface[c.Path]; ok {
			t.Errorf("%q is reported as absent from the agent binary and is also on its allow-list", c.Path)
		}
	}
	if len(surface)+len(AbsentFromAgentSurface()) != len(catalogue) {
		t.Errorf("allow-list (%d) + absent (%d) != catalogue (%d); every capability belongs to exactly one side",
			len(surface), len(AbsentFromAgentSurface()), len(catalogue))
	}
}

// TestGuardReportKeepsTheTwoGroupsApart asserts the JSON shape an agent branches on: a denied verb
// and an absent verb are different answers and must be distinguishable without parsing prose
// (ADR-0065 §7). It also pins that the absent list is always present, even when empty.
func TestGuardReportKeepsTheTwoGroupsApart(t *testing.T) {
	b, err := json.Marshal(NewGuardReport(nil, nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"guardrails", "absent_capabilities"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("guard report has no %q key: %s", key, b)
		}
	}
	if string(raw["absent_capabilities"]) != "[]" {
		t.Errorf("empty absent list encoded as %s, want [] (a missing key reads as \"unknown\" to an agent)",
			raw["absent_capabilities"])
	}

	b, err = json.Marshal(NewGuardReport(nil, AbsentFromAgentSurface()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"capability"`, `"what"`, `"why"`, `"who"`, `"operator_command"`, "addon remove"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("guard report JSON is missing %s: %s", want, b)
		}
	}
}
