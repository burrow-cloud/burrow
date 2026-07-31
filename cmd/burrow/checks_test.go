// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
)

// TestChecksHumanShowsTheDerivation: the command's whole claim is that the check is derived from what
// Burrow provisioned rather than from something a user typed, so the rendering has to SHOW the
// derivation. A listing that said only "postgres" would be asking the reader to take it on trust.
func TestChecksHumanShowsTheDerivation(t *testing.T) {
	res := client.ChecksResult{
		App: "web", Environment: "prod", Enabled: true,
		Dependencies: []client.Dependency{
			{Kind: "postgres", Provisioned: "a database and login role on environment prod's Postgres instance", EnvKey: "DATABASE_URL"},
			{Kind: "exposure", Provisioned: "a Service in front of container port 8080", Endpoint: "http://web.burrow-apps.svc"},
		},
	}
	got := checksHuman(res)
	for _, w := range []string{
		"checks on",
		"a database and login role",
		"the app's own DATABASE_URL, from inside its container",
		"a Service in front of container port 8080",
		"a request to http://web.burrow-apps.svc",
		"never fails the deploy",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("checksHuman() = %q, want it to contain %q", got, w)
		}
	}
}

// TestChecksHumanSaysWhenItIsOff: a default that was turned off must say so, since the whole point of
// the surface is that a Burrow-supplied hook is visible rather than silent.
func TestChecksHumanSaysWhenItIsOff(t *testing.T) {
	got := checksHuman(client.ChecksResult{App: "web", Environment: "prod", Enabled: false, Note: "it is off"})
	if !strings.Contains(got, "checks off") {
		t.Errorf("checksHuman() = %q, want it to say the check is off", got)
	}
	if !strings.Contains(got, "it is off") {
		t.Errorf("checksHuman() = %q, want it to carry the note", got)
	}
}

// TestDeployDependencyHumanIsSilentWhenEverythingPassed: a deploy that worked should not grow a
// paragraph confirming that the database Burrow attached is still there.
func TestDeployDependencyHumanIsSilentWhenEverythingPassed(t *testing.T) {
	got := deployDependencyHuman([]client.DependencyResult{
		{Kind: "postgres", Outcome: "passed"},
		{Kind: "exposure", Outcome: "passed", Status: 200},
	})
	if got != "" {
		t.Errorf("deployDependencyHuman() = %q, want nothing when every check passed", got)
	}
	if deployDependencyHuman(nil) != "" {
		t.Error("deployDependencyHuman(nil) printed something")
	}
}

// TestDeployDependencyHumanSaysTheDeployIsLive is the sentence that stops a failed check being read
// as a failed deploy. A user who sees "failed" under a deploy will otherwise assume something was
// undone, and nothing was.
func TestDeployDependencyHumanSaysTheDeployIsLive(t *testing.T) {
	got := deployDependencyHuman([]client.DependencyResult{
		{Kind: "postgres", Outcome: "failed", Reason: "AuthenticationFailed", Detail: "the instance rejected the credential"},
		{Kind: "exposure", Outcome: "passed", Status: 200},
	})
	for _, w := range []string{"NOT rolled back", "postgres: failed", "AuthenticationFailed", "rejected the credential"} {
		if !strings.Contains(got, w) {
			t.Errorf("deployDependencyHuman() = %q, want it to contain %q", got, w)
		}
	}
	if strings.Contains(got, "exposure") {
		t.Errorf("deployDependencyHuman() = %q, want passed checks omitted", got)
	}
}

// TestChecksLongCarriesTheReasoning: the help text is where an operator meets the two facts that stop
// this being misread — that the list is derived, and that a failure never fails a deploy.
func TestChecksLongCarriesTheReasoning(t *testing.T) {
	for _, w := range []string{
		"DERIVED, never configured",
		"NEVER FAILS THE DEPLOY",
		"not a readiness probe",
		"no shell and no client tools",
	} {
		if !strings.Contains(checksLong, w) {
			t.Errorf("checksLong does not mention %q", w)
		}
	}
}

// TestAppChecksIsUnderTheAppGroup keeps the command where a person looks for it (ADR-0024).
func TestAppChecksIsUnderTheAppGroup(t *testing.T) {
	var found bool
	for _, c := range newAppCmd().Commands() {
		if c.Name() != "checks" {
			continue
		}
		found = true
		names := map[string]bool{}
		for _, sub := range c.Commands() {
			names[sub.Name()] = true
		}
		for _, want := range []string{"show", "enable", "disable"} {
			if !names[want] {
				t.Errorf("`burrow app checks` has no %q subcommand", want)
			}
		}
	}
	if !found {
		t.Error("`burrow app checks` is not registered under the app group")
	}
}
