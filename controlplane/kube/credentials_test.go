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
