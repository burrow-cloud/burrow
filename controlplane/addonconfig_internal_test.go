// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import "testing"

// parseStorageSize is what stands between an operator and a volume shrink, so it is tested directly
// rather than only through the refusal it feeds. The interesting cases are the ones where a wrong
// answer is a silent one: a fractional quantity a storage class would round, and a suffix read as
// the wrong base — mistaking Gi for G by 7% is enough to call a shrink a grow.
func TestParseStorageSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{in: "50Gi", want: 50 << 30},
		{in: "1Ti", want: 1 << 40},
		{in: " 20Gi ", want: 20 << 30},
		{in: "512Mi", want: 512 << 20},
		{in: "1G", want: 1e9},
		{in: "1024", want: 1024},
		// Refused rather than rounded: comparing a rounded value against the one that was typed is how
		// a shrink gets waved through as "the same size".
		{in: "1.5Gi", bad: true},
		{in: "", bad: true},
		{in: "-5Gi", bad: true},
		{in: "big", bad: true},
	}
	for _, c := range cases {
		got, err := parseStorageSize(c.in)
		switch {
		case c.bad && err == nil:
			t.Errorf("parseStorageSize(%q) = %d, want a refusal", c.in, got)
		case !c.bad && err != nil:
			t.Errorf("parseStorageSize(%q): %v", c.in, err)
		case !c.bad && got != c.want:
			t.Errorf("parseStorageSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	// Gi is bigger than G, so a Gi value must not compare as smaller than the same number of G. This
	// is the comparison a shrink refusal is made on.
	gi, _ := parseStorageSize("50Gi")
	g, _ := parseStorageSize("50G")
	if gi <= g {
		t.Errorf("50Gi (%d) does not exceed 50G (%d); the binary and decimal suffixes are being read as one", gi, g)
	}
}

// TestReadAddressKeyFollowsTheAttachment: the read address is named after the variable the app
// already reads, so an app that renamed its connection string gets a matching pair rather than a
// `DATABASE_READ_URL` beside a `PG_DSN` (issue #462).
func TestReadAddressKeyFollowsTheAttachment(t *testing.T) {
	if got := readAddressKey("DATABASE_URL"); got != "DATABASE_URL_READ" {
		t.Errorf("readAddressKey(DATABASE_URL) = %q", got)
	}
	if got := readAddressKey("PG_DSN"); got != "PG_DSN_READ" {
		t.Errorf("readAddressKey(PG_DSN) = %q", got)
	}
}
