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

// An app's secret keys projected as FILES (ADR-0089 §5), in memory. Keyed by (app, environment)
// exactly as the store is, and — like the store — holding key names and filenames and never a value:
// the values live in the fake Kubernetes' Secret map, and this side of the seam never reads them.

// SecretMounts returns app's file projection in env, sorted by key. An app that mounts nothing
// yields the zero value and no error, which is where every app starts.
func (d *Database) SecretMounts(ctx context.Context, app, env string) (controlplane.SecretMounts, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSecretMounts]; err != nil {
		return controlplane.SecretMounts{}, err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	out := controlplane.SecretMounts{Dir: d.secretDirs[exposureKey(app, env)]}
	for _, m := range d.secretMounts[exposureKey(app, env)] {
		out.Mounts = append(out.Mounts, m)
	}
	out.Sort()
	return out, nil
}

// SetSecretMount upserts one key's projection for app in env.
func (d *Database) SetSecretMount(ctx context.Context, m controlplane.SecretMount) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetSecretMount]; err != nil {
		return err
	}
	if m.App == "" {
		return fmt.Errorf("database: set secret mount: empty app")
	}
	if m.Environment == "" {
		m.Environment = controlplane.DefaultEnvironment
	}
	key := exposureKey(m.App, m.Environment)
	if d.secretMounts[key] == nil {
		d.secretMounts[key] = map[string]controlplane.SecretMount{}
	}
	d.secretMounts[key][m.Key] = m
	return nil
}

// UnsetSecretMount stops projecting one key as a file. Unmounting a key that was not mounted is a
// no-op, matching the store.
func (d *Database) UnsetSecretMount(ctx context.Context, app, env, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpUnsetSecretMount]; err != nil {
		return err
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	delete(d.secretMounts[exposureKey(app, env)], key)
	return nil
}

// SetSecretsDir records the one directory app's mounted keys land in, in env.
func (d *Database) SetSecretsDir(ctx context.Context, app, env, dir string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetSecretsDir]; err != nil {
		return err
	}
	if app == "" {
		return fmt.Errorf("database: set secrets directory: empty app")
	}
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	d.secretDirs[exposureKey(app, env)] = dir
	return nil
}

// DeleteSecretMounts removes every mount and directory override for app across all environments.
func (d *Database) DeleteSecretMounts(ctx context.Context, app string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, mounts := range d.secretMounts {
		for _, m := range mounts {
			if m.App == app {
				delete(d.secretMounts, k)
				break
			}
		}
	}
	for k := range d.secretDirs {
		if name, _, _ := strings.Cut(k, "\x00"); name == app {
			delete(d.secretDirs, k)
		}
	}
	return nil
}
