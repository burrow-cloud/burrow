// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The store side of the lock (cloud ADR-0060): which apps and add-on instances are protected from
// the operations that cannot be undone. A row exists only where somebody locked something, so the
// table holds the exceptions rather than a flag per app.

// Lock returns the lock on one subject in one environment, or ErrNotFound when it is not locked.
//
// ABSENCE IS ErrNotFound RATHER THAN A ZERO VALUE, and that is deliberate on the read the
// destructive path makes. A method that answered (Lock{}, nil) for "no row" would return the same
// pair a scan error could be mistaken for, and the caller's mistake — reading "I do not know" as
// "not locked" — destroys the one thing somebody asked Burrow to protect. A sentinel cannot be read
// that way by accident.
func (s *Store) Lock(ctx context.Context, subject controlplane.LockSubject, env, name string) (controlplane.Lock, error) {
	const q = `SELECT subject, environment, name, locked_at FROM locks WHERE subject = $1 AND environment = $2 AND name = $3`
	var l controlplane.Lock
	err := s.db.QueryRowContext(ctx, q, string(subject), env, name).Scan(&l.Subject, &l.Environment, &l.Name, &l.LockedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.Lock{}, fmt.Errorf("postgres: no lock on %s %q in %q: %w", subject, name, env, controlplane.ErrNotFound)
	}
	if err != nil {
		return controlplane.Lock{}, fmt.Errorf("postgres: lock on %s %q in %q: %w", subject, name, env, err)
	}
	return l, nil
}

// SetLock locks a subject, leaving an existing lock's timestamp alone. The DO NOTHING is the
// contract rather than an optimization: locked_at answers "since when has this been protected", and
// a second person asserting the same protection has not changed that answer.
func (s *Store) SetLock(ctx context.Context, lock controlplane.Lock) error {
	if lock.Name == "" {
		return fmt.Errorf("postgres: set lock: empty name")
	}
	if lock.Subject == "" {
		return fmt.Errorf("postgres: set lock: empty subject")
	}
	const q = `
INSERT INTO locks (subject, environment, name, locked_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (subject, environment, name) DO NOTHING`
	if _, err := s.db.ExecContext(ctx, q, string(lock.Subject), lock.Environment, lock.Name, lock.LockedAt); err != nil {
		return fmt.Errorf("postgres: set lock on %s %q in %q: %w", lock.Subject, lock.Name, lock.Environment, err)
	}
	return nil
}

// DeleteLock removes a subject's lock. Removing one that does not exist is a no-op: it is the state
// the caller asked for, and an error would make the unlock of an already-unlocked thing look like a
// failure to somebody who is about to type a destructive command.
func (s *Store) DeleteLock(ctx context.Context, subject controlplane.LockSubject, env, name string) error {
	const q = `DELETE FROM locks WHERE subject = $1 AND environment = $2 AND name = $3`
	if _, err := s.db.ExecContext(ctx, q, string(subject), env, name); err != nil {
		return fmt.Errorf("postgres: delete lock on %s %q in %q: %w", subject, name, env, err)
	}
	return nil
}

// Locks returns the locks of one subject kind, name order. An empty env returns every environment's,
// which is what the add-ons listing needs: it spans environments, and a set narrowed to one would
// report every instance outside it as unlocked.
func (s *Store) Locks(ctx context.Context, subject controlplane.LockSubject, env string) ([]controlplane.Lock, error) {
	const q = `
SELECT subject, environment, name, locked_at FROM locks
WHERE subject = $1 AND ($2 = '' OR environment = $2)
ORDER BY environment, name`
	rows, err := s.db.QueryContext(ctx, q, string(subject), env)
	if err != nil {
		return nil, fmt.Errorf("postgres: locks on %s: %w", subject, err)
	}
	defer rows.Close()
	var locks []controlplane.Lock
	for rows.Next() {
		var l controlplane.Lock
		if err := rows.Scan(&l.Subject, &l.Environment, &l.Name, &l.LockedAt); err != nil {
			return nil, fmt.Errorf("postgres: scanning lock: %w", err)
		}
		locks = append(locks, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: locks on %s: %w", subject, err)
	}
	return locks, nil
}
