// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestEveryDeployedAddonProvesItAnswers walks the CATALOG rather than a list of types, so a new
// entry arrives with a readiness answer or fails here. That is the shape of the original defect: an
// add-on whose endpoint is returned to an agent the moment its container starts hands over a socket
// that refuses connections, and nothing about adding a catalog entry says the question was asked.
//
// Postgres is the one exclusion and it is stated rather than filtered quietly: Burrow authors no
// container for it — CloudNativePG composes the pods from the `Cluster` (ADR-0066 §1) and sets
// their probes — so there is no pod spec of Burrow's for a probe to land on.
func TestEveryDeployedAddonProvesItAnswers(t *testing.T) {
	for _, s := range cp.AddonCatalog() {
		if s.Type == cp.AddonPostgres {
			if s.Readiness().Enabled() && s.HealthPath != "" {
				t.Errorf("%s declares a health path, but Burrow authors no container for it", s.Type)
			}
			continue
		}
		r := s.Readiness()
		if !r.Enabled() {
			t.Errorf("%s resolves no readiness check: its instance would join its Service before it answers", s.Type)
			continue
		}
		if r.Port != s.Port {
			t.Errorf("%s probes port %d but its endpoint names port %d", s.Type, r.Port, s.Port)
		}
	}
}

// TestAddonReadinessKindsAreTheVettedOnes pins WHICH check each type gets, because the choice is
// per-type knowledge that only the catalog entry holds: VictoriaLogs and VictoriaMetrics serve
// /health, which is the store saying it is serving, and ValKey answers nothing over HTTP so a TCP
// connect is the strongest check that can be authored for it. Silently downgrading an HTTP check to
// a socket would still pass the case above while proving less than the entry intends.
func TestAddonReadinessKindsAreTheVettedOnes(t *testing.T) {
	for _, tc := range []struct {
		typ  cp.AddonType
		kind string
		path string
	}{
		{cp.AddonLogs, "http", "/health"},
		{cp.AddonMetrics, "http", "/health"},
		{cp.AddonCache, "tcp", ""},
	} {
		s, ok := cp.LookupAddon(tc.typ)
		if !ok {
			t.Fatalf("LookupAddon(%q) = false, want the catalog entry", tc.typ)
		}
		r := s.Readiness()
		if got := r.Kind(); got != tc.kind {
			t.Errorf("%s readiness kind = %q, want %q", tc.typ, got, tc.kind)
		}
		if r.Path != tc.path {
			t.Errorf("%s readiness path = %q, want %q", tc.typ, r.Path, tc.path)
		}
	}
}

// TestAddonReadinessTracksThePort is the derivation ADR-0076 §6's asymmetry argues for: the probe
// follows the entry's port instead of carrying its own copy of it, so a port that moves cannot
// leave a probe behind on the old one — which would render an instance that never becomes ready,
// and a readiness check that wrongly reports unhealthy costs a user the ability to deploy at all.
func TestAddonReadinessTracksThePort(t *testing.T) {
	s, ok := cp.LookupAddon(cp.AddonCache)
	if !ok {
		t.Fatal("LookupAddon(cache) = false, want the catalog entry")
	}
	s.Port = 6380
	if got := s.Readiness().Port; got != 6380 {
		t.Errorf("readiness port = %d after moving the entry's port to 6380, want it to follow", got)
	}
}
