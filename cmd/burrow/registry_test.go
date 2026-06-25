// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// nsWithDefaultSA returns a fake cluster with just the default ServiceAccount present in ns,
// as every real namespace has.
func nsWithDefaultSA(ns string) *fake.Clientset {
	return fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
	})
}

func registrySecretConfig(t *testing.T, cs *fake.Clientset, ns string) dockerConfig {
	t.Helper()
	s, err := cs.CoreV1().Secrets(ns).Get(context.Background(), registrySecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting registry secret: %v", err)
	}
	if s.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("registry secret type = %q, want %q", s.Type, corev1.SecretTypeDockerConfigJson)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		t.Fatalf("unmarshaling dockerconfigjson: %v", err)
	}
	return cfg
}

func defaultSAPullSecrets(t *testing.T, cs *fake.Clientset, ns string) []string {
	t.Helper()
	sa, err := cs.CoreV1().ServiceAccounts(ns).Get(context.Background(), "default", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting default SA: %v", err)
	}
	var names []string
	for _, ref := range sa.ImagePullSecrets {
		names = append(names, ref.Name)
	}
	return names
}

func TestRegistryLoginCreatesSecretAndPatchesSA(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	if err := registryLogin(context.Background(), cs, "apps", "ghcr.io", "alice", "tok123"); err != nil {
		t.Fatalf("registryLogin: %v", err)
	}

	cfg := registrySecretConfig(t, cs, "apps")
	auth, ok := cfg.Auths["ghcr.io"]
	if !ok {
		t.Fatal("ghcr.io not in the registry secret")
	}
	if auth.Username != "alice" || auth.Password != "tok123" {
		t.Errorf("stored credential = %q/%q, want alice/tok123", auth.Username, auth.Password)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("alice:tok123")); auth.Auth != want {
		t.Errorf("auth field = %q, want %q", auth.Auth, want)
	}

	if got := defaultSAPullSecrets(t, cs, "apps"); len(got) != 1 || got[0] != registrySecretName {
		t.Errorf("default SA imagePullSecrets = %v, want [%s]", got, registrySecretName)
	}
}

func TestRegistryLoginMergesAndDoesNotDuplicateSARef(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	ctx := context.Background()
	if err := registryLogin(ctx, cs, "apps", "ghcr.io", "alice", "t1"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if err := registryLogin(ctx, cs, "apps", "registry.example.com", "bob", "t2"); err != nil {
		t.Fatalf("second login: %v", err)
	}

	cfg := registrySecretConfig(t, cs, "apps")
	if len(cfg.Auths) != 2 || cfg.Auths["ghcr.io"].Password != "t1" || cfg.Auths["registry.example.com"].Password != "t2" {
		t.Errorf("merged auths wrong: %+v", cfg.Auths)
	}
	// The SA must reference the pull secret exactly once, not once per registry.
	if got := defaultSAPullSecrets(t, cs, "apps"); len(got) != 1 {
		t.Errorf("default SA imagePullSecrets = %v, want a single ref", got)
	}
}

func TestRegistryLogoutRemovesHostAndCleansUp(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	ctx := context.Background()
	mustLogin(t, cs, "apps", "ghcr.io", "alice", "t1")
	mustLogin(t, cs, "apps", "registry.example.com", "bob", "t2")

	// Removing one of two leaves the secret with the other and keeps the SA ref.
	if err := registryLogout(ctx, cs, "apps", "ghcr.io"); err != nil {
		t.Fatalf("logout ghcr.io: %v", err)
	}
	cfg := registrySecretConfig(t, cs, "apps")
	if _, ok := cfg.Auths["ghcr.io"]; ok {
		t.Error("ghcr.io should be gone")
	}
	if _, ok := cfg.Auths["registry.example.com"]; !ok {
		t.Error("registry.example.com should remain")
	}
	if got := defaultSAPullSecrets(t, cs, "apps"); len(got) != 1 {
		t.Errorf("SA ref should remain while a registry is configured, got %v", got)
	}

	// Removing the last one deletes the secret and detaches it from the SA.
	if err := registryLogout(ctx, cs, "apps", "registry.example.com"); err != nil {
		t.Fatalf("logout last: %v", err)
	}
	_, err := cs.CoreV1().Secrets("apps").Get(ctx, registrySecretName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("registry secret should be deleted when empty, got err=%v", err)
	}
	if got := defaultSAPullSecrets(t, cs, "apps"); len(got) != 0 {
		t.Errorf("SA ref should be detached when no registries remain, got %v", got)
	}
}

func TestRegistryListSorted(t *testing.T) {
	cs := nsWithDefaultSA("apps")
	ctx := context.Background()
	mustLogin(t, cs, "apps", "registry.example.com", "bob", "t2")
	mustLogin(t, cs, "apps", "ghcr.io", "alice", "t1")

	hosts, err := registryList(ctx, cs, "apps")
	if err != nil {
		t.Fatalf("registryList: %v", err)
	}
	if len(hosts) != 2 || hosts[0] != "ghcr.io" || hosts[1] != "registry.example.com" {
		t.Errorf("registryList = %v, want [ghcr.io registry.example.com]", hosts)
	}

	// An empty cluster lists nothing without erroring.
	empty, err := registryList(ctx, nsWithDefaultSA("apps"), "apps")
	if err != nil || len(empty) != 0 {
		t.Errorf("expected empty list with no error, got %v, %v", empty, err)
	}
}

func mustLogin(t *testing.T, cs *fake.Clientset, ns, host, user, pass string) {
	t.Helper()
	if err := registryLogin(context.Background(), cs, ns, host, user, pass); err != nil {
		t.Fatalf("registryLogin %s: %v", host, err)
	}
}
