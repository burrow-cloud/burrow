// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var (
	_ controlplane.DNSFactory  = (*DNSFactory)(nil)
	_ controlplane.DNSProvider = (*DNSProvider)(nil)
)

// DNSProvider is an in-memory controlplane.DNSProvider. VerifyAccess returns the configured
// error (nil = the token is accepted).
type DNSProvider struct {
	verifyErr error
}

func (p *DNSProvider) VerifyAccess(ctx context.Context) error { return p.verifyErr }

// DNSFactory is an in-memory controlplane.DNSFactory. By default every DNS(...) returns a
// provider that accepts the token; SetVerifyError makes built providers reject it, and
// SetFactoryError makes DNS(...) itself fail (e.g. an unsupported type). It records the
// (type, token) pairs it was asked for so a test can assert the engine read and passed the
// right token.
type DNSFactory struct {
	mu         sync.Mutex
	verifyErr  error
	factoryErr error
	lastType   controlplane.ProviderType
	lastToken  string
	callCount  int
}

// NewDNSFactory returns a factory whose providers accept any token.
func NewDNSFactory() *DNSFactory { return &DNSFactory{} }

// SetVerifyError makes providers built from here reject the token with err.
func (f *DNSFactory) SetVerifyError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyErr = err
}

// SetFactoryError makes DNS(...) return err without building a provider.
func (f *DNSFactory) SetFactoryError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.factoryErr = err
}

// LastToken returns the token most recently passed to DNS, and how many times DNS was called.
func (f *DNSFactory) LastToken() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastToken, f.callCount
}

func (f *DNSFactory) DNS(t controlplane.ProviderType, token string) (controlplane.DNSProvider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastType, f.lastToken, f.callCount = t, token, f.callCount+1
	if f.factoryErr != nil {
		return nil, f.factoryErr
	}
	return &DNSProvider{verifyErr: f.verifyErr}, nil
}
