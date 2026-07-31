// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"strings"
	"testing"
)

// TestHealthGuidanceIsOnTheAgentSurface is ADR-0076 §5's acceptance criterion, asserted rather than
// assumed. Burrow cannot write a health endpoint into a user's application; the agent can, and the
// only thing that makes it happen is this text arriving before the agent acts.
//
// The dependency warning is asserted separately because it is the half that goes wrong. Checking
// the database from /healthz is the internet's most common example, and it is precisely the thing
// ADR-0076 §2 forbids here: one shared Postgres backs every app in an environment, so a readiness
// endpoint that touched it would take every replica of every app out of service on one blip.
func TestHealthGuidanceIsOnTheAgentSurface(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"health", healthLong},
	}
	want := []string{
		"readiness probe",
		"NOT check",
		"database",
		"shared",
		"liveness probe",
		"a few lines",
	}
	for _, c := range cases {
		for _, w := range want {
			if !strings.Contains(c.text, w) {
				t.Errorf("%s guidance does not mention %q:\n%s", c.name, w, c.text)
			}
		}
	}
}

// TestHealthVerbsAreRegistered proves the guidance is not advice with no verb behind it: an agent
// told to add a health endpoint can also register it, without handing the user a command to run.
func TestHealthVerbsAreRegistered(t *testing.T) {
	paths := commandPaths(newRootCmd())
	have := make(map[string]bool, len(paths))
	for _, p := range paths {
		have[p] = true
	}
	for _, p := range []string{"health", "health set", "health unset"} {
		if !have[p] {
			t.Errorf("%q is not registered on the agent surface", p)
		}
	}
}

// TestHealthSetRequiresAPath: Burrow will not guess a path, and neither will the CLI default one in
// on the agent's behalf (ADR-0076 §3).
func TestHealthSetRequiresAPath(t *testing.T) {
	cmd := newHealthSetCmd()
	cmd.SetArgs([]string{"web"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err == nil {
		t.Error("health set with no --path succeeded, want an error rather than a guessed path")
	}
}
