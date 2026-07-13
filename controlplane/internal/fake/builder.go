// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Builder = (*Builder)(nil)

// Builder is an in-memory controlplane.Builder. Tests seed the digest it returns with SetDigest,
// inject a build failure with SetError, and read back the source ref and target image it was called
// with (LastSource / LastTarget) plus the call count, so the in-cluster build orchestration can be
// exercised — build success feeding the guarded deploy path, and build failure NOT touching it —
// without standing up a real Kubernetes build Job (ADR-0053 §6).
type Builder struct {
	mu         sync.Mutex
	digest     string
	err        error
	lastSource controlplane.SourceRef
	lastTarget string
	calls      int
}

// DefaultDigest is the digest NewBuilder returns until SetDigest overrides it, so a test can assert
// the built reference the engine deployed without first seeding a value.
const DefaultDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// NewBuilder returns a fake builder that returns DefaultDigest on a successful build.
func NewBuilder() *Builder { return &Builder{digest: DefaultDigest} }

// SetDigest seeds the digest a successful Build returns.
func (b *Builder) SetDigest(digest string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.digest = digest
}

// SetError makes Build return err (nil clears it), exercising the build-failure path where the deploy
// path must not be touched.
func (b *Builder) SetError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// LastSource returns the source ref Build was last called with.
func (b *Builder) LastSource() controlplane.SourceRef {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSource
}

// LastTarget returns the target image reference Build was last called with.
func (b *Builder) LastTarget() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastTarget
}

// Calls returns how many times Build has been called.
func (b *Builder) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *Builder) Build(_ context.Context, source controlplane.SourceRef, targetImage string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.lastSource = source
	b.lastTarget = targetImage
	if b.err != nil {
		return "", b.err
	}
	return b.digest, nil
}
