// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.RegistryClient = (*Registry)(nil)

// Registry is an in-memory controlplane.RegistryClient. Tests seed the tag list it returns with
// SetTags, read back the last imageRef and auth it was called with, and inject a failure with
// SetError, so the auto-deploy read path can be exercised against a known tag set and against a
// registry failure without a real registry.
type Registry struct {
	mu       sync.Mutex
	tags     []string
	err      error
	lastRef  string
	lastAuth controlplane.RegistryAuth
	calls    int
}

// NewRegistry returns an empty fake registry client.
func NewRegistry() *Registry { return &Registry{} }

// SetTags seeds the tags ListTags returns.
func (r *Registry) SetTags(tags ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tags = append([]string(nil), tags...)
}

// SetError makes ListTags return err (nil clears it).
func (r *Registry) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// LastRef returns the imageRef ListTags was last called with.
func (r *Registry) LastRef() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastRef
}

// LastAuth returns the auth ListTags was last called with — a test asserts the read path lists
// anonymously (the zero value) in this phase.
func (r *Registry) LastAuth() controlplane.RegistryAuth {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastAuth
}

// Calls returns how many times ListTags has been called.
func (r *Registry) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *Registry) ListTags(_ context.Context, imageRef string, auth controlplane.RegistryAuth) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastRef = imageRef
	r.lastAuth = auth
	if r.err != nil {
		return nil, r.err
	}
	return append([]string(nil), r.tags...), nil
}
