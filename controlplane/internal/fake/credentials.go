// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Credentials = (*Credentials)(nil)

// Credentials is an in-memory controlplane.Credentials. Tests seed tokens with Set and inject
// failure with SetError (OpToken).
type Credentials struct {
	mu     sync.Mutex
	tokens map[string]string
	errs   map[Op]error
}

// NewCredentials returns an empty fake credential store.
func NewCredentials() *Credentials {
	return &Credentials{tokens: make(map[string]string), errs: make(map[Op]error)}
}

// Set records that key holds token, as the developer's CLI would have written it.
func (c *Credentials) Set(key, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[key] = token
}

// SetError makes the Token op return err until cleared with SetError(OpToken, nil).
func (c *Credentials) SetError(op Op, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		delete(c.errs, op)
		return
	}
	c.errs[op] = err
}

func (c *Credentials) Token(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.errs[OpToken]; err != nil {
		return "", err
	}
	v, ok := c.tokens[key]
	if !ok {
		return "", fmt.Errorf("credentials: key %q: %w", key, controlplane.ErrNotFound)
	}
	return v, nil
}
