// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sort"

	"github.com/burrow-cloud/burrow/controlplane"
)

// What is locked (cloud ADR-0060), in memory. Keyed by (subject, environment, name) exactly as the
// store is, and — like the store — an absent entry means UNLOCKED, reported as ErrNotFound so a
// caller cannot read "no answer" as "not locked".

// lockKey keys one lock the way the store's primary key does.
func lockKey(subject controlplane.LockSubject, env, name string) string {
	return string(subject) + "\x00" + env + "\x00" + name
}

// Lock returns the lock on one subject in env, or ErrNotFound when it is not locked.
func (d *Database) Lock(ctx context.Context, subject controlplane.LockSubject, env, name string) (controlplane.Lock, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpLock]; err != nil {
		return controlplane.Lock{}, err
	}
	l, ok := d.locks[lockKey(subject, env, name)]
	if !ok {
		return controlplane.Lock{}, fmt.Errorf("database: no lock on %s %q in %q: %w", subject, name, env, controlplane.ErrNotFound)
	}
	return l, nil
}

// SetLock locks a subject, leaving an existing lock's timestamp alone.
func (d *Database) SetLock(ctx context.Context, lock controlplane.Lock) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpSetLock]; err != nil {
		return err
	}
	if lock.Name == "" {
		return fmt.Errorf("database: set lock: empty name")
	}
	if lock.Subject == "" {
		return fmt.Errorf("database: set lock: empty subject")
	}
	key := lockKey(lock.Subject, lock.Environment, lock.Name)
	if _, ok := d.locks[key]; ok {
		return nil
	}
	d.locks[key] = lock
	return nil
}

// DeleteLock removes a subject's lock; removing one that is not there is a no-op.
func (d *Database) DeleteLock(ctx context.Context, subject controlplane.LockSubject, env, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpDeleteLock]; err != nil {
		return err
	}
	delete(d.locks, lockKey(subject, env, name))
	return nil
}

// Locks returns the locks of one subject kind, environment then name order. An empty env returns
// every environment's.
func (d *Database) Locks(ctx context.Context, subject controlplane.LockSubject, env string) ([]controlplane.Lock, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[OpLocks]; err != nil {
		return nil, err
	}
	var out []controlplane.Lock
	for _, l := range d.locks {
		if l.Subject != subject || (env != "" && l.Environment != env) {
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Environment != out[j].Environment {
			return out[i].Environment < out[j].Environment
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
