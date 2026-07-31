// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The declared health endpoint (ADR-0076 §5) and the targeted exposure read the readiness default
// (§3) needs, in memory. Both are keyed by (app, environment) exactly as the store is.

// Exposure returns the recorded exposure for app in env, or ErrNotFound when the app is not
// published there — matching the store, where "not published" is the ordinary answer rather than a
// failure.
func (d *Database) Exposure(ctx context.Context, app, env string) (controlplane.Exposure, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpExposure]; err != nil {
		return controlplane.Exposure{}, err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	ex, ok := d.exposures[exposureKey(app, env)]
	if !ok {
		return controlplane.Exposure{}, fmt.Errorf("fake: exposure for %q in %q: %w", app, env, controlplane.ErrNotFound)
	}
	return ex, nil
}

// HealthEndpoint returns the endpoint declared for app in env, or the zero value when none was —
// not an error, since no endpoint is where every app starts.
func (d *Database) HealthEndpoint(ctx context.Context, app, env string) (controlplane.HealthEndpoint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpHealthEndpoint]; err != nil {
		return controlplane.HealthEndpoint{}, err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	return d.health[exposureKey(app, env)], nil
}

// SetHealthEndpoint upserts the declared endpoint for app in env.
func (d *Database) SetHealthEndpoint(ctx context.Context, ep controlplane.HealthEndpoint) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetHealthEndpoint]; err != nil {
		return err
	}
	if ep.App == "" {
		return fmt.Errorf("database: set health endpoint: empty app")
	}
	if ep.Environment == "" {
		ep.Environment = controlplane.DefaultEnvironment
	}
	d.health[exposureKey(ep.App, ep.Environment)] = ep
	return nil
}

// UnsetHealthEndpoint removes the declared endpoint for app in env. Removing one that was never
// declared is a no-op, matching the store.
func (d *Database) UnsetHealthEndpoint(ctx context.Context, app, env string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpUnsetHealthEndpoint]; err != nil {
		return err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	delete(d.health, exposureKey(app, env))
	return nil
}

// DeleteHealthEndpoints removes every declared endpoint for app across all environments.
func (d *Database) DeleteHealthEndpoints(ctx context.Context, app string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, ep := range d.health {
		if ep.App == app {
			delete(d.health, k)
		}
	}
	return nil
}
