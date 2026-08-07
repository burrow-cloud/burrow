// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// The store side of issue #462: the one fact an attachment could not previously carry, which is the
// name of the variable its connection string was written under.

// TestAddonEnvKeyDefaultsToDatabaseURL is the compatibility property, and it is the whole reason a
// missing row is not an error. Every attachment made before this table existed has no row and was
// written to DATABASE_URL, so the store must answer for them.
func TestAddonEnvKeyDefaultsToDatabaseURL(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	app := t.Name()

	key, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonEnvKey with no row: %v", err)
	}
	if key != cp.AppDatabaseURLKey {
		t.Errorf("key = %q, want %q for an attachment that named nothing", key, cp.AppDatabaseURLKey)
	}
}

// TestAddonEnvKeyRoundTrip covers the write, the upsert a rename performs, and the delete a detach
// performs — including that deleting returns the row to the default rather than to an error.
func TestAddonEnvKeyRoundTrip(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	app := t.Name()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	if err := s.SetAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, "DB_URL", now); err != nil {
		t.Fatalf("SetAddonEnvKey: %v", err)
	}
	if key, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment); err != nil || key != "DB_URL" {
		t.Fatalf("AddonEnvKey = %q, %v; want DB_URL", key, err)
	}
	// A rename upserts rather than colliding on the primary key.
	if err := s.SetAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, "PG_DSN", now.Add(time.Hour)); err != nil {
		t.Fatalf("SetAddonEnvKey (rename): %v", err)
	}
	if key, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment); err != nil || key != "PG_DSN" {
		t.Fatalf("AddonEnvKey after rename = %q, %v; want PG_DSN", key, err)
	}
	if err := s.DeleteAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment); err != nil {
		t.Fatalf("DeleteAddonEnvKey: %v", err)
	}
	if key, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment); err != nil || key != cp.AppDatabaseURLKey {
		t.Fatalf("AddonEnvKey after delete = %q, %v; want the default", key, err)
	}
	// Deleting again is a no-op, which is what a detach of an attachment that used the default does.
	if err := s.DeleteAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment); err != nil {
		t.Errorf("DeleteAddonEnvKey (absent row): %v", err)
	}
}

// TestAddonEnvKeyIsPerEnvironment: each environment has its own instance, so the same app may read a
// different variable in staging and in production, and one must not answer for the other.
func TestAddonEnvKeyIsPerEnvironment(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	app := t.Name()
	now := time.Now().UTC()
	staging := envLabel(t, "staging")

	if err := s.SetAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, "DB_URL", now); err != nil {
		t.Fatalf("SetAddonEnvKey(prod): %v", err)
	}
	if key, err := s.AddonEnvKey(ctx, "postgres", app, staging); err != nil || key != cp.AppDatabaseURLKey {
		t.Fatalf("AddonEnvKey(staging) = %q, %v; want the default — production's choice is not staging's", key, err)
	}
}

// TestDeleteAppAttachmentsForgetsEveryEnvironment is the durable side of an app teardown: an app
// created later under the same name starts from Burrow's default rather than a previous occupant's
// choice.
func TestDeleteAppAttachmentsForgetsEveryEnvironment(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	app := t.Name()
	now := time.Now().UTC()
	staging := envLabel(t, "staging")

	for _, env := range []string{cp.DefaultEnvironment, staging} {
		if err := s.SetAddonEnvKey(ctx, "postgres", app, env, "DB_URL", now); err != nil {
			t.Fatalf("SetAddonEnvKey(%s): %v", env, err)
		}
	}
	if err := s.DeleteAppAttachments(ctx, app); err != nil {
		t.Fatalf("DeleteAppAttachments: %v", err)
	}
	for _, env := range []string{cp.DefaultEnvironment, staging} {
		if key, err := s.AddonEnvKey(ctx, "postgres", app, env); err != nil || key != cp.AppDatabaseURLKey {
			t.Errorf("AddonEnvKey(%s) after teardown = %q, %v; want the default", env, key, err)
		}
	}
}
