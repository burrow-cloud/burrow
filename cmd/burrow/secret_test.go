// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestSetAppSecretWritesValueWithKubeconfig proves the SET path writes the value straight into the
// per-app Kubernetes Secret with the developer's clientset — never through burrowd/the API.
func TestSetAppSecretWritesValueWithKubeconfig(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	if err := setAppSecret(ctx, cs, "apps", "web", "STRIPE_KEY", "sk_live_x"); err != nil {
		t.Fatalf("setAppSecret: %v", err)
	}
	s, err := cs.CoreV1().Secrets("apps").Get(ctx, appSecretName("web"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if s.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret type = %q, want Opaque", s.Type)
	}
	if string(s.Data["STRIPE_KEY"]) != "sk_live_x" {
		t.Errorf("stored value = %q, want sk_live_x", s.Data["STRIPE_KEY"])
	}

	// A second key upserts into the same Secret without dropping the first.
	if err := setAppSecret(ctx, cs, "apps", "web", "DATABASE_URL", "postgres://y"); err != nil {
		t.Fatalf("second setAppSecret: %v", err)
	}
	s, _ = cs.CoreV1().Secrets("apps").Get(ctx, appSecretName("web"), metav1.GetOptions{})
	if string(s.Data["STRIPE_KEY"]) != "sk_live_x" || string(s.Data["DATABASE_URL"]) != "postgres://y" {
		t.Errorf("secret data = %v, want both keys", s.Data)
	}
}

func TestRestartWorkloadBumpsAnnotation(t *testing.T) {
	ctx := context.Background()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps"}}
	cs := fake.NewSimpleClientset(dep)

	rolled, err := restartWorkload(ctx, cs, "apps", "web")
	if err != nil {
		t.Fatalf("restartWorkload: %v", err)
	}
	if !rolled {
		t.Fatal("expected rolled=true for an existing Deployment")
	}
	got, _ := cs.AppsV1().Deployments("apps").Get(ctx, "web", metav1.GetOptions{})
	if got.Spec.Template.Annotations[restartedAtAnnotation] == "" {
		t.Error("restart annotation not set on the pod template")
	}
}

func TestRestartWorkloadMissingDeploymentIsNoRoll(t *testing.T) {
	// No Deployment yet: the Secret persists and the next deploy injects it via envFrom — not an
	// error, just nothing to roll.
	rolled, err := restartWorkload(context.Background(), fake.NewSimpleClientset(), "apps", "web")
	if err != nil {
		t.Fatalf("restartWorkload missing = %v, want nil", err)
	}
	if rolled {
		t.Error("rolled should be false when the Deployment does not exist yet")
	}
}

func TestSecretListShowsKeysNotValues(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/apps/web/secrets" {
			t.Errorf("request = %s %s, want GET /v1/apps/web/secrets", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []string{"DATABASE_URL", "STRIPE_KEY"}})
	}, "app", "secret", "list", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "DATABASE_URL") || !strings.Contains(out, "STRIPE_KEY") {
		t.Errorf("output = %q, want the keys", out)
	}
}

func TestSecretUnset(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	_, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "web", "key": "STRIPE_KEY"})
	}, "app", "secret", "unset", "web", "STRIPE_KEY", "--no-restart")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/v1/apps/web/secrets/STRIPE_KEY" {
		t.Errorf("request = %s %s, want DELETE /v1/apps/web/secrets/STRIPE_KEY", gotMethod, gotPath)
	}
	if gotQuery != "no_restart=true" {
		t.Errorf("query = %q, want no_restart=true", gotQuery)
	}
}
