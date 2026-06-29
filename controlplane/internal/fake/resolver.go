// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Resolver = (*Resolver)(nil)

// Resolver is an in-memory controlplane.Resolver. Seed what a host resolves to with Set;
// an unseeded host returns an error, modelling NXDOMAIN.
type Resolver struct {
	mu    sync.Mutex
	hosts map[string][]string
	err   error
}

// NewResolver returns a resolver that knows no hosts.
func NewResolver() *Resolver {
	return &Resolver{hosts: make(map[string][]string)}
}

// Set makes host resolve to addrs.
func (r *Resolver) Set(host string, addrs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[host] = addrs
}

// SetError makes every lookup return err until cleared with SetError(nil).
func (r *Resolver) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *Resolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	addrs, ok := r.hosts[host]
	if !ok {
		return nil, fmt.Errorf("fake resolver: no record for %q", host)
	}
	return append([]string(nil), addrs...), nil
}
