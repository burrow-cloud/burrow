// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.DatabaseProvisioner = (*Provisioner)(nil)

// Provisioner is an in-memory controlplane.DatabaseProvisioner. It records the apps it provisioned
// and returns a deterministic connection string per app, so an attach test can assert the engine
// called EnsureAppDatabase and threaded the URL into the secret path without standing up Postgres.
// Errors can be injected to exercise the failure path.
type Provisioner struct {
	mu        sync.Mutex
	ensured   []string // apps passed to EnsureAppDatabase, in call order
	dropped   []string // apps passed to DropAppDatabase, in call order
	ensureErr error
	dropErr   error
}

// NewProvisioner returns an empty fake provisioner.
func NewProvisioner() *Provisioner { return &Provisioner{} }

// SetEnsureError makes EnsureAppDatabase return err (nil clears it).
func (p *Provisioner) SetEnsureError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureErr = err
}

// SetDropError makes DropAppDatabase return err (nil clears it).
func (p *Provisioner) SetDropError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropErr = err
}

// Ensured returns the apps EnsureAppDatabase was called with, in order.
func (p *Provisioner) Ensured() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ensured...)
}

// Dropped returns the apps DropAppDatabase was called with, in order.
func (p *Provisioner) Dropped() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.dropped...)
}

// URLFor is the deterministic connection string the fake returns for app — exposed so a test can
// assert the engine wrote exactly this value into the secret without the value leaking elsewhere.
func URLFor(app string) string {
	return fmt.Sprintf("postgres://app_%s:fakepw@burrow-postgres.burrow-addons.svc:5432/%s?sslmode=disable", app, app)
}

func (p *Provisioner) EnsureAppDatabase(_ context.Context, app string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ensureErr != nil {
		return "", p.ensureErr
	}
	p.ensured = append(p.ensured, app)
	return URLFor(app), nil
}

func (p *Provisioner) DropAppDatabase(_ context.Context, app string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dropErr != nil {
		return p.dropErr
	}
	p.dropped = append(p.dropped, app)
	return nil
}
