// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// runAgentCapabilities runs `burrow agent capabilities` with no control plane and no cluster, which
// is half the point of the command: it reads a compiled-in catalogue, so it answers on a machine
// where nothing is installed. Any flag the other tests append would be a lie about what it needs.
func runAgentCapabilities(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(context.Background(), append([]string{"agent", "capabilities"}, args...), &out, &out); err != nil {
		t.Fatalf("agent capabilities %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// TestAgentCapabilitiesIsTheHomeOfTheAbsentList is the other half of issue #445. Taking the
// absent-capability table out of `guard list` is only a fix if the list still has somewhere to live:
// ADR-0065 §7 requires the boundary to be legible, so a removal with no home would have been a
// regression. This asserts the content that used to print under the dispositions, verbatim columns
// included, now prints here.
func TestAgentCapabilitiesIsTheHomeOfTheAbsentList(t *testing.T) {
	out := runAgentCapabilities(t)
	for _, want := range []string{
		"absent from burrow-agent entirely",
		"CAPABILITY",
		"RUN INSTEAD",
		"addon remove",
		"burrow addon remove <name>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("agent capabilities is missing %q:\n%s", want, out)
		}
	}
	// It answers about the shape of the agent binary, not about policy. A disposition has no place
	// here for the same reason the reverse was wrong.
	for _, unwanted := range []string{"GUARDRAIL", "DISPOSITION", "app.deploy"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("agent capabilities prints guardrail policy (%q), which is `guard list`'s answer:\n%s", unwanted, out)
		}
	}
}

// TestAgentCapabilitiesJSONMatchesTheGuardReportKey pins the machine-readable shape to the one
// `guard list --json` already uses. The list is the same list, so a consumer that learned the key
// from one answer reads the other without a second spelling of it.
func TestAgentCapabilitiesJSONMatchesTheGuardReportKey(t *testing.T) {
	out := runAgentCapabilities(t, "--json")
	var got agentsurface.AbsentCapabilitiesReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("agent capabilities --json is not an absent-capability report: %v\n%s", err, out)
	}
	if len(got.AbsentCapabilities) == 0 {
		t.Fatalf("absent_capabilities is empty:\n%s", out)
	}
	var found bool
	for _, c := range got.AbsentCapabilities {
		if c.Path == "addon remove" {
			found = true
			// The detail the two-column table leaves out is the reason --json exists: an agent
			// relaying a refusal needs what it is, why, and who can run it.
			if c.What == "" || c.Why == "" || c.Who == "" || c.Command == "" {
				t.Errorf("`addon remove` is reported without what/why/who/command: %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("absent_capabilities does not name `addon remove`:\n%s", out)
	}
}

// TestAgentCapabilitiesReportsTheSameListAsGuardJSON keeps the two answers from drifting. They are
// built from the same catalogue call today; this fails the moment one of them starts filtering,
// sorting, or enriching differently from the other.
func TestAgentCapabilitiesReportsTheSameListAsGuardJSON(t *testing.T) {
	var standalone agentsurface.AbsentCapabilitiesReport
	if err := json.Unmarshal([]byte(runAgentCapabilities(t, "--json")), &standalone); err != nil {
		t.Fatalf("agent capabilities --json: %v", err)
	}

	guardOut, _, err := runCLI(t, cannedGuardrails, "guard", "list", "--json")
	if err != nil {
		t.Fatalf("guard list --json: %v", err)
	}
	var report agentsurface.GuardReport
	if err := json.Unmarshal([]byte(guardOut), &report); err != nil {
		t.Fatalf("guard list --json: %v", err)
	}

	if len(standalone.AbsentCapabilities) != len(report.AbsentCapabilities) {
		t.Fatalf("agent capabilities reports %d capabilities, guard list --json reports %d",
			len(standalone.AbsentCapabilities), len(report.AbsentCapabilities))
	}
	for i, c := range standalone.AbsentCapabilities {
		if c != report.AbsentCapabilities[i] {
			t.Errorf("capability %d differs between the two answers:\n  agent capabilities: %+v\n  guard list --json:  %+v",
				i, c, report.AbsentCapabilities[i])
		}
	}
}

// TestAgentCapabilitiesDoesNotShadowToolWiring guards the one thing adding a subcommand to a command
// that also takes positional arguments could break: `burrow agent <tool>` must still reach the
// wiring, since Cobra only routes to a child when the first argument is that child's name.
func TestAgentCapabilitiesDoesNotShadowToolWiring(t *testing.T) {
	agentTempClaudeSettings(t)
	agentTempClaudeMemory(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"agent", "claude"}, &out, &out); err != nil {
		t.Fatalf("agent claude: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Wire Claude Code to burrow-agent") &&
		!strings.Contains(out.String(), "already wired") {
		t.Errorf("`burrow agent claude` no longer previews the wiring:\n%s", out.String())
	}
}

// TestWriteAbsentCapabilitiesSaysSoWhenEmpty covers the rendering a running catalogue cannot produce
// today: an empty list must state that nothing is absent rather than print nothing, since a silent
// command cannot be told from a broken one.
func TestWriteAbsentCapabilitiesSaysSoWhenEmpty(t *testing.T) {
	var b bytes.Buffer
	writeAbsentCapabilities(&b, nil, false)
	if !strings.Contains(b.String(), "No capabilities are absent from burrow-agent") {
		t.Errorf("an empty list renders as %q, want a sentence saying nothing is absent", b.String())
	}
	if strings.Contains(b.String(), "CAPABILITY") {
		t.Errorf("an empty list still prints a table header:\n%s", b.String())
	}
}

// The tests below are issue #582: the catalogue's RUN INSTEAD column is the largest concentration of
// operator commands a managed tenant can reach, and about half of them are refused to one. They reuse
// operatorCommands and assertNoOperatorCommand from targethints_test.go deliberately — that list is
// the project's one answer to "what does the managed product refuse", and a second list shaped
// differently would be worse than either.

// TestManagedCapabilityCatalogueNamesNoRefusedCommand is the rule, applied to every row at once. A
// remedy printed to a tenant must be something a tenant can carry out, whether it is a command or a
// sentence about who performs it.
func TestManagedCapabilityCatalogueNamesNoRefusedCommand(t *testing.T) {
	for _, c := range agentsurface.AbsentFromAgentSurface(true) {
		assertNoOperatorCommand(t, "the managed remedy for "+c.Path, c.Command)
		assertNoOperatorCommand(t, "the managed `who` for "+c.Path, c.Who)
	}
}

// TestManagedCapabilityListingSaysTheBoundaryIsUnchanged guards against the way this fix could
// mislead. The second column is reworded on the managed product and the first is not, and a reader
// who noticed only the rewording could conclude the agent is held tighter there. So the listing says
// outright that the list does not depend on the target, and only the remedy does.
func TestManagedCapabilityListingSaysTheBoundaryIsUnchanged(t *testing.T) {
	var b bytes.Buffer
	writeAbsentCapabilities(&b, agentsurface.AbsentFromAgentSurface(true), true)
	got := b.String()
	for _, want := range []string{
		"does not depend on your target",
		"same capabilities for the same reasons everywhere",
		"who performs each one instead",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the managed listing is missing %q, so a reworded column is all the reader sees:\n%s", want, got)
		}
	}
	// A row the platform performs names no command, so the column heads the answer rather than an
	// instruction to run one.
	if !strings.Contains(got, "INSTEAD") || strings.Contains(got, "RUN INSTEAD") {
		t.Errorf("the managed listing heads its second column RUN INSTEAD, over rows that name no command:\n%s", got)
	}
	if !strings.Contains(got, "the platform, which") {
		t.Errorf("no managed row names the platform as who performs it:\n%s", got)
	}
}

// TestSelfHostedCapabilityListingIsUnchanged is the other half of the guarantee, and the one worth
// being strict about: a self-hosted operator's catalogue does not change at all. Every row still
// prints the operator command it always did, under the heading it always had, with none of the
// managed prose anywhere in the output.
func TestSelfHostedCapabilityListingIsUnchanged(t *testing.T) {
	absent := agentsurface.AbsentFromAgentSurface(false)
	var b bytes.Buffer
	writeAbsentCapabilities(&b, absent, false)
	got := b.String()
	if !strings.Contains(got, "CAPABILITY") || !strings.Contains(got, "RUN INSTEAD") {
		t.Fatalf("the self-hosted listing lost its columns:\n%s", got)
	}
	for _, c := range absent {
		if !strings.Contains(got, c.Path) || !strings.Contains(got, c.Command) {
			t.Errorf("the self-hosted listing no longer carries %q -> %q:\n%s", c.Path, c.Command, got)
		}
	}
	for _, unwanted := range []string{"does not depend on your target", "the platform, which", "managed product"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the self-hosted listing carries managed wording (%q):\n%s", unwanted, got)
		}
	}
}

// TestAgentCapabilitiesOnCloudTargetIsWordedForATenant runs the command the way a tenant does, with
// the managed product as the active target. It is the wiring that is under test as much as the
// wording: the command connects to nothing, so the target kind is read from the selected target
// rather than recorded at a connection (targethints.go, recordSelectedTarget).
func TestAgentCapabilitiesOnCloudTargetIsWordedForATenant(t *testing.T) {
	signedInToCloud(t)

	out := runAgentCapabilities(t)
	assertNoOperatorCommand(t, "the capability listing on a managed target", out)
	for _, want := range []string{
		"absent from burrow-agent entirely",
		"does not depend on your target",
		"addon remove",
		"the platform, which provisions and operates the add-on instance",
		"burrow cluster config list",
		"burrow lock <app>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the managed capability listing is missing %q:\n%s", want, out)
		}
	}

	// --json carries the same substitution, since that is what burrow-agent's own report is compared
	// against and what an agent relays to the person in front of it.
	var report agentsurface.AbsentCapabilitiesReport
	if err := json.Unmarshal([]byte(runAgentCapabilities(t, "--json")), &report); err != nil {
		t.Fatalf("unmarshal the managed report: %v", err)
	}
	if len(report.AbsentCapabilities) == 0 {
		t.Fatal("the managed report lists no absent capabilities")
	}
	for _, c := range report.AbsentCapabilities {
		assertNoOperatorCommand(t, "the managed JSON remedy for "+c.Path, c.Command+" "+c.Who)
		if c.Who == "" {
			t.Errorf("%q is reported to a tenant with nobody named who can perform it", c.Path)
		}
	}
}

// TestAgentCapabilitiesWithNoCloudTargetKeepsTheOperatorCommands is the end-to-end counterpart: with
// no managed target selected the command answers exactly as it always has, which is what a
// self-hosted operator and a machine with nothing installed yet both get.
func TestAgentCapabilitiesWithNoCloudTargetKeepsTheOperatorCommands(t *testing.T) {
	tempConfig(t)

	out := runAgentCapabilities(t)
	for _, want := range []string{"RUN INSTEAD", "burrow addon remove <name>", "burrow cluster upgrade"} {
		if !strings.Contains(out, want) {
			t.Errorf("the self-hosted capability listing is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "does not depend on your target") {
		t.Errorf("the self-hosted listing carries the managed sentence:\n%s", out)
	}
}
