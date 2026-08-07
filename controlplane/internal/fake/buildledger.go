// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"sync"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.BuildLedger = (*BuildLedger)(nil)

// BuildLedger is an in-memory controlplane.BuildLedger. A test seeds it with the builds that
// succeeded and were never deployed (Add), then reads back what the reconciler did with each of them
// — reaped, held, or left alone — so the recovery of a build whose client went away can be exercised
// without a cluster (issue #504).
//
// It models the one behaviour of the real ledger that matters to the engine: a held build is not
// listed again, so a guardrail decision is recorded once rather than on every sweep.
type BuildLedger struct {
	mu       sync.Mutex
	builds   []controlplane.StrandedBuild
	held     map[string]string
	reaped   []string
	listErr  error
	holdErr  error
	listedAt []time.Time
}

// NewBuildLedger returns an empty ledger.
func NewBuildLedger() *BuildLedger {
	return &BuildLedger{held: map[string]string{}}
}

// Add seeds a build that succeeded and whose deploy never ran.
func (l *BuildLedger) Add(b controlplane.StrandedBuild) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.builds = append(l.builds, b)
}

// SetListError makes StrandedBuilds fail (nil clears it), so a test can assert a sweep that cannot
// list is contained rather than fatal.
func (l *BuildLedger) SetListError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listErr = err
}

// SetHoldError makes HoldBuild fail (nil clears it), so a test can assert what happens when the
// marker cannot be written at all — an install whose RBAC predates the verb the marker needs.
func (l *BuildLedger) SetHoldError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holdErr = err
}

// Reaped returns the ids of the builds the reconciler discarded, in order.
func (l *BuildLedger) Reaped() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reaped...)
}

// Held returns the reason recorded against a build the reconciler could not finish, and whether one
// was recorded at all.
func (l *BuildLedger) Held(id string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	reason, ok := l.held[id]
	return reason, ok
}

// Cutoffs returns the settling cutoffs StrandedBuilds was called with, in order, so a test can
// assert the reconciler asks for builds that finished before now minus its margin.
func (l *BuildLedger) Cutoffs() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.listedAt...)
}

func (l *BuildLedger) StrandedBuilds(_ context.Context, completedBefore time.Time) ([]controlplane.StrandedBuild, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listedAt = append(l.listedAt, completedBefore)
	if l.listErr != nil {
		return nil, l.listErr
	}
	var out []controlplane.StrandedBuild
	for _, b := range l.builds {
		if _, held := l.held[b.ID]; held {
			continue
		}
		if !b.CompletedAt.IsZero() && !b.CompletedAt.Before(completedBefore) {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func (l *BuildLedger) ReapBuild(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reaped = append(l.reaped, id)
	remaining := l.builds[:0]
	for _, b := range l.builds {
		if b.ID != id {
			remaining = append(remaining, b)
		}
	}
	l.builds = remaining
	return nil
}

func (l *BuildLedger) HoldBuild(_ context.Context, id, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holdErr != nil {
		return l.holdErr
	}
	l.held[id] = reason
	return nil
}
