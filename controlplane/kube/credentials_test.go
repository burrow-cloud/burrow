// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

func TestCredentialsToken(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: kube.DefaultCredentialsSecret, Namespace: "burrow"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"digitalocean": []byte("dop_tok")},
	})
	creds := kube.NewCredentials(cs, "burrow", "")

	got, err := creds.Token(ctx, "digitalocean")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "dop_tok" {
		t.Errorf("Token = %q, want dop_tok", got)
	}

	// A key that is not present is ErrNotFound, distinct from a transport error.
	if _, err := creds.Token(ctx, "cloudflare"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Errorf("missing key err = %v, want ErrNotFound", err)
	}
}

func TestCredentialsTokenSecretMissing(t *testing.T) {
	// Before any provider is configured the Secret exists (install creates it empty); a
	// missing Secret still reads as ErrNotFound rather than a crash.
	creds := kube.NewCredentials(fake.NewSimpleClientset(), "burrow", "")
	if _, err := creds.Token(context.Background(), "digitalocean"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Errorf("missing secret err = %v, want ErrNotFound", err)
	}
}

func TestCredentialsSetToken(t *testing.T) {
	ctx := context.Background()
	// install creates the Secret empty; SetToken upserts into it (ADR-0030).
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: kube.DefaultCredentialsSecret, Namespace: "burrow"},
		Type:       corev1.SecretTypeOpaque,
	})
	creds := kube.NewCredentials(cs, "burrow", "")

	if err := creds.SetToken(ctx, "digitalocean", "dop_tok"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	// Round-trips through Token.
	if got, err := creds.Token(ctx, "digitalocean"); err != nil || got != "dop_tok" {
		t.Fatalf("Token after SetToken = %q %v, want dop_tok nil", got, err)
	}
	// A second key upserts without disturbing the first.
	if err := creds.SetToken(ctx, "cloudflare", "cf_tok"); err != nil {
		t.Fatalf("SetToken second key: %v", err)
	}
	if got, _ := creds.Token(ctx, "digitalocean"); got != "dop_tok" {
		t.Errorf("first key disturbed: %q", got)
	}
	// Rotating a key replaces it in place.
	if err := creds.SetToken(ctx, "digitalocean", "rotated"); err != nil {
		t.Fatalf("SetToken rotate: %v", err)
	}
	if got, _ := creds.Token(ctx, "digitalocean"); got != "rotated" {
		t.Errorf("rotate = %q, want rotated", got)
	}
}

func TestCredentialsSetTokenCreatesSecret(t *testing.T) {
	// No Secret yet (an older install): SetToken creates it as Opaque.
	ctx := context.Background()
	creds := kube.NewCredentials(fake.NewSimpleClientset(), "burrow", "")
	if err := creds.SetToken(ctx, "digitalocean", "dop_tok"); err != nil {
		t.Fatalf("SetToken create: %v", err)
	}
	if got, err := creds.Token(ctx, "digitalocean"); err != nil || got != "dop_tok" {
		t.Errorf("Token = %q %v, want dop_tok nil", got, err)
	}
}
