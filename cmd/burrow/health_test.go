// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
)

// TestHealthHumanLeadsWithTheProbe: the human rendering answers "what is Burrow doing to my app"
// first, then where it came from. The declaration is secondary because the probe is what acts.
func TestHealthHumanLeadsWithTheProbe(t *testing.T) {
	cases := []struct {
		name string
		res  client.HealthResult
		want []string
	}{
		{
			name: "the conservative default",
			res:  client.HealthResult{App: "web", Environment: "prod", Probe: "tcp", ProbePort: 8080, Source: "exposure", Hint: "add one"},
			want: []string{"TCP connect on port 8080", "published on", "liveness probe: none", "add one"},
		},
		{
			name: "a declared endpoint",
			res:  client.HealthResult{App: "web", Environment: "prod", Path: "/healthz", Probe: "http", ProbePort: 8080, ProbePath: "/healthz", Source: "endpoint"},
			want: []string{"HTTP GET /healthz on port 8080", "declared for this app", "liveness probe: none"},
		},
		{
			name: "no probe at all",
			res:  client.HealthResult{App: "worker", Environment: "prod", Probe: "none", Source: "none", AppliesOn: "the app's next release"},
			want: []string{"no readiness probe", "applies on: the app's next release"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := healthHuman(c.res)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("healthHuman() = %q, want it to contain %q", got, w)
				}
			}
		})
	}
}

// TestAppHealthLongCarriesTheGuidance: the operator CLI states the same case ADR-0076 §5 puts on the
// agent surface, including what the endpoint must not check.
func TestAppHealthLongCarriesTheGuidance(t *testing.T) {
	for _, w := range []string{"never sets a liveness probe", "Do NOT have that endpoint check your database", "shared"} {
		if !strings.Contains(healthLong, w) {
			t.Errorf("healthLong does not mention %q", w)
		}
	}
}

// TestAppHealthIsUnderTheAppGroup keeps the command where a person looks for it (ADR-0024).
func TestAppHealthIsUnderTheAppGroup(t *testing.T) {
	var found bool
	for _, c := range newAppCmd().Commands() {
		if c.Name() == "health" {
			found = true
			names := map[string]bool{}
			for _, sub := range c.Commands() {
				names[sub.Name()] = true
			}
			for _, want := range []string{"show", "set", "unset"} {
				if !names[want] {
					t.Errorf("`burrow app health` has no %q subcommand", want)
				}
			}
		}
	}
	if !found {
		t.Error("`burrow app health` is not registered under the app group")
	}
}
