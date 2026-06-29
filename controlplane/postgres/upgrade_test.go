// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import "testing"

func TestParseMajorMinor(t *testing.T) {
	cases := []struct {
		in        string
		maj, min  int
		wantError bool
	}{
		{"0.1.0", 0, 1, false},
		{"v0.2.3", 0, 2, false},
		{"1.4", 1, 4, false},
		{"0.1.0-rc1", 0, 1, false},
		{"0.1.0+build5", 0, 1, false},
		{"v1.0.0-alpha+meta", 1, 0, false},
		{" 0.7.2 ", 0, 7, false},
		{"1", 0, 0, true},
		{"x.y.z", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			maj, min, err := parseMajorMinor(c.in)
			if c.wantError {
				if err == nil {
					t.Fatalf("parseMajorMinor(%q) = nil error, want error", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMajorMinor(%q) error: %v", c.in, err)
			}
			if maj != c.maj || min != c.min {
				t.Fatalf("parseMajorMinor(%q) = %d.%d, want %d.%d", c.in, maj, min, c.maj, c.min)
			}
		})
	}
}

func TestCheckUpgrade(t *testing.T) {
	cases := []struct {
		name                   string
		dMaj, dMin, bMaj, bMin int
		ok                     bool
	}{
		{"same version", 0, 1, 0, 1, true},
		{"one minor step", 0, 1, 0, 2, true},
		{"another one-step", 1, 9, 1, 10, true},
		{"two minor skip", 0, 1, 0, 3, false},
		{"big skip", 0, 1, 0, 9, false},
		{"downgrade", 0, 3, 0, 2, false},
		{"cross-major up", 0, 9, 1, 0, false},
		{"cross-major down", 1, 0, 0, 9, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkUpgrade(c.dMaj, c.dMin, c.bMaj, c.bMin)
			if c.ok && err != nil {
				t.Fatalf("checkUpgrade(%d.%d -> %d.%d) = %v, want allowed", c.dMaj, c.dMin, c.bMaj, c.bMin, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("checkUpgrade(%d.%d -> %d.%d) = nil, want refused", c.dMaj, c.dMin, c.bMaj, c.bMin)
			}
		})
	}
}
