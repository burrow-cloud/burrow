// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package fake

import (
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.IDSource = (*IDs)(nil)

// IDs is a deterministic controlplane.IDSource that hands out "r1", "r2", ... so tests
// can predict release identifiers.
type IDs struct {
	mu sync.Mutex
	n  int
}

// NewIDs returns an ID source starting at r1.
func NewIDs() *IDs {
	return &IDs{}
}

// NewID returns the next sequential identifier.
func (i *IDs) NewID() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.n++
	return fmt.Sprintf("r%d", i.n)
}
