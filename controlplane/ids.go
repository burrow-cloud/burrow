// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

// IDSource mints release identifiers. It is a seam (ADR-0010): the engine never reads
// ambient randomness or time to make an ID, so a test can supply a deterministic
// counter while production supplies a UUID minter. Implementations must return a
// non-empty, unique string on each call.
type IDSource interface {
	// NewID returns a fresh release identifier.
	NewID() string
}
