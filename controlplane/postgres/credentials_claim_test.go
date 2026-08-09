// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// Claiming the install's first principal (ADR-0084 §2), tested against a schema of its own.
//
// The claim is defined against an EMPTY principals table, which the shared store-test database is
// not, so this runs in a scratch schema migrated from nothing — the same isolation the migration
// tests use. It skips without BURROW_TEST_DATABASE_URL.

// claimStore migrates a scratch schema to the current head and returns a Store over it.
func claimStore(t *testing.T) *Store {
	t.Helper()
	db, provider := scratchSchema(t)
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Store{db: db}
}

// claimCredential is the credential a claim writes alongside its principal: the store records both
// or neither, so every claim here carries one.
func claimCredential(principal string, at time.Time) controlplane.Credential {
	return controlplane.Credential{
		ID:          principal + "-cred",
		PrincipalID: principal,
		Kind:        controlplane.CredentialKindUser,
		TokenHash:   controlplane.HashToken(principal + "-token"),
		CreatedAt:   at,
	}
}

// TestClaimFirstPrincipalHasOneWinner is the property the claim exists for: callers racing to claim
// an install produce exactly one admin. An unlocked read-then-insert would let several through here
// — under READ COMMITTED nobody sees anybody else's uncommitted row, and the names differ so no
// unique constraint catches it — and every one of them would be an admin nobody chose.
func TestClaimFirstPrincipalHasOneWinner(t *testing.T) {
	ctx := context.Background()
	s := claimStore(t)
	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		won, lost int
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("claimer-%d", i)
			err := s.ClaimFirstPrincipal(ctx,
				controlplane.Principal{ID: name, Name: name, Admin: true, CreatedAt: at},
				claimCredential(name, at))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, controlplane.ErrAlreadyClaimed):
				lost++
			default:
				t.Errorf("ClaimFirstPrincipal: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if won != 1 || lost != racers-1 {
		t.Fatalf("%d claims won and %d were refused, want exactly one winner out of %d", won, lost, racers)
	}
	all, err := s.Principals(ctx)
	if err != nil {
		t.Fatalf("Principals: %v", err)
	}
	if len(all) != 1 || !all[0].Admin {
		t.Fatalf("principals = %+v, want exactly one admin", all)
	}
}

// TestClaimFirstPrincipalRefusesAfterAnyPrincipalExists: the window closes on the first principal of
// any kind, not on the first ADMIN. A non-admin recorded first would otherwise leave the claim open
// for somebody to walk into.
func TestClaimFirstPrincipalRefusesAfterAnyPrincipalExists(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	// Each subtest gets its own schema, because scratchSchema names one after the running test.
	t.Run("a non-admin principal closes the window too", func(t *testing.T) {
		s := claimStore(t)
		if err := s.CreatePrincipal(ctx, controlplane.Principal{ID: "dana", Name: "dana", CreatedAt: at}); err != nil {
			t.Fatalf("CreatePrincipal: %v", err)
		}
		err := s.ClaimFirstPrincipal(ctx,
			controlplane.Principal{ID: "late", Name: "late", Admin: true, CreatedAt: at}, claimCredential("late", at))
		if !errors.Is(err, controlplane.ErrAlreadyClaimed) {
			t.Fatalf("ClaimFirstPrincipal after a principal exists = %v, want ErrAlreadyClaimed", err)
		}
	})

	// A REVOKED principal still counts: the row is what says the install has been claimed, and
	// re-opening the window because somebody was retired would be a way back in.
	t.Run("a revoked principal still counts", func(t *testing.T) {
		s := claimStore(t)
		if err := s.ClaimFirstPrincipal(ctx,
			controlplane.Principal{ID: "first", Name: "first", Admin: true, CreatedAt: at}, claimCredential("first", at)); err != nil {
			t.Fatalf("ClaimFirstPrincipal: %v", err)
		}
		if err := s.RevokePrincipal(ctx, "first", at.Add(time.Hour)); err != nil {
			t.Fatalf("RevokePrincipal: %v", err)
		}
		err := s.ClaimFirstPrincipal(ctx,
			controlplane.Principal{ID: "second", Name: "second", Admin: true, CreatedAt: at}, claimCredential("second", at))
		if !errors.Is(err, controlplane.ErrAlreadyClaimed) {
			t.Fatalf("claiming after the first principal was revoked = %v, want ErrAlreadyClaimed", err)
		}
	})
}
