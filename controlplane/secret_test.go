// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

func TestListSecretsReturnsKeysOnly(t *testing.T) {
	ctx := context.Background()
	e, k, _, _, _ := newEngine(t, permissive())
	// A `secret set` happens over the kubeconfig path, not the engine, so seed the fake directly.
	k.SetSecret("web", "STRIPE_KEY", "sk_live_x")
	k.SetSecret("web", "DATABASE_URL", "postgres://y")

	keys, err := e.ListSecrets(ctx, "web")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(keys) != 2 || keys[0] != "DATABASE_URL" || keys[1] != "STRIPE_KEY" {
		t.Errorf("keys = %v, want [DATABASE_URL STRIPE_KEY] sorted", keys)
	}
}

func TestListSecretsEmpty(t *testing.T) {
	e, _, _, _, _ := newEngine(t, permissive())
	keys, err := e.ListSecrets(context.Background(), "web")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("keys = %v, want empty for an app with no secrets", keys)
	}
}

func TestUnsetSecretRemovesAndRolls(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	k.SetSecret("web", "A", "1")
	k.SetSecret("web", "B", "2")

	if err := e.UnsetSecret(ctx, "web", "A", false); err != nil {
		t.Fatalf("UnsetSecret: %v", err)
	}
	if _, ok := k.SecretValue("web", "A"); ok {
		t.Error("secret A should be removed")
	}
	if v, _ := k.SecretValue("web", "B"); v != "2" {
		t.Errorf("secret B = %q, want 2 retained", v)
	}
	// A default unset rolls the running workload (envFrom is read only at pod start).
	if _, rolled := k.RestartedAt("web"); !rolled {
		t.Error("default UnsetSecret should roll the running workload")
	}
}

func TestUnsetSecretNoRestartDoesNotRoll(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	k.SetSecret("web", "A", "1")

	if err := e.UnsetSecret(ctx, "web", "A", true); err != nil {
		t.Fatalf("UnsetSecret no-restart: %v", err)
	}
	if _, ok := k.SecretValue("web", "A"); ok {
		t.Error("secret A should be removed from the Secret")
	}
	if _, rolled := k.RestartedAt("web"); rolled {
		t.Error("--no-restart UnsetSecret must not roll the workload")
	}
}

func TestUnsetSecretNoRunningWorkloadIsNoOpRoll(t *testing.T) {
	ctx := context.Background()
	e, k, _, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "A", "1")
	// No deployed workload: the unset persists, and the missing-workload roll is not an error.
	if err := e.UnsetSecret(ctx, "web", "A", false); err != nil {
		t.Fatalf("UnsetSecret with no workload: %v", err)
	}
	if _, ok := k.SecretValue("web", "A"); ok {
		t.Error("secret A should be removed even with no running workload")
	}
}

func TestSecretInvalidInputs(t *testing.T) {
	ctx := context.Background()
	e, _, _, _, _ := newEngine(t, permissive())

	if _, err := e.ListSecrets(ctx, "Bad!"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("ListSecrets bad app = %v, want ErrInvalid", err)
	}
	if err := e.UnsetSecret(ctx, "web", "1BAD", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("UnsetSecret bad key = %v, want ErrInvalid", err)
	}
	if err := e.UnsetSecret(ctx, "Bad!", "A", true); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("UnsetSecret bad app = %v, want ErrInvalid", err)
	}
}

// TestDeployRendersEnvFromSecretName is a guard that the workload spec the engine applies carries
// the app, so the adapter derives burrow-app-<app>-secrets — the envFrom injection point. The
// adapter test asserts the rendered envFrom; here we pin that a deploy applies a spec for the app.
func TestDeployAppliesSpecForApp(t *testing.T) {
	ctx := context.Background()
	e, k, r, _, _ := newEngine(t, permissive())
	r.Add("img:1", "sha256:1")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	spec, ok := k.Spec("web")
	if !ok || spec.App != "web" {
		t.Errorf("applied spec = %+v, want App=web", spec)
	}
}
