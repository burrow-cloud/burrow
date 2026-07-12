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
// SetError, so the auto-deploy read/watch path can be exercised against a known tag set and against a
// registry failure without a real registry. Tags and errors can also be set PER imageRef (SetTagsFor
// / SetErrorFor) so a multi-app poll pass can give one app a failure while another lists cleanly —
// the isolation the fault-injection tests assert.
type Registry struct {
	mu       sync.Mutex
	tags     []string // default tag list, returned for any ref with no per-ref override
	tagsFor  map[string][]string
	err      error // default error, returned for any ref with no per-ref override
	errFor   map[string]error
	lastRef  string
	lastAuth controlplane.RegistryAuth
	calls    int
}

// NewRegistry returns an empty fake registry client.
func NewRegistry() *Registry {
	return &Registry{tagsFor: make(map[string][]string), errFor: make(map[string]error)}
}

// SetTags seeds the tags ListTags returns for any ref without a per-ref override.
func (r *Registry) SetTags(tags ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tags = append([]string(nil), tags...)
}

// SetTagsFor seeds the tags ListTags returns for a specific imageRef, taking precedence over the
// default set. It lets one poll pass return different tags per app.
func (r *Registry) SetTagsFor(ref string, tags ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tagsFor[ref] = append([]string(nil), tags...)
}

// SetError makes ListTags return err for any ref without a per-ref override (nil clears it).
func (r *Registry) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// SetErrorFor makes ListTags return err for a specific imageRef, taking precedence over the default
// error — so a fault can be injected for one app while another lists cleanly (nil clears it).
func (r *Registry) SetErrorFor(ref string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.errFor, ref)
		return
	}
	r.errFor[ref] = err
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
	if err, ok := r.errFor[imageRef]; ok {
		return nil, err
	}
	if r.err != nil {
		return nil, r.err
	}
	if tags, ok := r.tagsFor[imageRef]; ok {
		return append([]string(nil), tags...), nil
	}
	return append([]string(nil), r.tags...), nil
}
