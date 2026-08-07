// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.HTTPProbe = (*Probe)(nil)

// Probe is an in-memory controlplane.HTTPProbe for the publish pre-flight. Seed the hosts that
// answer with Answers; a host that was not seeded returns an error, modelling a request that never
// reached the cluster. Every probed URL is recorded, so a test can assert WHICH path was walked —
// the pre-flight probes the ACME challenge path on purpose, and a test that only checked "a probe
// happened" would not notice that changing.
type Probe struct {
	mu     sync.Mutex
	hosts  map[string]int
	probed []string
}

// NewProbe returns a probe that no host answers.
func NewProbe() *Probe {
	return &Probe{hosts: make(map[string]int)}
}

// Answers makes every request to host come back with status.
func (p *Probe) Answers(host string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hosts[host] = status
}

// Probed returns the URLs probed so far, in order.
func (p *Probe) Probed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.probed...)
}

// ProbeHTTP records the request and answers for a seeded host.
func (p *Probe) ProbeHTTP(_ context.Context, raw string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probed = append(p.probed, raw)
	u, err := url.Parse(raw)
	if err != nil {
		return 0, fmt.Errorf("fake probe: %w", err)
	}
	status, ok := p.hosts[u.Hostname()]
	if !ok {
		return 0, fmt.Errorf("fake probe: nothing answers for %q", u.Hostname())
	}
	return status, nil
}
