// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCliVersionDevDefault(t *testing.T) {
	// A test binary has no module release version, so the CLI reports the dev default.
	if got := cliVersion(); got != "dev" {
		t.Errorf("cliVersion() = %q, want dev for an unversioned build", got)
	}
}

func TestBurrowdImage(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "burrowd", Namespace: "burrow"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: "burrowd", Image: "ghcr.io/burrow-cloud/burrowd:v0.2.1"},
				}},
			},
		},
	})
	img, err := burrowdImage(ctx, cs, "burrow")
	if err != nil {
		t.Fatalf("burrowdImage: %v", err)
	}
	if img != "ghcr.io/burrow-cloud/burrowd:v0.2.1" {
		t.Errorf("image = %q", img)
	}

	// No control plane installed → IsNotFound, which the command renders as "not installed".
	if _, err := burrowdImage(ctx, fake.NewSimpleClientset(), "burrow"); !apierrors.IsNotFound(err) {
		t.Errorf("absent burrowd err = %v, want IsNotFound", err)
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/burrow-cloud/burrowd:v0.2.1": "v0.2.1",
		"burrowd:e2e":                         "e2e",
		"registry:5000/burrowd:v1":            "v1",                // a registry-host port colon is not the tag
		"ghcr.io/x/burrowd@sha256:abcd":       "ghcr.io/x/burrowd", // digest, no tag
		"burrowd":                             "burrowd",           // untagged
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVersionCommandPrintsCLILine(t *testing.T) {
	var out, errb bytes.Buffer
	// No reachable cluster in the test env, so the control-plane line is best-effort; the CLI
	// line must always print and the command must succeed.
	if err := run(context.Background(), []string{"version", "--kubeconfig", "/nonexistent"}, &out, &errb); err != nil {
		t.Fatalf("version: %v", err)
	}
	if s := out.String(); !strings.Contains(s, "burrow (CLI):  dev") {
		t.Errorf("version output = %q, want the CLI version line", s)
	}
}
