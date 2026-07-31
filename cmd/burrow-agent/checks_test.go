// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"strings"
	"testing"
)

// TestChecksIsReadOnlyOnTheAgentSurface is the omission stated as a test, because an omission is
// invisible otherwise. Reading what Burrow checks is a fact about the app; turning the check OFF is
// standing authority to stop verifying what Burrow handed it — the same class as a lifecycle hook or
// an auto-deploy level, both of which are operator-only. An agent that could silence a check it was
// failing would be an agent that could make its own work look correct.
func TestChecksIsReadOnlyOnTheAgentSurface(t *testing.T) {
	paths := commandPaths(newRootCmd())
	have := make(map[string]bool, len(paths))
	for _, p := range paths {
		have[p] = true
	}
	if !have["checks"] {
		t.Error("`checks` is not registered on the agent surface; an agent reading a failed dependency on a deploy has no way to ask what the check covers")
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "checks ") {
			t.Errorf("`%s` is on the agent surface; turning the deploy-time check off is an operator action", p)
		}
	}
}

// TestChecksGuidanceExplainsThatAFailureIsNotAFailedDeploy is the sentence that stops the most likely
// misreading. An agent that sees "failed" under a successful deploy will otherwise conclude something
// was rolled back, and nothing was.
func TestChecksGuidanceExplainsThatAFailureIsNotAFailedDeploy(t *testing.T) {
	for _, w := range []string{
		"NEVER FAILS THE DEPLOY",
		"does not roll back by itself",
		"DERIVED",
		"NOT a readiness probe",
		"blast radius",
	} {
		if !strings.Contains(checksLong, w) {
			t.Errorf("the checks guidance does not mention %q:\n%s", w, checksLong)
		}
	}
}
