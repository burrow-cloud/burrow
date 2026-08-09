// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// Issue #462: an attach may NAME the variable its connection string is written under, and the name
// is recorded with the attachment rather than re-derived. These are the properties that make the
// name safe to have: the default did not move, the name survives to every operation that acts on the
// attachment, and a name something else already answers to is refused rather than overwritten.

// TestAttachDefaultKeyUnchanged is the promise to every app already running: an attach that names no
// variable writes DATABASE_URL, exactly as it did before the name was a choice, and records nothing
// that would make a later call behave differently.
func TestAttachDefaultKeyUnchanged(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newPostgresEngine(t)

	res, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{})
	if err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if res.SecretKey != cp.AppDatabaseURLKey {
		t.Errorf("secret key = %q, want the unchanged default %q", res.SecretKey, cp.AppDatabaseURLKey)
	}
	if res.PreviousSecretKey != "" {
		t.Errorf("previous secret key = %q, want empty: nothing was renamed", res.PreviousSecretKey)
	}
	if _, ok := k.SecretValue("web", cp.AppDatabaseURLKey); !ok {
		t.Error("DATABASE_URL was not written")
	}
	// And the recorded name for an attachment that named nothing reads back as the default, so an
	// attachment made before this table existed and one made after are indistinguishable.
	key, recorded, err := d.AddonEnvKey(ctx, string(cp.AddonPostgres), "web", cp.DefaultEnvironment, defaultInstance(cp.DefaultEnvironment))
	if err != nil {
		t.Fatalf("AddonEnvKey: %v", err)
	}
	if !recorded || key != cp.AppDatabaseURLKey {
		t.Errorf("recorded key = %q (%v), want %q", key, recorded, cp.AppDatabaseURLKey)
	}
}

// TestAttachWritesNamedKey is the feature: the connection string lands in the variable the app
// actually reads, and DATABASE_URL is not written at all.
func TestAttachWritesNamedKey(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newPostgresEngine(t)

	res, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "PG_DSN"})
	if err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if res.SecretKey != "PG_DSN" {
		t.Errorf("secret key = %q, want PG_DSN", res.SecretKey)
	}
	val, ok := k.SecretValue("web", "PG_DSN")
	if !ok {
		t.Fatal("PG_DSN was not written into the app's Secret")
	}
	if val != fake.URLFor("web", cp.DefaultEnvironment) {
		t.Errorf("stored PG_DSN = %q, want the provisioner-generated URL", val)
	}
	if _, ok := k.SecretValue("web", cp.AppDatabaseURLKey); ok {
		t.Error("DATABASE_URL was written as well; the app asked for one variable, not two")
	}
	// The name is PERSISTED, not inferred: it is read back from the record, which is what detach,
	// the dependency check and the restore cutover all consult.
	key, _, err := d.AddonEnvKey(ctx, string(cp.AddonPostgres), "web", cp.DefaultEnvironment, defaultInstance(cp.DefaultEnvironment))
	if err != nil {
		t.Fatalf("AddonEnvKey: %v", err)
	}
	if key != "PG_DSN" {
		t.Errorf("recorded key = %q, want PG_DSN", key)
	}
}

// TestReattachKeepsTheChosenName is the rotation case. Re-attaching rotates the role password, so
// the second attach must write the SAME variable the app is reading — an empty name means "whatever
// this attachment uses", not "DATABASE_URL", or a rotation would silently move the credential back
// to the default and leave the app on a password that no longer works.
func TestReattachKeepsTheChosenName(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"}); err != nil {
		t.Fatalf("first AttachAddon: %v", err)
	}
	res, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{})
	if err != nil {
		t.Fatalf("re-AttachAddon: %v", err)
	}
	if res.SecretKey != "DB_URL" {
		t.Errorf("secret key = %q, want DB_URL: a rotation writes where the app reads", res.SecretKey)
	}
	if _, ok := k.SecretValue("web", cp.AppDatabaseURLKey); ok {
		t.Error("the rotation moved the credential back to DATABASE_URL")
	}
}

// TestAttachRenameMovesTheVariable covers renaming an attachment that already exists. One app has one
// database per environment, so the name MOVES rather than multiplying: the new variable is written,
// the old one is removed — it holds a password this attach has just rotated — and the result says so.
func TestAttachRenameMovesTheVariable(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{}); err != nil {
		t.Fatalf("first AttachAddon: %v", err)
	}
	res, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"})
	if err != nil {
		t.Fatalf("renaming AttachAddon: %v", err)
	}
	if res.SecretKey != "DB_URL" {
		t.Errorf("secret key = %q, want DB_URL", res.SecretKey)
	}
	if res.PreviousSecretKey != cp.AppDatabaseURLKey {
		t.Errorf("previous secret key = %q, want %q so the move is reported rather than inferred",
			res.PreviousSecretKey, cp.AppDatabaseURLKey)
	}
	if _, ok := k.SecretValue("web", cp.AppDatabaseURLKey); ok {
		t.Error("the old DATABASE_URL survived the rename; the password was rotated, so it is a dead credential the app still reads")
	}
	if _, ok := k.SecretValue("web", "DB_URL"); !ok {
		t.Error("DB_URL was not written")
	}
}

// TestAttachRefusesOccupiedSecretKey is the destructive case the check exists for: a Secret value
// cannot be read back, so writing over one destroys it with no way to recover it. The refusal names
// what holds the key.
func TestAttachRefusesOccupiedSecretKey(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := newPostgresEngine(t)

	if err := k.SetSecretValue(ctx, "web", "DB_URL", "postgres://elsewhere/db"); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("AttachAddon over an existing secret = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "DB_URL") || !strings.Contains(err.Error(), "secret") {
		t.Errorf("error %q does not name the conflict", err)
	}
	// NOTHING was provisioned: the refusal happens before any work, so a rejected name leaves no
	// database and no rotated password behind.
	if got := prov.Ensured(); len(got) != 0 {
		t.Errorf("EnsureAppDatabase called %v; a refused name must provision nothing", got)
	}
	if val, _ := k.SecretValue("web", "DB_URL"); val != "postgres://elsewhere/db" {
		t.Errorf("the existing secret value was changed to %q", val)
	}
}

// TestAttachRefusesOccupiedConfigKey covers the other half of the app's environment. A config var and
// a Secret key render into the workload under one name, and which one wins is not something a user
// should have to know.
func TestAttachRefusesOccupiedConfigKey(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	if err := d.SetAppEnv(ctx, "web", "DB_URL", "postgres://elsewhere/db"); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("AttachAddon over an existing config var = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "DB_URL") || !strings.Contains(err.Error(), "config") {
		t.Errorf("error %q does not name the conflict", err)
	}
}

// TestAttachOwnKeyIsNotAConflict: the attachment's own variable is its to overwrite. Without this a
// rotation would refuse itself the second time it ran.
func TestAttachOwnKeyIsNotAConflict(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"}); err != nil {
		t.Fatalf("first AttachAddon: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"}); err != nil {
		t.Fatalf("re-AttachAddon under the same name: %v", err)
	}
}

// TestAttachRejectsMalformedKey: a name a container runtime would not accept is refused before
// anything is provisioned.
func TestAttachRejectsMalformedKey(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := newPostgresEngine(t)

	for _, bad := range []string{"9LIVES", "has-a-dash", "has space", "has=equals"} {
		if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: bad}); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("AttachAddon(--as %q) = %v, want ErrInvalid", bad, err)
		}
	}
	if got := prov.Ensured(); len(got) != 0 {
		t.Errorf("EnsureAppDatabase called %v; an invalid name must provision nothing", got)
	}
}

// TestDetachRemovesTheRecordedKey is the property that makes naming safe at all: detach removes the
// variable that was written, not the constant. Removing the wrong one leaves a live credential in
// the app's environment pointing at a database this call just dropped.
func TestDetachRemovesTheRecordedKey(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newPostgresEngine(t)

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"}); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", "", cp.DetachAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("DetachAddon: %v", err)
	}
	if _, ok := k.SecretValue("web", "DB_URL"); ok {
		t.Error("detach left DB_URL behind: the app still holds a credential for a dropped database")
	}
	// The record goes with it, so a later attach starts from Burrow's default rather than inheriting
	// a name for an attachment that no longer exists.
	_, recorded, err := d.AddonEnvKey(ctx, string(cp.AddonPostgres), "web", cp.DefaultEnvironment, defaultInstance(cp.DefaultEnvironment))
	if err != nil {
		t.Fatalf("AddonEnvKey: %v", err)
	}
	if recorded {
		t.Errorf("the attachment record survived the detach; a later attach would inherit a name for an attachment that no longer exists")
	}
}

// TestDetachRefusesWhenTheKeyCannotBeRead: the name is read BEFORE the guardrail and before anything
// is dropped, so a store that will not answer stops the detach rather than removing the default and
// dropping the database anyway.
func TestDetachRefusesWhenTheKeyCannotBeRead(t *testing.T) {
	ctx := context.Background()
	e, _, d, prov := newPostgresEngine(t)

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"}); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	d.SetError(fake.OpAddonEnvKey, errors.New("database unavailable"))
	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", "", cp.DetachAddonOptions{Confirm: true}); err == nil {
		t.Fatal("DetachAddon succeeded with the recorded name unreadable")
	}
	if got := prov.Revoked(); len(got) != 0 {
		t.Errorf("RevokeAppDatabase called %v; nothing on the instance may be touched when the key is unknown", got)
	}
}

// TestAppChecksProbeTheNamedKey: the deploy-time dependency check reports the variable this app
// actually reads (ADR-0076 §4), so the probe run inside the container looks up the right one and the
// failure message names the right one.
func TestAppChecksProbeTheNamedKey(t *testing.T) {
	ctx := context.Background()
	e, _, d, prov := newPostgresEngine(t)
	// The derivation asks the environment's instance who is attached, so both halves have to exist:
	// the registered instance and an app database on it.
	installPostgresAddon(t, d, cp.DefaultEnvironment)
	prov.SetAttachedApps(cp.DefaultEnvironment, "web")

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{EnvKey: "DB_URL"}); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	rep, err := e.AppChecks(ctx, "web", "")
	if err != nil {
		t.Fatalf("AppChecks: %v", err)
	}
	var found bool
	for _, d := range rep.Dependencies {
		if d.Kind == cp.DependencyPostgres {
			found = true
			if d.EnvKey != "DB_URL" {
				t.Errorf("dependency env key = %q, want DB_URL", d.EnvKey)
			}
		}
	}
	if !found {
		t.Error("no postgres dependency was derived for an attached app")
	}
}
