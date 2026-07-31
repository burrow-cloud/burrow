// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// Whether the deploy-time dependency check runs for an app (ADR-0076 §4), in memory. Keyed by
// (app, environment) exactly as the store is, and — like the store — a missing entry means ENABLED,
// because the check is Burrow's default rather than something a user opted into.

// DependencyChecksEnabled reports whether the deploy-time dependency check runs for app in env.
func (d *Database) DependencyChecksEnabled(ctx context.Context, app, env string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDependencyChecksEnabled]; err != nil {
		return false, err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	enabled, ok := d.depChecks[exposureKey(app, env)]
	if !ok {
		return true, nil
	}
	return enabled, nil
}

// SetDependencyChecks records whether the check runs for app in env.
func (d *Database) SetDependencyChecks(ctx context.Context, app, env string, enabled bool, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetDependencyChecks]; err != nil {
		return err
	}
	if app == "" {
		return fmt.Errorf("database: set dependency checks: empty app")
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	d.depChecks[exposureKey(app, env)] = enabled
	return nil
}

// DeleteDependencyCheckSettings removes app's setting across all environments.
func (d *Database) DeleteDependencyCheckSettings(ctx context.Context, app string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.depChecks {
		if name, _, _ := strings.Cut(k, "\x00"); name == app {
			delete(d.depChecks, k)
		}
	}
	return nil
}
