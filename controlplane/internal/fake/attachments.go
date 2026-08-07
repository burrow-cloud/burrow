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

// The name an attachment's connection string is written under (ADR-0031, issue #462), in memory.
// Keyed by (addon, app, environment) exactly as the store is, and — like the store — a missing entry
// means DATABASE_URL, because that is what every attachment made before the name was a choice used.

// attachmentKey is the fake's composite key, matching the store's primary key.
func attachmentKey(addon, app, env string) string { return addon + "\x00" + app + "\x00" + env }

// AddonEnvKey returns the variable name app's attachment to addon in env was written under.
func (d *Database) AddonEnvKey(ctx context.Context, addon, app, env string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAddonEnvKey]; err != nil {
		return "", err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	key, ok := d.attachments[attachmentKey(addon, app, env)]
	if !ok || key == "" {
		return controlplane.AppDatabaseURLKey, nil
	}
	return key, nil
}

// SetAddonEnvKey records the variable name app's attachment to addon in env is written under.
func (d *Database) SetAddonEnvKey(ctx context.Context, addon, app, env, key string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetAddonEnvKey]; err != nil {
		return err
	}
	if app == "" {
		return fmt.Errorf("database: set attachment key: empty app")
	}
	if key == "" {
		return fmt.Errorf("database: set attachment key for %q: empty key", app)
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	d.attachments[attachmentKey(addon, app, env)] = key
	return nil
}

// DeleteAddonEnvKey forgets the recorded name for one attachment. Deleting one that was never
// recorded is a no-op.
func (d *Database) DeleteAddonEnvKey(ctx context.Context, addon, app, env string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDeleteAddonEnvKey]; err != nil {
		return err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	delete(d.attachments, attachmentKey(addon, app, env))
	return nil
}

// DeleteAppAttachments forgets every recorded name for app.
func (d *Database) DeleteAppAttachments(ctx context.Context, app string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.attachments {
		parts := strings.Split(k, "\x00")
		if len(parts) == 3 && parts[1] == app {
			delete(d.attachments, k)
		}
	}
	return nil
}
