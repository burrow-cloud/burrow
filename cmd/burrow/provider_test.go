// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func credentialValue(t *testing.T, cs *fake.Clientset, ns, key string) (string, bool) {
	t.Helper()
	s, err := cs.CoreV1().Secrets(ns).Get(context.Background(), credentialsSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting credentials secret: %v", err)
	}
	if s.Type != corev1.SecretTypeOpaque {
		t.Errorf("credentials secret type = %q, want %q", s.Type, corev1.SecretTypeOpaque)
	}
	v, ok := s.Data[key]
	return string(v), ok
}

func TestWriteCredentialCreatesThenUpserts(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()

	// First write creates the Secret (install may not have, e.g. an older install).
	if err := writeCredential(ctx, cs, "burrow", "digitalocean", "tok-1"); err != nil {
		t.Fatalf("writeCredential create: %v", err)
	}
	if v, ok := credentialValue(t, cs, "burrow", "digitalocean"); !ok || v != "tok-1" {
		t.Fatalf("after create: %q ok=%v, want tok-1", v, ok)
	}

	// A second provider adds a key without disturbing the first.
	if err := writeCredential(ctx, cs, "burrow", "cloudflare", "tok-2"); err != nil {
		t.Fatalf("writeCredential second key: %v", err)
	}
	if v, _ := credentialValue(t, cs, "burrow", "digitalocean"); v != "tok-1" {
		t.Errorf("first key disturbed: %q", v)
	}
	if v, _ := credentialValue(t, cs, "burrow", "cloudflare"); v != "tok-2" {
		t.Errorf("second key = %q, want tok-2", v)
	}

	// Re-writing a key rotates it in place.
	if err := writeCredential(ctx, cs, "burrow", "digitalocean", "tok-rotated"); err != nil {
		t.Fatalf("writeCredential rotate: %v", err)
	}
	if v, _ := credentialValue(t, cs, "burrow", "digitalocean"); v != "tok-rotated" {
		t.Errorf("rotate = %q, want tok-rotated", v)
	}
}

func TestWriteCredentialUpsertsIntoExistingEmptySecret(t *testing.T) {
	// install creates the Secret empty; provider add must upsert into it, not fail.
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretName, Namespace: "burrow"},
		Type:       corev1.SecretTypeOpaque,
	})
	if err := writeCredential(context.Background(), cs, "burrow", "digitalocean", "tok"); err != nil {
		t.Fatalf("writeCredential into empty secret: %v", err)
	}
	if v, ok := credentialValue(t, cs, "burrow", "digitalocean"); !ok || v != "tok" {
		t.Errorf("value = %q ok=%v, want tok", v, ok)
	}
}

func TestReadSecretStdinTrims(t *testing.T) {
	got, err := readSecretStdin(strings.NewReader("  dop_v1_abc\n"))
	if err != nil {
		t.Fatalf("readSecretStdin: %v", err)
	}
	if got != "dop_v1_abc" {
		t.Errorf("readSecretStdin = %q, want trimmed token", got)
	}
}
