// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package agentsurface

import (
	"strings"
	"testing"
)

// These tests hold the distinction issue #582 turns on, in the place the catalogue is built rather
// than in either of the two commands that print it.
//
// The catalogue answers two questions, and only one of them has the same answer on every target.
// WHAT the agent may not do, and WHY, is the shape of the `burrow-agent` binary: one binary, the same
// verbs held back for the same reasons, whichever kind of target it is pointed at. WHO performs it
// instead is a fact about the target, because about half the operator commands named here are refused
// to a managed tenant.
//
// Both halves need pinning, and the first is the one that is easy to lose: an implementation that
// merely dropped the rows a tenant cannot act on would leave a managed reader with a SHORTER list and
// the impression that the agent is held tighter on the managed product than on a cluster, which is
// false.

// TestTheAgentBoundaryDoesNotMoveWithTheTarget asserts the first half, member by member: the same
// capabilities, in the same order, with the same description and the same reason. Only Who and
// Command are allowed to differ.
func TestTheAgentBoundaryDoesNotMoveWithTheTarget(t *testing.T) {
	cluster, managed := AbsentFromAgentSurface(false), AbsentFromAgentSurface(true)
	if len(cluster) != len(managed) {
		t.Fatalf("the managed product reports %d absent capabilities and a cluster reports %d; the agent "+
			"surface is one binary and withholds the same capabilities on both", len(managed), len(cluster))
	}
	for i, c := range cluster {
		m := managed[i]
		switch {
		case c.Path != m.Path:
			t.Errorf("entry %d is %q on a cluster and %q on the managed product; the list and its order "+
				"are the binary's, not the target's", i, c.Path, m.Path)
		case c.What != m.What:
			t.Errorf("%q is described differently on the two targets:\n cluster: %s\n managed: %s",
				c.Path, c.What, m.What)
		case c.Why != m.Why:
			t.Errorf("%q is held back for a different reason on the two targets:\n cluster: %s\n managed: %s",
				c.Path, c.Why, m.Why)
		}
	}
}

// TestEveryAbsentCapabilityNamesSomebodyOnEitherTarget asserts the second half's floor. A row may
// name no command on the managed product — the platform performs it and there is no invocation to
// hand a tenant — but it may never name nobody. Who is what makes an absent verb a refusal the agent
// can relay rather than a dead end (ADR-0065 §5), and that is the same requirement on both targets.
func TestEveryAbsentCapabilityNamesSomebodyOnEitherTarget(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, c := range AbsentFromAgentSurface(managed) {
			if strings.TrimSpace(c.Who) == "" {
				t.Errorf("%q names nobody who can perform it (managed=%v); an absent capability with no "+
					"`who` is the dead end ADR-0065 §7 exists to prevent", c.Path, managed)
			}
		}
	}
}

// TestClusterCapabilitiesAreTheCatalogueUnchanged is the pin on the self-hosted answer: for every
// entry, what a cluster reports is exactly what the catalogue declares, with the WhoOperator default
// filled in as it always was. A managed remedy can therefore be added to any entry without touching
// what a self-hosted operator reads, and this test fails if one ever leaks across.
func TestClusterCapabilitiesAreTheCatalogueUnchanged(t *testing.T) {
	declared := map[string]Capability{}
	for _, c := range catalogue {
		declared[c.Path] = c
	}
	for _, c := range AbsentFromAgentSurface(false) {
		d := declared[c.Path]
		if c.Command != d.Command {
			t.Errorf("%q reports command %q on a cluster; the catalogue declares %q", c.Path, c.Command, d.Command)
		}
		want := d.Who
		if want == "" {
			want = WhoOperator
		}
		if c.Who != want {
			t.Errorf("%q reports who = %q on a cluster, want %q", c.Path, c.Who, want)
		}
		if c.Command == "" {
			t.Errorf("%q names no command on a cluster; every capability held back from the agent is an "+
				"operator's to run there, and the listing's second column is that command", c.Path)
		}
	}
}

// TestManagedRemediesAreDeclaredInPairs covers the catalogue's own consistency, since the two managed
// members are hand-written per entry. A managed COMMAND with no managed WHO would report a tenant's
// command beside the operator answer that a kubeconfig is needed to run it, which is the mismatch the
// whole change exists to remove; forTarget fills WhoTenant rather than leaving that, and this asserts
// no entry relies on the fallback by accident.
func TestManagedRemediesAreDeclaredInPairs(t *testing.T) {
	for _, c := range catalogue {
		if c.managedCommand != "" && c.managedWho == "" {
			t.Errorf("%q declares a managed command (%q) and no managed who; say who runs it", c.Path, c.managedCommand)
		}
		if c.Surface == Agent && (c.managedWho != "" || c.managedCommand != "") {
			t.Errorf("%q is on the agent surface and declares a managed remedy; a capability the agent "+
				"carries needs no answer to \"who does it instead\"", c.Path)
		}
	}
}

// TestManagedRemediesReplaceTheOperatorAnswerWhereverOneIsDeclared checks the substitution itself
// end to end for the three shapes an entry can take, named rather than derived so the intended
// classification is written down: a capability whose command carries over (`lock`), one whose write
// is refused but whose READ answers for a tenant (`cluster config set`), and one the platform simply
// performs (`addon remove`).
func TestManagedRemediesReplaceTheOperatorAnswerWhereverOneIsDeclared(t *testing.T) {
	byPath := map[string]Capability{}
	for _, c := range AbsentFromAgentSurface(true) {
		byPath[c.Path] = c
	}

	// Carries over: an app-scoped verb runs against either kind of target, so the command stands and
	// only the credential in the WHO changes.
	if got := byPath["lock"]; got.Command != "burrow lock <app>" || got.Who != WhoTenant {
		t.Errorf("`lock` on the managed product reports %q / %q, want the same command and the tenant answer %q",
			got.Command, got.Who, WhoTenant)
	}
	// Refused write, answering read: the value is the platform's to set and the tenant's to see.
	if got := byPath["cluster config set"]; !strings.Contains(got.Command, "burrow cluster config list") {
		t.Errorf("`cluster config set` on the managed product reports %q, want the listing that shows the "+
			"limits in force", got.Command)
	}
	// The platform performs it: no command, and the WHO is the whole answer.
	if got := byPath["addon remove"]; got.Command != "" || !strings.Contains(got.Who, "platform") {
		t.Errorf("`addon remove` on the managed product reports %q / %q, want no command and the platform "+
			"named as who operates the instance", got.Command, got.Who)
	}
}
