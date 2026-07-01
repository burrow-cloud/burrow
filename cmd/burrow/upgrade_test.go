// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/connect"
)

// existingInstall builds a fake cluster that looks like a completed `burrow install` in
// namespace ns, deploying apps into appNS. Extra container env vars (e.g. the add-on
// namespace) can be appended to model installs from different eras.
func existingInstall(ns, appNS string, extraEnv ...corev1.EnvVar) *fake.Clientset {
	env := append([]corev1.EnvVar{{Name: "BURROW_NAMESPACE", Value: appNS}}, extraEnv...)
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
							Env:  env,
						}},
					},
				},
			},
		},
	)
}

func TestUpgradeOptionsPreservesState(t *testing.T) {
	cs := existingInstall("burrow", "apps", corev1.EnvVar{Name: "BURROW_ADDON_NAMESPACE", Value: "addons"})
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
	if opts.AddonNamespace != "addons" {
		t.Errorf("add-on namespace not preserved: got %q", opts.AddonNamespace)
	}
	if opts.Image != "registry.example.com/burrowd:v0.1.2" {
		t.Errorf("image not set to the upgrade target: got %q", opts.Image)
	}
}

// TestUpgradeOptionsDefaultsAddonNamespace covers an install that predates add-ons: the
// running Deployment carries no BURROW_ADDON_NAMESPACE env, so the upgrade falls back to the
// default add-on namespace rather than re-rendering an empty one.
func TestUpgradeOptionsDefaultsAddonNamespace(t *testing.T) {
	cs := existingInstall("burrow", "apps")
	opts, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
	if err != nil {
		t.Fatalf("upgradeOptions: %v", err)
	}
	if opts.AddonNamespace != connect.DefaultAddonNamespace {
		t.Errorf("add-on namespace not defaulted: got %q, want %q", opts.AddonNamespace, connect.DefaultAddonNamespace)
	}
}

// emptyMetaField matches a rendered name/namespace field with no value (a trailing space may
// remain after the colon, so \s* before the line end matters).
var emptyMetaField = regexp.MustCompile(`(?m)^\s*(name|namespace):\s*$`)

// TestUpgradeOptionsRendersNoEmptyFields guards every installOptions field an upgrade must
// carry forward: rendering the manifests from upgradeOptions must never leave a name or
// namespace field blank, whichever era the install came from. A blank field is what made
// server-side apply reject the upgrade ("applying namespace/: name is required").
func TestUpgradeOptionsRendersNoEmptyFields(t *testing.T) {
	fixtures := map[string]*fake.Clientset{
		"with add-on env":   existingInstall("burrow", "apps", corev1.EnvVar{Name: "BURROW_ADDON_NAMESPACE", Value: "addons"}),
		"predating add-ons": existingInstall("burrow", "apps"),
	}
	for name, cs := range fixtures {
		t.Run(name, func(t *testing.T) {
			opts, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
			if err != nil {
				t.Fatalf("upgradeOptions: %v", err)
			}
			manifests, err := renderManifests(opts)
			if err != nil {
				t.Fatalf("renderManifests: %v", err)
			}
			if m := emptyMetaField.FindString(manifests); m != "" {
				t.Errorf("rendered manifests contain an empty name/namespace field: %q", m)
			}
		})
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
