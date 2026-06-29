// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.ClusterProber = (*ClusterProber)(nil)

// ClusterProber is an in-memory controlplane.ClusterProber: it returns a canned capability report
// (ADR-0034) so engine tests can drive ClusterCapabilities without a cluster. The cluster-derived
// fields are seeded; the engine fills the DNS field from the providers registry. An error can be
// injected to exercise the failure path.
type ClusterProber struct {
	Caps controlplane.ClusterCapabilities
	Err  error
}

// NewClusterProber returns a prober reporting caps.
func NewClusterProber(caps controlplane.ClusterCapabilities) *ClusterProber {
	return &ClusterProber{Caps: caps}
}

// DetectCapabilities returns the seeded report (or the injected error).
func (p *ClusterProber) DetectCapabilities(_ context.Context) (controlplane.ClusterCapabilities, error) {
	if p.Err != nil {
		return controlplane.ClusterCapabilities{}, p.Err
	}
	return p.Caps, nil
}
