// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The name an attachment's connection string is written under (ADR-0031, issue #462), in memory.
// Keyed by (addon, app, environment, instance) exactly as the store is, and — like the store — a
// missing entry is reported as missing rather than defaulted: an app may hold several attachments in
// one environment (ADR-0091 §3), and only the engine knows which instance an unrecorded attachment
// can belong to.

// attachmentKey is the fake's composite key, matching the store's primary key.
func attachmentKey(addon, app, env, instance string) string {
	return addon + "\x00" + app + "\x00" + env + "\x00" + instance
}

// AddonEnvKey returns the variable name app's attachment to addon's instance in env was written
// under, and whether a row was recorded at all.
func (d *Database) AddonEnvKey(ctx context.Context, addon, app, env, instance string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpAddonEnvKey]; err != nil {
		return "", false, err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	key, ok := d.attachments[attachmentKey(addon, app, env, instance)]
	if !ok || key == "" {
		return "", false, nil
	}
	return key, true, nil
}

// SetAddonEnvKey records the variable name app's attachment to addon's instance in env is written
// under.
func (d *Database) SetAddonEnvKey(ctx context.Context, addon, app, env, instance, key string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetAddonEnvKey]; err != nil {
		return err
	}
	if app == "" {
		return fmt.Errorf("database: set attachment key: empty app")
	}
	if instance == "" {
		return fmt.Errorf("database: set attachment key for %q: empty instance", app)
	}
	if key == "" {
		return fmt.Errorf("database: set attachment key for %q: empty key", app)
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	d.attachments[attachmentKey(addon, app, env, instance)] = key
	return nil
}

// DeleteAddonEnvKey forgets the recorded name for one attachment. Deleting one that was never
// recorded is a no-op, and every other attachment of the same app is left alone.
func (d *Database) DeleteAddonEnvKey(ctx context.Context, addon, app, env, instance string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDeleteAddonEnvKey]; err != nil {
		return err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	delete(d.attachments, attachmentKey(addon, app, env, instance))
	return nil
}

// AppAttachments returns every recorded attachment app holds to addon in env, instance order.
func (d *Database) AppAttachments(ctx context.Context, addon, app, env string) ([]controlplane.AddonAttachment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	out := []controlplane.AddonAttachment{}
	for k, key := range d.attachments {
		parts := strings.Split(k, "\x00")
		if len(parts) != 4 || parts[0] != addon || parts[1] != app || parts[2] != env {
			continue
		}
		out = append(out, controlplane.AddonAttachment{
			Addon:       controlplane.AddonType(addon),
			App:         app,
			Environment: env,
			Instance:    parts[3],
			SecretKey:   key,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out, nil
}

// DeleteAppAttachments forgets every recorded name for app.
func (d *Database) DeleteAppAttachments(ctx context.Context, app string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.attachments {
		parts := strings.Split(k, "\x00")
		if len(parts) == 4 && parts[1] == app {
			delete(d.attachments, k)
		}
	}
	return nil
}
