// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package agentsurface

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
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
		// A capability held back from the agent normally lives on the operator CLI, so its command
		// is a `burrow ...` invocation. The exception is a capability Burrow does not perform AT ALL
		// — bucket deletion is the first (ADR-0063 §5) — where the honest answer is the vendor's own
		// tool. Such an entry must say so in `who`, since the default operator answer would be a lie:
		// there is no burrow command to run.
		if c.Who == "" || c.Who == WhoOperator {
			if !strings.HasPrefix(c.Command, "burrow ") {
				t.Errorf("%q names operator command %q, want a `burrow ...` invocation (or an explicit "+
					"`who` naming whoever performs it instead)", c.Path, c.Command)
			}
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
	b, err := json.Marshal(NewGuardReport(client.GuardScope{}, nil, nil))
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
	// The global listing carries no scope: there is no tier to distinguish, so an object of blanks
	// would be noise on the answer every agent reads first.
	if _, ok := raw["scope"]; ok {
		t.Errorf("unscoped guard report carries a scope: %s", b)
	}

	b, err = json.Marshal(NewGuardReport(client.GuardScope{}, nil, AbsentFromAgentSurface()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"capability"`, `"what"`, `"why"`, `"who"`, `"operator_command"`, "addon remove"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("guard report JSON is missing %s: %s", want, b)
		}
	}

	// A scoped report says what it is about, so `"source":"name"` on an entry below is readable
	// without the reader knowing the arguments the call was made with (ADR-0085 §4).
	scoped := NewGuardReport(client.GuardScope{Env: "prod", Name: "website"},
		[]client.Guardrail{{Code: "app.run", Disposition: "deny", Source: "name"}}, nil)
	b, err = json.Marshal(scoped)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"scope"`, `"env":"prod"`, `"name":"website"`, `"source":"name"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("scoped guard report JSON is missing %s: %s", want, b)
		}
	}
}

// TestBucketDeletionIsHeldBackFromEveryBurrowSurface pins ADR-0063 §5's tier-1 half in the table
// that both readers derive from. Bucket deletion is the first capability Burrow performs NOWHERE:
// its blast radius is every backup the platform holds, and a bucket name lives in a global
// namespace, so a mistaken argument can reach past the cluster entirely. It is therefore held back
// from the agent binary AND absent from the operator CLI, and the answer to "who can" names the
// vendor rather than a burrow command that does not exist.
func TestBucketDeletionIsHeldBackFromEveryBurrowSurface(t *testing.T) {
	var got *Capability
	for _, c := range AbsentFromAgentSurface() {
		if c.Path == "bucket delete" {
			cap := c
			got = &cap
		}
	}
	if got == nil {
		t.Fatal("bucket deletion is not reported as absent; an agent asked to clean up storage would " +
			"hit `unknown command` with no account of who can do it (ADR-0065 §7)")
	}
	if got.Who == WhoOperator || !strings.Contains(strings.ToLower(got.Who), "vendor") {
		t.Errorf("bucket deletion reports who = %q; Burrow has no such command on either CLI, so the "+
			"answer must name the vendor", got.Who)
	}
	if !strings.Contains(strings.ToLower(got.Why), "global") {
		t.Errorf("the reason does not name the global bucket namespace, which is half of why this is "+
			"tier 1 rather than a guardrail: %q", got.Why)
	}
	if _, onAgent := AgentSurface()["bucket delete"]; onAgent {
		t.Error("bucket deletion is on the agent allow-list")
	}
}
