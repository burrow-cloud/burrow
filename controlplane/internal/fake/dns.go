// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var (
	_ controlplane.DNSFactory  = (*DNSFactory)(nil)
	_ controlplane.DNSProvider = (*DNSProvider)(nil)
)

// DNSProvider is an in-memory controlplane.DNSProvider. VerifyAccess and the record
// operations return their configured error (nil = success); successful EnsureRecord/
// DeleteRecord update an inspectable in-memory record set.
type DNSProvider struct {
	mu          sync.Mutex
	verifyErr   error
	ensureErr   error
	deleteErr   error
	records     map[string]controlplane.DNSRecord // by Name
	ensureCalls int
	deleteCalls int
}

func newDNSProvider() *DNSProvider {
	return &DNSProvider{records: make(map[string]controlplane.DNSRecord)}
}

func (p *DNSProvider) VerifyAccess(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verifyErr
}

func (p *DNSProvider) EnsureRecord(ctx context.Context, r controlplane.DNSRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureCalls++
	if p.ensureErr != nil {
		return p.ensureErr
	}
	p.records[r.Name] = r
	return nil
}

func (p *DNSProvider) DeleteRecord(ctx context.Context, host string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteCalls++
	if p.deleteErr != nil {
		return p.deleteErr
	}
	if _, ok := p.records[host]; !ok {
		return fmt.Errorf("dns: no record for %q: %w", host, controlplane.ErrNotFound)
	}
	delete(p.records, host)
	return nil
}

// Record returns the record currently held for name, and whether one exists.
func (p *DNSProvider) Record(name string) (controlplane.DNSRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.records[name]
	return r, ok
}

// Calls returns how many times EnsureRecord and DeleteRecord were invoked.
func (p *DNSProvider) Calls() (ensure, delete int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureCalls, p.deleteCalls
}

// DNSFactory is an in-memory controlplane.DNSFactory. Every DNS(...) returns the same shared
// provider (so a test can configure and inspect it), unless SetFactoryError makes DNS(...)
// itself fail. It records the (type, token) pairs it was asked for, so a test can assert the
// engine read and passed the right token.
type DNSFactory struct {
	mu         sync.Mutex
	factoryErr error
	provider   *DNSProvider
	lastType   controlplane.ProviderType
	lastToken  string
	callCount  int
}

// NewDNSFactory returns a factory whose shared provider accepts any token.
func NewDNSFactory() *DNSFactory { return &DNSFactory{provider: newDNSProvider()} }

// Provider returns the shared provider the factory hands out, for configuration and
// inspection.
func (f *DNSFactory) Provider() *DNSProvider { return f.provider }

// SetVerifyError makes the shared provider reject VerifyAccess with err.
func (f *DNSFactory) SetVerifyError(err error) {
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	f.provider.verifyErr = err
}

// SetEnsureError makes the shared provider fail EnsureRecord with err.
func (f *DNSFactory) SetEnsureError(err error) {
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	f.provider.ensureErr = err
}

// SetDeleteError makes the shared provider fail DeleteRecord with err.
func (f *DNSFactory) SetDeleteError(err error) {
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	f.provider.deleteErr = err
}

// SetFactoryError makes DNS(...) return err without handing out a provider.
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
	return f.provider, nil
}
