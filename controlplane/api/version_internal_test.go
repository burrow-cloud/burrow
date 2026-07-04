// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api

import "testing"

// TestClientSupported exercises the one-minor compatibility window (ADR-0039): burrowd serves the
// same minor, one minor back, and any newer client, but refuses a client two or more minors behind.
// A non-release version on either side, or a locally built (pseudo-version) client, is always served.
func TestClientSupported(t *testing.T) {
	cases := []struct {
		name           string
		server, client string
		want           bool
	}{
		{"same minor, older patch", "v0.9.1", "v0.9.0", true},
		{"exact same version", "v0.9.1", "v0.9.1", true},
		{"one minor back", "v0.9.1", "v0.8.5", true},
		{"two minors back", "v0.9.1", "v0.7.9", false},
		{"three minors back", "v0.9.1", "v0.6.0", false},
		{"newer client, one ahead", "v0.9.1", "v0.10.0", true},
		{"newer client, many ahead", "v0.9.1", "v1.2.0", true},
		{"empty client served", "v0.9.1", "", true},
		{"dev client served", "v0.9.1", "dev", true},
		{"pseudo-version client served", "v0.9.1", "v0.0.0-20240101000000-abcdef012345", true},
		{"empty server permissive", "", "v0.1.0", true},
		{"dev v0.0.0 server never blocks", "v0.0.0", "v0.9.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientSupported(tc.server, tc.client); got != tc.want {
				t.Errorf("clientSupported(%q, %q) = %v, want %v", tc.server, tc.client, got, tc.want)
			}
		})
	}
}

// TestOneMinorBack confirms the window floor is one minor below the server, and that a major's ".0"
// has no older minor within the major (returned unchanged).
func TestOneMinorBack(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v0.9", "v0.8"},
		{"v0.10", "v0.9"},
		{"v1.4", "v1.3"},
		{"v0.0", "v0.0"},
		{"v2.0", "v2.0"},
		{"garbage", "garbage"},
	}
	for _, tc := range cases {
		if got := oneMinorBack(tc.in); got != tc.want {
			t.Errorf("oneMinorBack(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
