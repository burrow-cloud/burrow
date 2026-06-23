// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Registry = (*Registry)(nil)

// Registry is an in-memory controlplane.Registry. Tests seed known references with Add
// and inject failure with SetError.
type Registry struct {
	mu      sync.Mutex
	digests map[string]string
	errs    map[Op]error
}

// NewRegistry returns an empty fake registry.
func NewRegistry() *Registry {
	return &Registry{
		digests: make(map[string]string),
		errs:    make(map[Op]error),
	}
}

// Add records that reference resolves to digest.
func (r *Registry) Add(reference, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.digests[reference] = digest
}

// SetError makes op return err until cleared with SetError(op, nil). Only OpResolve is
// meaningful for the registry.
func (r *Registry) SetError(op Op, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.errs, op)
		return
	}
	r.errs[op] = err
}

func (r *Registry) Resolve(ctx context.Context, reference string) (controlplane.ImageInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.errs[OpResolve]; err != nil {
		return controlplane.ImageInfo{}, err
	}
	digest, ok := r.digests[reference]
	if !ok {
		return controlplane.ImageInfo{}, fmt.Errorf("registry: reference %q: %w", reference, controlplane.ErrNotFound)
	}
	return controlplane.ImageInfo{Reference: reference, Digest: digest}, nil
}
