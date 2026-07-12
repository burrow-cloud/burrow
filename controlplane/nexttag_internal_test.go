// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "testing"

// TestNextSemverTags covers the pure next-tag computation (ADR-0052 §8): a stable semver tag yields
// the next patch/minor/major, the leading "v" style is preserved, a two-part tag is completed to
// three parts, and a non-semver or prerelease tag reports ok=false so the caller degrades to a note.
func TestNextSemverTags(t *testing.T) {
	cases := []struct {
		current             string
		patch, minor, major string
		ok                  bool
	}{
		{"1.4.2", "1.4.3", "1.5.0", "2.0.0", true},
		{"v1.4.2", "v1.4.3", "v1.5.0", "v2.0.0", true},
		{"0.0.0", "0.0.1", "0.1.0", "1.0.0", true},
		{"1.2", "1.2.1", "1.3.0", "2.0.0", true}, // two-part tag completed to three
		{"latest", "", "", "", false},
		{"sha-abc123", "", "", "", false},
		{"", "", "", "", false},
		{"1.4.2-rc.1", "", "", "", false}, // prerelease is not a stable release tag
	}
	for _, c := range cases {
		patch, minor, major, ok := nextSemverTags(c.current)
		if ok != c.ok {
			t.Errorf("nextSemverTags(%q) ok = %v, want %v", c.current, ok, c.ok)
			continue
		}
		if ok && (patch != c.patch || minor != c.minor || major != c.major) {
			t.Errorf("nextSemverTags(%q) = %q/%q/%q, want %q/%q/%q",
				c.current, patch, minor, major, c.patch, c.minor, c.major)
		}
	}
}
