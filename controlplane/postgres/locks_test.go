// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// The store side of the lock (cloud ADR-0060). Every name is scoped to the test's own name, so these
// are safe against the shared database the suite runs on.

func lockName(t *testing.T, suffix string) string {
	t.Helper()
	return strings.ToLower(t.Name()) + "-" + suffix
}

// TestStoreLockAbsenceIsNotFound is the property the destructive path depends on: a thing nobody
// locked answers ErrNotFound rather than a zero value, so no caller can read "I have no answer" as
// "it is not locked".
func TestStoreLockAbsenceIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	_, err := s.Lock(ctx, cp.LockSubjectApp, "prod", lockName(t, "web"))
	if !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Lock on an unlocked app err = %v, want ErrNotFound", err)
	}
}

// TestStoreLockRoundTrip: lock, read it back, unlock, and read the absence back. Unlocking something
// that is not locked is a no-op rather than an error — it is the state the caller asked for.
func TestStoreLockRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := lockName(t, "web")
	at := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)

	if err := s.SetLock(ctx, cp.Lock{Subject: cp.LockSubjectApp, Environment: "prod", Name: app, LockedAt: at}); err != nil {
		t.Fatalf("SetLock: %v", err)
	}
	got, err := s.Lock(ctx, cp.LockSubjectApp, "prod", app)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if got.Name != app || got.Subject != cp.LockSubjectApp || got.Environment != "prod" || !got.LockedAt.Equal(at) {
		t.Errorf("Lock = %+v, want the row that was written", got)
	}

	if err := s.DeleteLock(ctx, cp.LockSubjectApp, "prod", app); err != nil {
		t.Fatalf("DeleteLock: %v", err)
	}
	if _, err := s.Lock(ctx, cp.LockSubjectApp, "prod", app); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("after unlock err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteLock(ctx, cp.LockSubjectApp, "prod", app); err != nil {
		t.Errorf("unlocking something that is not locked errored: %v", err)
	}
}

// TestStoreLockKeepsItsFirstTimestamp: a second lock leaves the first one's time alone. The row
// answers "since when has this been protected", and a second person asserting the same protection is
// not a new answer to that question.
func TestStoreLockKeepsItsFirstTimestamp(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := lockName(t, "web")
	first := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	later := first.Add(72 * time.Hour)

	for _, at := range []time.Time{first, later} {
		if err := s.SetLock(ctx, cp.Lock{Subject: cp.LockSubjectApp, Environment: "prod", Name: app, LockedAt: at}); err != nil {
			t.Fatalf("SetLock(%s): %v", at, err)
		}
	}
	got, err := s.Lock(ctx, cp.LockSubjectApp, "prod", app)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !got.LockedAt.Equal(first) {
		t.Errorf("locked_at = %s, want the first lock's time %s", got.LockedAt, first)
	}
}

// TestStoreLocksAreScopedBySubjectAndEnvironment: an app and an add-on instance of the same name are
// two locks, and the same name in two environments is two locks. The second half is what keeps a
// lock on production from reading as a lock on staging — the mistake the mechanism exists for.
func TestStoreLocksAreScopedBySubjectAndEnvironment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	name := lockName(t, "shared")
	at := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)

	if err := s.SetLock(ctx, cp.Lock{Subject: cp.LockSubjectApp, Environment: "prod", Name: name, LockedAt: at}); err != nil {
		t.Fatalf("SetLock(app/prod): %v", err)
	}
	if _, err := s.Lock(ctx, cp.LockSubjectAddonInstance, "prod", name); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("an app lock answered for an add-on instance of the same name: %v", err)
	}
	if _, err := s.Lock(ctx, cp.LockSubjectApp, "staging", name); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("a lock in prod answered for staging: %v", err)
	}

	// The listing narrows the same way, and an empty environment spans them all — what the add-ons
	// listing needs, since it reports every environment's instances in one table.
	if err := s.SetLock(ctx, cp.Lock{Subject: cp.LockSubjectApp, Environment: "staging", Name: name, LockedAt: at}); err != nil {
		t.Fatalf("SetLock(app/staging): %v", err)
	}
	prod, err := s.Locks(ctx, cp.LockSubjectApp, "prod")
	if err != nil {
		t.Fatalf("Locks(prod): %v", err)
	}
	if len(matching(prod, name)) != 1 {
		t.Errorf("Locks(prod) for %q = %d rows, want 1", name, len(matching(prod, name)))
	}
	all, err := s.Locks(ctx, cp.LockSubjectApp, "")
	if err != nil {
		t.Fatalf("Locks(all): %v", err)
	}
	if len(matching(all, name)) != 2 {
		t.Errorf("Locks(all environments) for %q = %d rows, want 2", name, len(matching(all, name)))
	}
}

// matching narrows a listing to the rows this test wrote, so the shared database's other rows do not
// decide the assertion.
func matching(locks []cp.Lock, name string) []cp.Lock {
	var out []cp.Lock
	for _, l := range locks {
		if l.Name == name {
			out = append(out, l)
		}
	}
	return out
}
