// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

func appSecret(app string, data map[string]string) *corev1.Secret {
	d := map[string][]byte{}
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cp.AppSecretName(app), Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       d,
	}
}

func TestSecretKeysReturnsSortedKeysNotValues(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(appSecret("web", map[string]string{"STRIPE_KEY": "sk_live_x", "DATABASE_URL": "postgres://y"}))
	a := kube.New(cs, ns)

	keys, err := a.SecretKeys(ctx, "web")
	if err != nil {
		t.Fatalf("SecretKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "DATABASE_URL" || keys[1] != "STRIPE_KEY" {
		t.Errorf("keys = %v, want [DATABASE_URL STRIPE_KEY] sorted", keys)
	}
}

func TestSecretKeysMissingSecretIsEmpty(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), ns)
	keys, err := a.SecretKeys(context.Background(), "web")
	if err != nil {
		t.Fatalf("SecretKeys on missing secret: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("keys = %v, want empty for an app with no secrets", keys)
	}
}

func TestSetSecretValueCreatesAndUpserts(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	a := kube.New(cs, ns)

	// First set creates the Secret (Opaque, Burrow labels) with the value.
	if err := a.SetSecretValue(ctx, "web", "STRIPE_KEY", "sk_live_x"); err != nil {
		t.Fatalf("SetSecretValue: %v", err)
	}
	s, err := cs.CoreV1().Secrets(ns).Get(ctx, cp.AppSecretName("web"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if s.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %q, want Opaque", s.Type)
	}
	if s.Labels["app.kubernetes.io/managed-by"] != "burrow" || s.Labels["app.kubernetes.io/name"] != "web" {
		t.Errorf("labels = %v, want name=web managed-by=burrow", s.Labels)
	}
	if string(s.Data["STRIPE_KEY"]) != "sk_live_x" {
		t.Errorf("value = %q, want sk_live_x", s.Data["STRIPE_KEY"])
	}

	// A second key upserts into the same Secret without dropping the first.
	if err := a.SetSecretValue(ctx, "web", "DATABASE_URL", "postgres://y"); err != nil {
		t.Fatalf("second SetSecretValue: %v", err)
	}
	s, _ = cs.CoreV1().Secrets(ns).Get(ctx, cp.AppSecretName("web"), metav1.GetOptions{})
	if string(s.Data["STRIPE_KEY"]) != "sk_live_x" || string(s.Data["DATABASE_URL"]) != "postgres://y" {
		t.Errorf("data = %v, want both keys retained", s.Data)
	}
}

func TestUnsetSecretKeyRemovesKey(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(appSecret("web", map[string]string{"A": "1", "B": "2"}))
	a := kube.New(cs, ns)

	if err := a.UnsetSecretKey(ctx, "web", "A"); err != nil {
		t.Fatalf("UnsetSecretKey: %v", err)
	}
	s, err := cs.CoreV1().Secrets(ns).Get(ctx, cp.AppSecretName("web"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get secret: %v", err)
	}
	if _, ok := s.Data["A"]; ok {
		t.Error("key A should be removed")
	}
	if string(s.Data["B"]) != "2" {
		t.Errorf("key B = %q, want 2 retained", s.Data["B"])
	}
}

func TestUnsetSecretKeyAbsentIsNoOp(t *testing.T) {
	ctx := context.Background()
	// Missing Secret entirely: no error.
	a := kube.New(fake.NewSimpleClientset(), ns)
	if err := a.UnsetSecretKey(ctx, "web", "A"); err != nil {
		t.Errorf("UnsetSecretKey on missing secret = %v, want nil", err)
	}
	// Secret present but key absent: no error.
	a = kube.New(fake.NewSimpleClientset(appSecret("web", map[string]string{"B": "2"})), ns)
	if err := a.UnsetSecretKey(ctx, "web", "A"); err != nil {
		t.Errorf("UnsetSecretKey on absent key = %v, want nil", err)
	}
}

func TestRestartWorkloadBumpsAnnotation(t *testing.T) {
	ctx := context.Background()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns}}
	cs := fake.NewSimpleClientset(dep)
	a := kube.New(cs, ns)

	at := time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)
	if err := a.RestartWorkload(ctx, "web", at); err != nil {
		t.Fatalf("RestartWorkload: %v", err)
	}
	got, err := cs.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get deploy: %v", err)
	}
	if v := got.Spec.Template.Annotations[cp.RestartedAtAnnotation]; v != at.Format(time.RFC3339Nano) {
		t.Errorf("restart annotation = %q, want %q", v, at.Format(time.RFC3339Nano))
	}
}

func TestRestartWorkloadMissingIsNotFound(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), ns)
	err := a.RestartWorkload(context.Background(), "web", time.Now())
	if !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("RestartWorkload missing = %v, want ErrNotFound", err)
	}
}
