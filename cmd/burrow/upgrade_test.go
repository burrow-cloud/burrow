// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// existingInstall builds a fake cluster that looks like a completed `burrow install` in
// namespace ns, deploying apps into appNS.
func existingInstall(ns, appNS string) *fake.Clientset {
	return fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: ns},
			Data:       map[string][]byte{"token": []byte("existing-token")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd-db", Namespace: ns},
			Data:       map[string][]byte{"password": []byte("existing-pw")},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "burrowd",
							Env:  []corev1.EnvVar{{Name: "BURROW_NAMESPACE", Value: appNS}},
						}},
					},
				},
			},
		},
	)
}

func TestUpgradeOptionsPreservesState(t *testing.T) {
	cs := existingInstall("burrow", "apps")
	opts, err := upgradeOptions(context.Background(), cs, "burrow", "registry.example.com/burrowd:v0.1.2")
	if err != nil {
		t.Fatalf("upgradeOptions: %v", err)
	}
	if opts.Token != "existing-token" {
		t.Errorf("token not preserved: got %q", opts.Token)
	}
	if opts.DBPassword != "existing-pw" {
		t.Errorf("db password not preserved: got %q", opts.DBPassword)
	}
	if opts.AppNamespace != "apps" {
		t.Errorf("app namespace not preserved: got %q", opts.AppNamespace)
	}
	if opts.Image != "registry.example.com/burrowd:v0.1.2" {
		t.Errorf("image not set to the upgrade target: got %q", opts.Image)
	}
}

func TestUpgradeOptionsNotInstalled(t *testing.T) {
	cs := fake.NewSimpleClientset()
	_, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
	if err == nil {
		t.Fatal("expected an error upgrading with nothing installed")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error should explain Burrow is not installed, got: %v", err)
	}
}

func TestAlreadyInstalled(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		got, err := alreadyInstalled(context.Background(), existingInstall("burrow", "default"), "burrow")
		if err != nil {
			t.Fatalf("alreadyInstalled: %v", err)
		}
		if !got {
			t.Error("expected an existing install to be detected")
		}
	})
	t.Run("absent", func(t *testing.T) {
		got, err := alreadyInstalled(context.Background(), fake.NewSimpleClientset(), "burrow")
		if err != nil {
			t.Fatalf("alreadyInstalled: %v", err)
		}
		if got {
			t.Error("expected no install to be detected in an empty cluster")
		}
	})
}
