// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestProviderAddWithoutTypeListsSupportedTypes(t *testing.T) {
	var out, errb bytes.Buffer
	// Missing <type>: the error and usage must name the available types so the user isn't left
	// guessing what to pass.
	_ = run(context.Background(), []string{"provider", "add"}, &out, &errb)
	s := errb.String()
	for _, want := range []string{"needs <type>", "cloudflare", "digitalocean"} {
		if !strings.Contains(s, want) {
			t.Errorf("provider add (no type) output missing %q:\n%s", want, s)
		}
	}
}

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

func TestReadAndDeleteCredential(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	if err := writeCredential(ctx, cs, "burrow", "digitalocean", "tok"); err != nil {
		t.Fatal(err)
	}

	if v, existed, err := readCredential(ctx, cs, "burrow", "digitalocean"); err != nil || !existed || v != "tok" {
		t.Fatalf("readCredential = %q %v %v, want tok true nil", v, existed, err)
	}
	if _, existed, _ := readCredential(ctx, cs, "burrow", "absent"); existed {
		t.Errorf("absent key reported as existing")
	}

	if err := deleteCredential(ctx, cs, "burrow", "digitalocean"); err != nil {
		t.Fatalf("deleteCredential: %v", err)
	}
	if _, existed, _ := readCredential(ctx, cs, "burrow", "digitalocean"); existed {
		t.Errorf("key still present after delete")
	}
}

func TestRestoreCredentialRollsBack(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	// A new key that fails validation is removed entirely.
	_ = writeCredential(ctx, cs, "burrow", "cloudflare", "bad")
	restoreCredential(ctx, cs, "burrow", "cloudflare", "", false)
	if _, existed, _ := readCredential(ctx, cs, "burrow", "cloudflare"); existed {
		t.Errorf("a never-existed key should be deleted on rollback")
	}

	// Rotating an existing key to a bad token rolls back to the prior value.
	_ = writeCredential(ctx, cs, "burrow", "digitalocean", "good")
	prior, existed, _ := readCredential(ctx, cs, "burrow", "digitalocean")
	_ = writeCredential(ctx, cs, "burrow", "digitalocean", "bad-rotation")
	restoreCredential(ctx, cs, "burrow", "digitalocean", prior, existed)
	if v, _, _ := readCredential(ctx, cs, "burrow", "digitalocean"); v != "good" {
		t.Errorf("rollback left %q, want the prior good token", v)
	}
}

func TestReadTokenFromPipe(t *testing.T) {
	// A non-terminal reader (a pipe/redirect, as in a script) is read directly and trimmed.
	got, err := readToken(strings.NewReader("  dop_v1_abc\n"), io.Discard, "token: ")
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "dop_v1_abc" {
		t.Errorf("readToken = %q, want the trimmed token", got)
	}
}

func TestProviderTypesCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"provider", "types"}, &out, &errb); err != nil {
		t.Fatalf("provider types: %v", err)
	}
	s := out.String()
	for _, want := range []string{"TYPE", "SUPPORTS", "cloudflare", "digitalocean", "dns"} {
		if !strings.Contains(s, want) {
			t.Errorf("provider types output missing %q:\n%s", want, s)
		}
	}
}
