// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/internal/agentsurface"
)

// The lock is an operator mechanism, and its absence from this binary is the whole of what makes it
// worth anything (cloud ADR-0060 §4). A lock's only property is that destroying something takes a
// SECOND deliberate act by a person; two acts by one caller in one loop are one act, so an agent
// that could unlock could delete.
//
// This is asserted here as well as by the closed allow-list because it is a claim about a property
// rather than about a list: the allow-list would also pass if somebody added `lock` to the catalogue
// as an Agent capability, which is exactly the change this test exists to fail.

// TestAgentCarriesNoLockVerb asserts neither verb is registered on this binary, in any spelling.
func TestAgentCarriesNoLockVerb(t *testing.T) {
	for _, path := range registeredCommandPaths() {
		if strings.Contains(path, "lock") {
			t.Errorf("burrow-agent registers %q. Neither `lock` nor `unlock` belongs on the agent "+
				"surface: the mechanism's only property is that a person performs a second deliberate "+
				"act before anything is destroyed, and an agent that can perform both acts has "+
				"performed one (cloud ADR-0060 §4).", path)
		}
	}
}

// TestLockAndUnlockAreReportedAsAbsent asserts the other half: the agent can find out what a lock is
// and who removes one. An absent verb that is also LEGIBLE is a refusal the agent relays to a person
// ("this is locked, and here is the command that unlocks it"); an absent verb that is invisible is a
// dead end, and a dead end is what pushes an agent off the control channel entirely (ADR-0065 §5).
func TestLockAndUnlockAreReportedAsAbsent(t *testing.T) {
	byPath := map[string]agentsurface.Capability{}
	for _, c := range absentCapabilities(newRootCmd()) {
		byPath[c.Path] = c
	}
	for _, path := range []string{"lock", "unlock"} {
		c, ok := byPath[path]
		if !ok {
			t.Errorf("`guard` does not report %q as absent; an agent meeting a locked refusal would "+
				"have no account of what stands in the way or who can remove it", path)
			continue
		}
		if c.What == "" || c.Why == "" {
			t.Errorf("absent capability %q has no what/why: %+v", path, c)
		}
		if !strings.HasPrefix(c.Command, "burrow "+path+" ") {
			t.Errorf("absent capability %q names the operator command %q, want a `burrow %s ...` invocation",
				path, c.Command, path)
		}
		// Who is filled in from the operator default when the entry does not state one, so an agent
		// always has somebody to hand the refusal to.
		if c.Who != agentsurface.WhoOperator {
			t.Errorf("absent capability %q names %q as who can perform it, want the operator default", path, c.Who)
		}
	}
}
