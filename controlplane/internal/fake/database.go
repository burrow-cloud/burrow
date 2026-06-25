// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Database = (*Database)(nil)

// Database is an in-memory controlplane.Database. It stores releases by ID and tracks
// per-app save order so LatestRelease and Releases are deterministic. Records are deep
// copied in and out, so callers never share Env/Command memory with the store — the
// same isolation a real database gives. Errors can be injected per operation.
type Database struct {
	mu     sync.Mutex
	byID   map[string]controlplane.Release
	order  map[string][]string // app -> release IDs, save order, deduplicated
	errs   map[Op]error
	policy controlplane.Policy
}

// NewDatabase returns an empty fake database with the default guardrail policy.
func NewDatabase() *Database {
	return &Database{
		byID:   make(map[string]controlplane.Release),
		order:  make(map[string][]string),
		errs:   make(map[Op]error),
		policy: controlplane.DefaultPolicy(),
	}
}

// SetPolicy replaces the whole guardrail policy. It is a test helper for arranging a
// specific policy before exercising the engine.
func (d *Database) SetPolicy(p controlplane.Policy) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.policy = p
}

// Policy returns the current guardrail policy.
func (d *Database) Policy(ctx context.Context) (controlplane.Policy, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpPolicy]; err != nil {
		return controlplane.Policy{}, err
	}
	return d.policy, nil
}

// SetGuardrail persists one guardrail's disposition, overlaying it on the current policy.
func (d *Database) SetGuardrail(ctx context.Context, code controlplane.GuardrailCode, disp controlplane.Disposition) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetGuardrail]; err != nil {
		return err
	}
	if !disp.Valid() {
		return fmt.Errorf("database: set guardrail: invalid disposition %q", disp)
	}
	d.policy = d.policy.With(code, disp)
	return nil
}

// SetError makes op return err until cleared with SetError(op, nil).
func (d *Database) SetError(op Op, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.errs, op)
		return
	}
	d.errs[op] = err
}

func (d *Database) SaveRelease(ctx context.Context, r controlplane.Release) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSaveRelease]; err != nil {
		return err
	}
	if r.ID == "" {
		return fmt.Errorf("database: save release: empty ID")
	}
	if _, exists := d.byID[r.ID]; !exists {
		d.order[r.App] = append(d.order[r.App], r.ID)
	}
	d.byID[r.ID] = cloneRelease(r)
	return nil
}

func (d *Database) Release(ctx context.Context, id string) (controlplane.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpRelease]; err != nil {
		return controlplane.Release{}, err
	}
	r, ok := d.byID[id]
	if !ok {
		return controlplane.Release{}, fmt.Errorf("database: release %q: %w", id, controlplane.ErrNotFound)
	}
	return cloneRelease(r), nil
}

func (d *Database) LatestRelease(ctx context.Context, app string) (controlplane.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpLatestRelease]; err != nil {
		return controlplane.Release{}, err
	}
	ids := d.order[app]
	if len(ids) == 0 {
		return controlplane.Release{}, fmt.Errorf("database: latest release for app %q: %w", app, controlplane.ErrNotFound)
	}
	return cloneRelease(d.byID[ids[len(ids)-1]]), nil
}

func (d *Database) Releases(ctx context.Context, app string) ([]controlplane.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpReleases]; err != nil {
		return nil, err
	}
	ids := d.order[app]
	out := make([]controlplane.Release, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneRelease(d.byID[id]))
	}
	return out, nil
}
