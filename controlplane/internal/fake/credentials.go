// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Credentials = (*Credentials)(nil)

// Credentials is an in-memory controlplane.Credentials. Tests seed tokens with Set, read what
// SetToken wrote with Get, and inject failure with SetError (OpToken, OpSetToken).
type Credentials struct {
	mu     sync.Mutex
	tokens map[string]string
	errs   map[Op]error
}

// NewCredentials returns an empty fake credential store.
func NewCredentials() *Credentials {
	return &Credentials{tokens: make(map[string]string), errs: make(map[Op]error)}
}

// Set records that key holds token, seeding the store directly (e.g. a token a prior set wrote).
func (c *Credentials) Set(key, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[key] = token
}

// Get returns the token stored under key and whether it is present, so a test can assert what
// SetToken wrote into the credential store.
func (c *Credentials) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.tokens[key]
	return v, ok
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

// SetToken upserts key=value into the in-memory store, modelling burrowd writing the token it
// received over the control-plane API into burrow-credentials (ADR-0030). An OpSetToken error can
// be injected to exercise the failure path.
func (c *Credentials) SetToken(ctx context.Context, key, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.errs[OpSetToken]; err != nil {
		return err
	}
	c.tokens[key] = value
	return nil
}
