// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package sys

import (
	"encoding/hex"
	"testing"
	"time"
)

func TestClockNow(t *testing.T) {
	before := time.Now()
	got := Clock{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Clock.Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestIDsUniqueAndHex(t *testing.T) {
	ids := IDs{}
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := ids.NewID()
		if len(id) != 32 {
			t.Fatalf("NewID() = %q, want 32 hex chars", id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("NewID() = %q is not hex: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("NewID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}
