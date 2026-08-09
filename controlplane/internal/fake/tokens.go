// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.TokenSource = (*Tokens)(nil)

// Tokens is a deterministic controlplane.TokenSource that hands out "t1", "t2", ... so a test can
// predict the token an issue returns and then present it back (ADR-0084 §2).
//
// PREDICTABLE IS THE POINT HERE AND IS RUINOUS ANYWHERE ELSE: the token is the credential, so the
// production source is controlplane/sys's, which reads crypto/rand. This one exists so the engine
// reads no ambient randomness (ADR-0010) and never leaves the test binary.
type Tokens struct {
	mu sync.Mutex
	n  int
}

// NewTokens returns a token source starting at t1.
func NewTokens() *Tokens {
	return &Tokens{}
}

// NewToken returns the next sequential token.
func (t *Tokens) NewToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n++
	return fmt.Sprintf("t%d", t.n)
}
