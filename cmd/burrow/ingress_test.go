// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRenderIssuer(t *testing.T) {
	// Production with an email.
	prod, err := renderIssuer(ingressOptions{issuerName: "letsencrypt", email: "me@example.com"})
	if err != nil {
		t.Fatalf("renderIssuer: %v", err)
	}
	for _, want := range []string{
		"kind: ClusterIssuer",
		"name: letsencrypt",
		"server: " + acmeProductionURL,
		"email: me@example.com",
		"name: letsencrypt-account-key",
		"class: nginx", // HTTP-01 solver via ingress-nginx
	} {
		if !strings.Contains(prod, want) {
			t.Errorf("issuer missing %q\n%s", want, prod)
		}
	}

	// Staging without an email: the staging directory, and no email line at all.
	stg, err := renderIssuer(ingressOptions{issuerName: "le-staging", staging: true})
	if err != nil {
		t.Fatalf("renderIssuer staging: %v", err)
	}
	if !strings.Contains(stg, "server: "+acmeStagingURL) || !strings.Contains(stg, "name: le-staging") {
		t.Errorf("staging issuer wrong:\n%s", stg)
	}
	if strings.Contains(stg, "email:") {
		t.Errorf("no email should be rendered when none is given:\n%s", stg)
	}
}

func TestIngressDetection(t *testing.T) {
	ctx := context.Background()

	// Empty cluster: neither present.
	cs := fake.NewSimpleClientset()
	if got, _ := ingressControllerPresent(ctx, cs); got {
		t.Errorf("ingress controller should be absent on an empty cluster")
	}
	if got, _ := certManagerPresent(ctx, cs); got {
		t.Errorf("cert-manager should be absent on an empty cluster")
	}

	// Detect the controller by its IngressClass, and cert-manager by its Deployment.
	cs = fake.NewSimpleClientset(
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager", Namespace: "cert-manager"}},
	)
	if got, _ := ingressControllerPresent(ctx, cs); !got {
		t.Errorf("ingress controller should be detected via the nginx IngressClass")
	}
	if got, _ := certManagerPresent(ctx, cs); !got {
		t.Errorf("cert-manager should be detected via its Deployment")
	}

	// Also detect the controller by its Deployment when no IngressClass exists yet.
	cs = fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ingress-nginx-controller", Namespace: "ingress-nginx"}},
	)
	if got, _ := ingressControllerPresent(ctx, cs); !got {
		t.Errorf("ingress controller should be detected via its Deployment")
	}
}

func TestIngressInstallDryRun(t *testing.T) {
	var out, errb bytes.Buffer
	// dry-run must not touch a cluster (no kubeconfig needed) — it only prints the plan.
	err := run(context.Background(), []string{"system", "ingress", "install", "--dry-run", "--staging", "--email", "a@b.com"}, &out, &errb)
	if err != nil {
		t.Fatalf("ingress install --dry-run: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		ingressNginxManifest,
		certManagerManifest,
		"kind: ClusterIssuer",
		acmeStagingURL,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, s)
		}
	}
}
