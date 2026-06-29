// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"sync"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Clock = (*Clock)(nil)

// Clock is a controllable controlplane.Clock. It does not advance on its own; tests
// move it with Advance or Set, so every time-dependent result is deterministic.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a Clock fixed at start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
