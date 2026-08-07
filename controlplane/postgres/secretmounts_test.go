// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// The store tests for a secret projected as a file (ADR-0089 §5). Every app name is scoped to the
// test's own name so they are safe against the shared database the suite runs on.

func mountApp(t *testing.T, suffix string) string {
	t.Helper()
	return strings.ToLower(t.Name()) + "-" + suffix
}

// TestStoreSecretMountRoundTrip: mount, read back, re-mount under a new filename, unmount. The read
// of an app that mounts nothing is the important row — it must be the zero value and NOT an error,
// because the deploy path reads it on every apply and "no mount" is where every app starts.
func TestStoreSecretMountRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := mountApp(t, "web")
	at := time.Date(2026, 8, 7, 2, 0, 0, 0, time.UTC)

	got, err := s.SecretMounts(ctx, app, "prod")
	if err != nil {
		t.Fatalf("SecretMounts (nothing mounted) returned an error, want the zero value: %v", err)
	}
	if got.Any() || got.Directory() != cp.DefaultSecretsDir {
		t.Errorf("SecretMounts (nothing mounted) = %+v, want the zero value at the default directory", got)
	}

	for _, m := range []cp.SecretMount{
		{App: app, Environment: "prod", Key: "TLS_KEY", Filename: "TLS_KEY", UpdatedAt: at},
		{App: app, Environment: "prod", Key: "GOOGLE_CREDENTIALS", Filename: "creds.json", UpdatedAt: at},
	} {
		if err := s.SetSecretMount(ctx, m); err != nil {
			t.Fatalf("SetSecretMount(%s): %v", m.Key, err)
		}
	}
	got, err = s.SecretMounts(ctx, app, "prod")
	if err != nil {
		t.Fatalf("SecretMounts: %v", err)
	}
	// Sorted by key, so the pod template a set of mounts renders is always the same bytes.
	if len(got.Mounts) != 2 || got.Mounts[0].Key != "GOOGLE_CREDENTIALS" || got.Mounts[1].Key != "TLS_KEY" {
		t.Fatalf("SecretMounts = %+v, want both mounts sorted by key", got.Mounts)
	}
	if got.Mounts[0].Filename != "creds.json" || !got.Mounts[0].UpdatedAt.Equal(at) {
		t.Errorf("mount = %+v, want creds.json at %s", got.Mounts[0], at)
	}

	// Re-mounting a key replaces its projection rather than adding a second file.
	if err := s.SetSecretMount(ctx, cp.SecretMount{App: app, Environment: "prod", Key: "TLS_KEY", Filename: "tls.key", UpdatedAt: at.Add(time.Hour)}); err != nil {
		t.Fatalf("SetSecretMount (re-mount): %v", err)
	}
	got, _ = s.SecretMounts(ctx, app, "prod")
	if len(got.Mounts) != 2 || got.Mounts[1].Filename != "tls.key" {
		t.Errorf("SecretMounts after re-mount = %+v, want TLS_KEY renamed in place", got.Mounts)
	}

	if err := s.UnsetSecretMount(ctx, app, "prod", "TLS_KEY"); err != nil {
		t.Fatalf("UnsetSecretMount: %v", err)
	}
	got, _ = s.SecretMounts(ctx, app, "prod")
	if len(got.Mounts) != 1 || got.Mounts[0].Key != "GOOGLE_CREDENTIALS" {
		t.Errorf("SecretMounts after unmount = %+v, want only the key that was left mounted", got.Mounts)
	}
	// Unmounting one that was never mounted is a no-op, since it is what a caller reaches for when
	// they are not sure what is mounted.
	if err := s.UnsetSecretMount(ctx, app, "prod", "TLS_KEY"); err != nil {
		t.Errorf("UnsetSecretMount (already unmounted): %v", err)
	}
}

// TestStoreSecretsDirIsPerAppAndPerEnvironment: the directory is one value for the whole app in one
// environment (ADR-0089 §2), and the same app in another environment is untouched by it.
func TestStoreSecretsDirIsPerAppAndPerEnvironment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := mountApp(t, "web")
	at := time.Date(2026, 8, 7, 2, 0, 0, 0, time.UTC)

	if err := s.SetSecretsDir(ctx, app, "prod", "/etc/app/secrets", at); err != nil {
		t.Fatalf("SetSecretsDir: %v", err)
	}
	if err := s.SetSecretMount(ctx, cp.SecretMount{App: app, Environment: "prod", Key: "TLS_KEY", Filename: "tls.key", UpdatedAt: at}); err != nil {
		t.Fatalf("SetSecretMount: %v", err)
	}
	if err := s.SetSecretMount(ctx, cp.SecretMount{App: app, Environment: "staging", Key: "TLS_KEY", Filename: "tls.key", UpdatedAt: at}); err != nil {
		t.Fatalf("SetSecretMount(staging): %v", err)
	}

	prod, _ := s.SecretMounts(ctx, app, "prod")
	if prod.Directory() != "/etc/app/secrets" {
		t.Errorf("prod directory = %q, want the override", prod.Directory())
	}
	staging, _ := s.SecretMounts(ctx, app, "staging")
	if staging.Directory() != cp.DefaultSecretsDir {
		t.Errorf("staging directory = %q, want the default: the override is per environment", staging.Directory())
	}
}

// TestStoreDeleteSecretMountsIsTheTeardown: an app created later under the same name starts with
// nothing on disk rather than inheriting a previous occupant's file layout.
func TestStoreDeleteSecretMountsIsTheTeardown(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	app := mountApp(t, "web")
	at := time.Date(2026, 8, 7, 2, 0, 0, 0, time.UTC)

	if err := s.SetSecretsDir(ctx, app, "prod", "/etc/app/secrets", at); err != nil {
		t.Fatalf("SetSecretsDir: %v", err)
	}
	for _, env := range []string{"prod", "staging"} {
		if err := s.SetSecretMount(ctx, cp.SecretMount{App: app, Environment: env, Key: "TLS_KEY", Filename: "tls.key", UpdatedAt: at}); err != nil {
			t.Fatalf("SetSecretMount(%s): %v", env, err)
		}
	}
	if err := s.DeleteSecretMounts(ctx, app); err != nil {
		t.Fatalf("DeleteSecretMounts: %v", err)
	}
	for _, env := range []string{"prod", "staging"} {
		got, err := s.SecretMounts(ctx, app, env)
		if err != nil {
			t.Fatalf("SecretMounts(%s): %v", env, err)
		}
		if got.Any() || got.Directory() != cp.DefaultSecretsDir {
			t.Errorf("SecretMounts(%s) after teardown = %+v, want nothing at the default directory", env, got)
		}
	}
}
