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

// costNoticeMarker is the distinctive phrase the LoadBalancer cost notice carries; the nodeport
// path must never print it. The notice is provider-agnostic (it never names the detected cloud).
const costNoticeMarker = "LoadBalancers normally cost money"

func TestIngressInstallDryRunExpose(t *testing.T) {
	// loadbalancer: the plan names the cloud (LoadBalancer) manifest and carries the cost notice.
	var lb bytes.Buffer
	if err := run(context.Background(), []string{"system", "ingress", "install", "--dry-run", "--expose", "loadbalancer"}, &lb, &lb); err != nil {
		t.Fatalf("dry-run loadbalancer: %v", err)
	}
	if !strings.Contains(lb.String(), costNoticeMarker) {
		t.Errorf("loadbalancer dry-run should include the cost notice %q:\n%s", costNoticeMarker, lb.String())
	}
	if !strings.Contains(lb.String(), ingressNginxManifest) {
		t.Errorf("loadbalancer dry-run should reference the cloud manifest:\n%s", lb.String())
	}
	if strings.Contains(lb.String(), ingressNginxBaremetalManifest) {
		t.Errorf("loadbalancer dry-run should not reference the baremetal manifest:\n%s", lb.String())
	}

	// nodeport: the plan references the baremetal (NodePort) manifest and omits the cost notice.
	var np bytes.Buffer
	if err := run(context.Background(), []string{"system", "ingress", "install", "--dry-run", "--expose", "nodeport"}, &np, &np); err != nil {
		t.Fatalf("dry-run nodeport: %v", err)
	}
	if strings.Contains(np.String(), costNoticeMarker) {
		t.Errorf("nodeport dry-run should omit the cost notice:\n%s", np.String())
	}
	if !strings.Contains(np.String(), ingressNginxBaremetalManifest) {
		t.Errorf("nodeport dry-run should reference the baremetal manifest:\n%s", np.String())
	}
}

func TestResolveExposeAuto(t *testing.T) {
	ctx := context.Background()

	// A known cloud provider (DigitalOcean) supports LoadBalancer, so auto picks loadbalancer and
	// ingressManifestFor returns the cloud manifest.
	cloud := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "digitalocean://123"}},
	)
	expose, err := resolveExpose(ctx, exposeAuto, cloud)
	if err != nil {
		t.Fatalf("resolveExpose cloud: %v", err)
	}
	if expose != exposeLoadBalancer {
		t.Errorf("auto on a cloud provider should pick loadbalancer, got %q", expose)
	}
	if got := ingressManifestFor(expose); got != ingressNginxManifest {
		t.Errorf("loadbalancer should use the cloud manifest, got %q", got)
	}

	// Bare-metal (no recognized providerID) has no inferred LoadBalancer support, so auto picks
	// nodeport and ingressManifestFor returns the baremetal manifest.
	bare := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	expose, err = resolveExpose(ctx, exposeAuto, bare)
	if err != nil {
		t.Fatalf("resolveExpose bare-metal: %v", err)
	}
	if expose != exposeNodePort {
		t.Errorf("auto on bare-metal should pick nodeport, got %q", expose)
	}
	if got := ingressManifestFor(expose); got != ingressNginxBaremetalManifest {
		t.Errorf("nodeport should use the baremetal manifest, got %q", got)
	}
}

func TestConfirmInstall(t *testing.T) {
	// -y / --yes skips the prompt: returns true without reading stdin or printing "Proceed?".
	var out bytes.Buffer
	ok, err := confirmInstall(ingressOptions{yes: true}, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatalf("confirmInstall yes: %v", err)
	}
	if !ok {
		t.Errorf("--yes should proceed without prompting")
	}
	if strings.Contains(out.String(), "Proceed?") {
		t.Errorf("--yes should not print the prompt:\n%s", out.String())
	}

	// Without -y the prompt is shown and the typed answer decides.
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"n\n", false},
		{"\n", false}, // the [y/N] default
		{"", false},   // EOF
	} {
		var b bytes.Buffer
		ok, err := confirmInstall(ingressOptions{}, strings.NewReader(tc.in), &b)
		if err != nil {
			t.Fatalf("confirmInstall(%q): %v", tc.in, err)
		}
		if ok != tc.want {
			t.Errorf("confirmInstall(%q) = %v, want %v", tc.in, ok, tc.want)
		}
		if !strings.Contains(b.String(), "Proceed?") {
			t.Errorf("interactive confirm should print the prompt for input %q:\n%s", tc.in, b.String())
		}
	}
}
