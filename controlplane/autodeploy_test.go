// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "testing"

func TestAutoDeployLevelValid(t *testing.T) {
	for _, l := range []AutoDeployLevel{AutoDeployOff, AutoDeployPatch, AutoDeployMinor, AutoDeployMajor} {
		if !l.Valid() {
			t.Errorf("level %q should be valid", l)
		}
	}
	for _, l := range []AutoDeployLevel{"", "PATCH", "latest", "on", "none"} {
		if AutoDeployLevel(l).Valid() {
			t.Errorf("level %q should not be valid", l)
		}
	}
}

// TestDefaultAutoDeployLevel pins the opt-in default: an app with no explicitly-set level is off, so
// the watcher never polls or moves it until an operator opts in (ADR-0054, revising ADR-0052 §2).
func TestDefaultAutoDeployLevel(t *testing.T) {
	if DefaultAutoDeployLevel != AutoDeployOff {
		t.Errorf("default auto-deploy level = %q, want %q (auto-deploy is opt-in)", DefaultAutoDeployLevel, AutoDeployOff)
	}
	if !DefaultAutoDeployLevel.Valid() {
		t.Errorf("default auto-deploy level %q should be valid", DefaultAutoDeployLevel)
	}
}

func TestParseAutoDeployLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    AutoDeployLevel
		wantErr bool
	}{
		{"off", AutoDeployOff, false},
		{"patch", AutoDeployPatch, false},
		{"minor", AutoDeployMinor, false},
		{"major", AutoDeployMajor, false},
		{"  minor  ", AutoDeployMinor, false}, // whitespace tolerated
		{"MINOR", AutoDeployMinor, false},     // case-insensitive
		{"Patch", AutoDeployPatch, false},
		{"", "", true},
		{"none", "", true},
		{"latest", "", true},
		{"on", "", true},
	}
	for _, tc := range tests {
		got, err := ParseAutoDeployLevel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseAutoDeployLevel(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAutoDeployLevel(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAutoDeployLevel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveAutoDeploy(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		available   []string
		level       AutoDeployLevel
		wantTarget  string
		wantUpgrade string
	}{
		{
			name:        "patch takes highest patch in minor, surfaces major",
			current:     "1.2.5",
			available:   []string{"1.2.6", "1.2.7", "1.3.0", "2.0.0"},
			level:       AutoDeployPatch,
			wantTarget:  "1.2.7",
			wantUpgrade: "2.0.0",
		},
		{
			name:        "minor takes highest minor in major, surfaces major",
			current:     "1.2.5",
			available:   []string{"1.2.6", "1.3.0", "1.4.2", "2.0.0"},
			level:       AutoDeployMinor,
			wantTarget:  "1.4.2",
			wantUpgrade: "2.0.0",
		},
		{
			name:        "major takes highest, nothing above the cap",
			current:     "1.2.5",
			available:   []string{"1.3.0", "2.0.0", "3.1.0"},
			level:       AutoDeployMajor,
			wantTarget:  "3.1.0",
			wantUpgrade: "",
		},
		{
			name:        "off deploys nothing and surfaces nothing",
			current:     "1.2.5",
			available:   []string{"1.2.6", "1.3.0", "2.0.0"},
			level:       AutoDeployOff,
			wantTarget:  "",
			wantUpgrade: "",
		},
		{
			name:        "never downgrade",
			current:     "1.5.0",
			available:   []string{"1.4.9", "1.4.0"},
			level:       AutoDeployMajor,
			wantTarget:  "",
			wantUpgrade: "",
		},
		{
			name:        "equal only is not an upgrade",
			current:     "1.2.5",
			available:   []string{"1.2.5"},
			level:       AutoDeployMajor,
			wantTarget:  "",
			wantUpgrade: "",
		},
		{
			name:        "non-semver tags ignored",
			current:     "1.2.5",
			available:   []string{"latest", "sha-abc123", "1.2.6", "nightly"},
			level:       AutoDeployPatch,
			wantTarget:  "1.2.6",
			wantUpgrade: "",
		},
		{
			name:        "current non-semver yields nothing",
			current:     "latest",
			available:   []string{"1.2.6", "1.3.0"},
			level:       AutoDeployMinor,
			wantTarget:  "",
			wantUpgrade: "",
		},
		{
			name:        "prerelease excluded under minor",
			current:     "1.2.5",
			available:   []string{"1.2.6-rc1"},
			level:       AutoDeployMinor,
			wantTarget:  "",
			wantUpgrade: "",
		},
		{
			name:        "prerelease excluded under major (not surfaced either)",
			current:     "1.2.5",
			available:   []string{"2.0.0-rc1"},
			level:       AutoDeployMinor,
			wantTarget:  "",
			wantUpgrade: "",
		},
		{
			name:        "prerelease skipped, stable of same version taken",
			current:     "1.2.5",
			available:   []string{"1.2.6-rc1", "1.2.6"},
			level:       AutoDeployPatch,
			wantTarget:  "1.2.6",
			wantUpgrade: "",
		},
		{
			name:        "v-prefix and bare mixed compare correctly",
			current:     "1.2.5",
			available:   []string{"v1.2.6", "1.2.7"},
			level:       AutoDeployPatch,
			wantTarget:  "1.2.7",
			wantUpgrade: "",
		},
		{
			name:        "v-prefix tag returned verbatim",
			current:     "v1.2.5",
			available:   []string{"v1.2.6"},
			level:       AutoDeployPatch,
			wantTarget:  "v1.2.6",
			wantUpgrade: "",
		},
		{
			name:        "patch stays in minor, newer minor surfaced",
			current:     "1.2.5",
			available:   []string{"1.3.0"},
			level:       AutoDeployPatch,
			wantTarget:  "",
			wantUpgrade: "1.3.0",
		},
		{
			name:        "patch surfaces highest above cap across minors and majors",
			current:     "1.2.5",
			available:   []string{"1.3.0", "1.4.0", "2.0.0"},
			level:       AutoDeployPatch,
			wantTarget:  "",
			wantUpgrade: "2.0.0",
		},
		{
			name:        "minor does not cross major even when only a major is newer",
			current:     "1.4.0",
			available:   []string{"2.0.0", "2.1.0"},
			level:       AutoDeployMinor,
			wantTarget:  "",
			wantUpgrade: "2.1.0",
		},
		{
			name:        "patch after a manual minor jump auto-patches the new minor",
			current:     "1.5.0",
			available:   []string{"1.5.1", "1.5.2", "1.6.0"},
			level:       AutoDeployPatch,
			wantTarget:  "1.5.2",
			wantUpgrade: "1.6.0",
		},
		{
			name:        "order in available does not matter, semver order wins",
			current:     "1.2.5",
			available:   []string{"1.4.2", "1.2.9", "1.3.7", "1.2.6"},
			level:       AutoDeployMinor,
			wantTarget:  "1.4.2",
			wantUpgrade: "",
		},
		{
			name:        "empty available",
			current:     "1.2.5",
			available:   nil,
			level:       AutoDeployMinor,
			wantTarget:  "",
			wantUpgrade: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, upgrade := ResolveAutoDeploy(tc.current, tc.available, tc.level)
			if target != tc.wantTarget || upgrade != tc.wantUpgrade {
				t.Errorf("ResolveAutoDeploy(%q, %v, %q) = (%q, %q), want (%q, %q)",
					tc.current, tc.available, tc.level, target, upgrade, tc.wantTarget, tc.wantUpgrade)
			}
		})
	}
}
