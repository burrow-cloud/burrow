// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	err := run(context.Background(), []string{"cluster", "ingress", "install", "--dry-run", "--staging", "--email", "a@b.com"}, &out, &errb)
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

// costNoticeMarker is the distinctive phrase the LoadBalancer cost/HA notice carries; the nodeport
// path must never print it. The notice is provider-agnostic beyond the DigitalOcean example price.
const costNoticeMarker = "a LoadBalancer is billable"

// spofNoticeMarker is the distinctive phrase the NodePort notice carries: it explains that pointing
// DNS at a single node makes that node a single point of failure.
const spofNoticeMarker = "single point of failure"

func TestIngressInstallDryRunExpose(t *testing.T) {
	// loadbalancer: the plan names the cloud (LoadBalancer) manifest and carries the cost/HA notice.
	var lb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "ingress", "install", "--dry-run", "--expose", "loadbalancer"}, &lb, &lb); err != nil {
		t.Fatalf("dry-run loadbalancer: %v", err)
	}
	if !strings.Contains(lb.String(), costNoticeMarker) {
		t.Errorf("loadbalancer dry-run should include the cost notice %q:\n%s", costNoticeMarker, lb.String())
	}
	if !strings.Contains(lb.String(), "high availability") {
		t.Errorf("loadbalancer dry-run should explain the HA benefit:\n%s", lb.String())
	}
	if strings.Contains(lb.String(), spofNoticeMarker) {
		t.Errorf("loadbalancer dry-run should not print the nodeport SPOF notice:\n%s", lb.String())
	}
	if !strings.Contains(lb.String(), ingressNginxManifest) {
		t.Errorf("loadbalancer dry-run should reference the cloud manifest:\n%s", lb.String())
	}
	if strings.Contains(lb.String(), ingressNginxBaremetalManifest) {
		t.Errorf("loadbalancer dry-run should not reference the baremetal manifest:\n%s", lb.String())
	}

	// nodeport: the plan references the baremetal (NodePort) manifest, prints the single-point-of-
	// failure notice, and omits the cost notice.
	var np bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "ingress", "install", "--dry-run", "--expose", "nodeport"}, &np, &np); err != nil {
		t.Fatalf("dry-run nodeport: %v", err)
	}
	if strings.Contains(np.String(), costNoticeMarker) {
		t.Errorf("nodeport dry-run should omit the cost notice:\n%s", np.String())
	}
	if !strings.Contains(np.String(), spofNoticeMarker) {
		t.Errorf("nodeport dry-run should include the single-point-of-failure notice:\n%s", np.String())
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

func TestIngressInstallApproveFlag(t *testing.T) {
	install := ingressInstallCmd(t)
	flags := install.Flags()

	approve := flags.Lookup("approve")
	if approve == nil {
		t.Fatal("ingress install should register the --approve flag")
	}
	if approve.Shorthand != "" {
		t.Errorf("--approve must have no shorthand (a cost approval should not be a single keystroke), got -%s", approve.Shorthand)
	}

	// --yes and its -y shorthand are gone.
	if f := flags.Lookup("yes"); f != nil {
		t.Errorf("--yes should be removed, found %v", f)
	}
	if f := flags.ShorthandLookup("y"); f != nil {
		t.Errorf("-y should be removed, found %v", f)
	}
}

// ingressInstallCmd builds the ingress command tree and returns its "install" subcommand.
func ingressInstallCmd(t *testing.T) *cobra.Command {
	t.Helper()
	parent := newIngressCmd()
	for _, c := range parent.Commands() {
		if c.Name() == "install" {
			return c
		}
	}
	t.Fatal("ingress command has no install subcommand")
	return nil
}

func TestWriteIngressPlanNotices(t *testing.T) {
	o := ingressOptions{issuerName: "letsencrypt"}

	// The loadbalancer plan is printed and carries the billable/HA cost notice, not the SPOF notice.
	var lb bytes.Buffer
	writeIngressPlan(&lb, o, exposeLoadBalancer, ingressManifestFor(exposeLoadBalancer), false, false)
	if !strings.Contains(lb.String(), "Plan (expose: loadbalancer)") {
		t.Errorf("loadbalancer plan should be printed:\n%s", lb.String())
	}
	if !strings.Contains(lb.String(), costNoticeMarker) || !strings.Contains(lb.String(), "high availability") {
		t.Errorf("loadbalancer plan should carry the cost/HA notice:\n%s", lb.String())
	}
	if strings.Contains(lb.String(), spofNoticeMarker) {
		t.Errorf("loadbalancer plan should not carry the SPOF notice:\n%s", lb.String())
	}

	// The nodeport plan is printed and carries the single-point-of-failure notice, not the cost one.
	var np bytes.Buffer
	writeIngressPlan(&np, o, exposeNodePort, ingressManifestFor(exposeNodePort), false, false)
	if !strings.Contains(np.String(), "Plan (expose: nodeport)") {
		t.Errorf("nodeport plan should be printed:\n%s", np.String())
	}
	if !strings.Contains(np.String(), spofNoticeMarker) {
		t.Errorf("nodeport plan should carry the SPOF notice:\n%s", np.String())
	}
	if strings.Contains(np.String(), costNoticeMarker) {
		t.Errorf("nodeport plan should not carry the cost notice:\n%s", np.String())
	}
}

func TestConfirmInstall(t *testing.T) {
	// --approve proceeds without prompting: returns true without reading stdin or printing "Proceed?".
	// It never depends on whether stdin is a terminal.
	var out bytes.Buffer
	ok, err := confirmInstall(ingressOptions{approve: true}, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatalf("confirmInstall approve: %v", err)
	}
	if !ok {
		t.Errorf("--approve should proceed without prompting")
	}
	if strings.Contains(out.String(), "Proceed?") {
		t.Errorf("--approve should not print the prompt:\n%s", out.String())
	}

	// Non-interactive (no terminal) without --approve must refuse: an error, no prompt, no proceed.
	// strings.Reader is not a terminal, so the default stdinIsTerminal seam already reports false.
	var nb bytes.Buffer
	ok, err = confirmInstall(ingressOptions{}, strings.NewReader(""), &nb)
	if err == nil {
		t.Fatalf("non-interactive confirmInstall without --approve should error")
	}
	if ok {
		t.Errorf("non-interactive confirmInstall without --approve should not proceed")
	}
	if !strings.Contains(err.Error(), "--approve") {
		t.Errorf("the non-interactive error should point at --approve:\n%v", err)
	}
	if strings.Contains(nb.String(), "Proceed?") {
		t.Errorf("non-interactive confirmInstall should not prompt:\n%s", nb.String())
	}

	// On an interactive terminal (forced via the seam) and without --approve, the prompt is shown
	// and the typed answer decides.
	origTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = origTerm })
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
