// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package sys holds the production implementations of the control plane's trivial
// seams — the system Clock and a crypto/rand ID source — the concrete values
// cmd/burrowd injects in place of the test fakes (ADR-0010). It lives under
// controlplane/ (not controlplane/internal) so cmd/burrowd and the managed module can
// wire it; it is source-available under FSL-1.1-ALv2.
package sys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var (
	_ controlplane.Clock    = Clock{}
	_ controlplane.IDSource = IDs{}
	_ controlplane.Resolver = Resolver{}
)

// Clock is the real wall clock.
type Clock struct{}

// Now returns the current time.
func (Clock) Now() time.Time { return time.Now() }

// Resolver does real DNS lookups via the system resolver.
type Resolver struct{}

// LookupHost returns the addresses host resolves to.
func (Resolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// IDs mints release identifiers from crypto/rand: 128 bits of randomness, hex-encoded.
type IDs struct{}

// NewID returns a fresh random identifier. It panics only if the system's secure
// random source fails, which is unrecoverable and does not happen in normal operation.
func (IDs) NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("sys: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
