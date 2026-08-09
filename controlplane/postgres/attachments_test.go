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
// name of the variable its connection string was written under — now keyed by the instance it is
// against, since an app may hold several in one environment (ADR-0091 §3).

// TestAddonEnvKeyReportsAnUnrecordedAttachmentAsUnrecorded is the property the engine's
// compatibility rests on, and it is why this returns a second value rather than a default.
//
// A missing row means DATABASE_URL for an attachment made before the name was a choice, and ONLY on
// the environment's default instance — the only instance those attachments can be against. The store
// cannot know which instance that is, so it reports the absence and the engine resolves it. A store
// that answered DATABASE_URL here would tell a second attach it already owns the variable the first
// one holds.
func TestAddonEnvKeyReportsAnUnrecordedAttachmentAsUnrecorded(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	app := t.Name()

	key, recorded, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, defaultInstance(cp.DefaultEnvironment))
	if err != nil {
		t.Fatalf("AddonEnvKey with no row: %v", err)
	}
	if recorded || key != "" {
		t.Errorf("key = %q, recorded = %v; want an unrecorded attachment to say so", key, recorded)
	}
}

// TestAddonEnvKeyRoundTrip covers the write, the upsert a rename performs, and the delete a detach
// performs.
func TestAddonEnvKeyRoundTrip(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	app := t.Name()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	instance := defaultInstance(cp.DefaultEnvironment)

	if err := s.SetAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, instance, "DB_URL", now); err != nil {
		t.Fatalf("SetAddonEnvKey: %v", err)
	}
	if key, recorded, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, instance); err != nil || !recorded || key != "DB_URL" {
		t.Fatalf("AddonEnvKey = %q, %v, %v; want DB_URL recorded", key, recorded, err)
	}
	// A rename upserts rather than colliding on the primary key.
	if err := s.SetAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, instance, "PG_DSN", now.Add(time.Hour)); err != nil {
		t.Fatalf("SetAddonEnvKey (rename): %v", err)
	}
	if key, _, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, instance); err != nil || key != "PG_DSN" {
		t.Fatalf("AddonEnvKey after rename = %q, %v; want PG_DSN", key, err)
	}
	if err := s.DeleteAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, instance); err != nil {
		t.Fatalf("DeleteAddonEnvKey: %v", err)
	}
	if _, recorded, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, instance); err != nil || recorded {
		t.Fatalf("AddonEnvKey after delete: recorded = %v, %v; want unrecorded", recorded, err)
	}
	// Deleting again is a no-op, which is what a detach of an attachment that used the default does.
	if err := s.DeleteAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, instance); err != nil {
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

	if err := s.SetAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, defaultInstance(cp.DefaultEnvironment), "DB_URL", now); err != nil {
		t.Fatalf("SetAddonEnvKey(prod): %v", err)
	}
	if _, recorded, err := s.AddonEnvKey(ctx, "postgres", app, staging, defaultInstance(staging)); err != nil || recorded {
		t.Fatalf("AddonEnvKey(staging) recorded = %v, %v; want nothing — production's choice is not staging's", recorded, err)
	}
}

// TestAddonEnvKeyIsPerInstance is ADR-0091 §3's attachment key, one level below the environment: an
// app attached to two instances in ONE environment holds two attachments, each under its own
// variable, and neither answers for the other.
//
// It is the property that makes a second attachment expressible at all. With the old key the second
// attach would have found the first one's row, decided it already owned `DATABASE_URL`, and
// overwritten the first attachment's connection string with a credential for another server.
func TestAddonEnvKeyIsPerInstance(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	app := t.Name()
	now := time.Now().UTC()
	first := defaultInstance(cp.DefaultEnvironment)
	second := "burrow-postgres-an4lyt"

	if err := s.SetAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, first, cp.AppDatabaseURLKey, now); err != nil {
		t.Fatalf("SetAddonEnvKey(first): %v", err)
	}
	if err := s.SetAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, second, "ANALYTICS_URL", now); err != nil {
		t.Fatalf("SetAddonEnvKey(second): %v", err)
	}
	if key, _, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, first); err != nil || key != cp.AppDatabaseURLKey {
		t.Errorf("first attachment = %q, %v; want %s", key, err, cp.AppDatabaseURLKey)
	}
	if key, _, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, second); err != nil || key != "ANALYTICS_URL" {
		t.Errorf("second attachment = %q, %v; want ANALYTICS_URL", key, err)
	}

	// And both are listed, so an operation that has to act on all of an app's databases can find
	// them.
	all, err := s.AppAttachments(ctx, "postgres", app, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AppAttachments: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("AppAttachments = %d rows, want 2", len(all))
	}

	// Detaching one leaves the other's variable untouched (ADR-0091 §3).
	if err := s.DeleteAddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, second); err != nil {
		t.Fatalf("DeleteAddonEnvKey(second): %v", err)
	}
	if key, recorded, err := s.AddonEnvKey(ctx, "postgres", app, cp.DefaultEnvironment, first); err != nil || !recorded || key != cp.AppDatabaseURLKey {
		t.Errorf("first attachment after detaching the second = %q, %v, %v; want it untouched", key, recorded, err)
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
		if err := s.SetAddonEnvKey(ctx, "postgres", app, env, defaultInstance(env), "DB_URL", now); err != nil {
			t.Fatalf("SetAddonEnvKey(%s): %v", env, err)
		}
	}
	if err := s.DeleteAppAttachments(ctx, app); err != nil {
		t.Fatalf("DeleteAppAttachments: %v", err)
	}
	for _, env := range []string{cp.DefaultEnvironment, staging} {
		if _, recorded, err := s.AddonEnvKey(ctx, "postgres", app, env, defaultInstance(env)); err != nil || recorded {
			t.Errorf("AddonEnvKey(%s) after teardown: recorded = %v, %v; want nothing", env, recorded, err)
		}
	}
}
