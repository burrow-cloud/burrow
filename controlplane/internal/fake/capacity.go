// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.CapacityProber = (*CapacityProber)(nil)

// CapacityProber is an in-memory controlplane.CapacityProber: it returns a canned resource state
// (node allocatable + pod requests) so engine tests can drive ClusterCapacity — the headroom, top
// consumers, and verdict math — without a cluster (issue #275). An error can be injected to
// exercise the failure path.
type CapacityProber struct {
	State controlplane.ClusterResourceState
	Err   error
}

// NewCapacityProber returns a prober reporting state.
func NewCapacityProber(state controlplane.ClusterResourceState) *CapacityProber {
	return &CapacityProber{State: state}
}

// ReadResourceState returns the seeded state (or the injected error).
func (p *CapacityProber) ReadResourceState(_ context.Context) (controlplane.ClusterResourceState, error) {
	if p.Err != nil {
		return controlplane.ClusterResourceState{}, p.Err
	}
	return p.State, nil
}
