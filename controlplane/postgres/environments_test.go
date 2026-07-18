// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestStoreEnvironments(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	// Names are prefixed with the test name so the test is isolated and idempotent against the
	// shared database (matching the other store tests).
	staging := t.Name() + "-staging"
	prod := t.Name() + "-prod"

	if err := s.CreateEnvironment(ctx, staging, staging+"-ns"); err != nil {
		t.Fatalf("CreateEnvironment(staging): %v", err)
	}
	if err := s.CreateEnvironment(ctx, prod, prod+"-ns"); err != nil {
		t.Fatalf("CreateEnvironment(prod): %v", err)
	}

	// A duplicate name is rejected as ErrInvalid.
	if err := s.CreateEnvironment(ctx, staging, "other-ns"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("duplicate CreateEnvironment err = %v, want ErrInvalid", err)
	}

	// GetEnvironment returns the stored row; an unknown name is ErrNotFound.
	got, err := s.GetEnvironment(ctx, prod)
	if err != nil || got.Name != prod || got.Namespace != prod+"-ns" {
		t.Fatalf("GetEnvironment(prod) = %+v, err=%v", got, err)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("GetEnvironment created_at should be set, got zero")
	}
	if _, err := s.GetEnvironment(ctx, t.Name()+"-missing"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("GetEnvironment(missing) err = %v, want ErrNotFound", err)
	}

	// ListEnvironments returns rows ordered by name; the two we created appear, prod before staging.
	envs, err := s.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	idxProd, idxStaging := -1, -1
	for i, e := range envs {
		switch e.Name {
		case prod:
			idxProd = i
		case staging:
			idxStaging = i
		}
	}
	if idxProd < 0 || idxStaging < 0 {
		t.Fatalf("ListEnvironments missing created rows: %+v", envs)
	}
	if idxProd > idxStaging {
		t.Errorf("ListEnvironments not name-ordered: prod at %d, staging at %d", idxProd, idxStaging)
	}

	// DeleteEnvironment removes the row; a subsequent get and a second delete both report ErrNotFound.
	if err := s.DeleteEnvironment(ctx, prod); err != nil {
		t.Fatalf("DeleteEnvironment(prod): %v", err)
	}
	if _, err := s.GetEnvironment(ctx, prod); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("GetEnvironment(deleted) err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteEnvironment(ctx, prod); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("DeleteEnvironment(missing) err = %v, want ErrNotFound", err)
	}
}
