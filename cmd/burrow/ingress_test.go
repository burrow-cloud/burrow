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

	// An orphan "nginx" IngressClass with NO running controller must NOT count as present: the class
	// outlives a deleted controller, and keying off it would wrongly skip the install. cert-manager is
	// still detected by its Deployment.
	cs = fake.NewSimpleClientset(
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager", Namespace: "cert-manager"}},
	)
	if got, _ := ingressControllerPresent(ctx, cs); got {
		t.Errorf("an orphan nginx IngressClass with no running controller must not be reported present")
	}
	if got, _ := certManagerPresent(ctx, cs); !got {
		t.Errorf("cert-manager should be detected via its Deployment")
	}

	// A ready ingress-nginx controller Deployment (matched by the standard recommended labels) is the
	// real present signal, wherever its namespace.
	cs = fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ingress-nginx-controller",
				Namespace: "ingress-nginx",
				Labels: map[string]string{
					"app.kubernetes.io/name":      "ingress-nginx",
					"app.kubernetes.io/component": "controller",
				},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
		},
	)
	if got, _ := ingressControllerPresent(ctx, cs); !got {
		t.Errorf("a ready ingress-nginx controller Deployment should be detected as present")
	}

	// The same controller Deployment with 0 ready replicas is NOT present: install must proceed until
	// a replica is actually running.
	cs = fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ingress-nginx-controller",
				Namespace: "ingress-nginx",
				Labels: map[string]string{
					"app.kubernetes.io/name":      "ingress-nginx",
					"app.kubernetes.io/component": "controller",
				},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 0},
		},
	)
	if got, _ := ingressControllerPresent(ctx, cs); got {
		t.Errorf("a controller Deployment with 0 ready replicas must not be reported present")
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

// costNoticeMarker is the distinctive phrase the LoadBalancer cost/HA notice carries; a run that
// creates no LoadBalancer must never print it. The notice is provider-agnostic beyond the
// DigitalOcean example price.
const costNoticeMarker = "a LoadBalancer is billable"

func TestIngressInstallDryRunExpose(t *testing.T) {
	// loadbalancer: dry-run has no live provider, so the plan frames the cost honestly — billable on a
	// cloud, free on servicelb / MetalLB — rather than asserting the current always-billable note.
	var lb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "ingress", "install", "--dry-run", "--expose", "loadbalancer"}, &lb, &lb); err != nil {
		t.Fatalf("dry-run loadbalancer: %v", err)
	}
	if strings.Contains(lb.String(), costNoticeMarker) {
		t.Errorf("loadbalancer dry-run should not assert the always-billable note (provider is unknown until apply):\n%s", lb.String())
	}
	for _, want := range []string{"billable", "free", "servicelb", "MetalLB"} {
		if !strings.Contains(lb.String(), want) {
			t.Errorf("loadbalancer dry-run notice should mention %q so cost framing is honest before the probe:\n%s", want, lb.String())
		}
	}
	if !strings.Contains(lb.String(), ingressNginxManifest) {
		t.Errorf("loadbalancer dry-run should reference the cloud manifest:\n%s", lb.String())
	}

	// auto: same honest cost framing, and it names MetalLB as the fallback when no LoadBalancer
	// provider is present (ADR-0043) rather than mentioning NodePort.
	var auto bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "ingress", "install", "--dry-run", "--expose", "auto"}, &auto, &auto); err != nil {
		t.Fatalf("dry-run auto: %v", err)
	}
	for _, want := range []string{"billable", "free", "MetalLB"} {
		if !strings.Contains(auto.String(), want) {
			t.Errorf("auto dry-run should frame cost honestly and name MetalLB as the no-provider fallback:\n%s", auto.String())
		}
	}
	if strings.Contains(strings.ToLower(auto.String()), "nodeport") {
		t.Errorf("auto dry-run must not mention NodePort (dropped by ADR-0043):\n%s", auto.String())
	}

	// nodeport is no longer a valid --expose value: it is rejected before any cluster contact (ADR-0043).
	var np bytes.Buffer
	err := run(context.Background(), []string{"cluster", "ingress", "install", "--dry-run", "--expose", "nodeport"}, &np, &np)
	if err == nil {
		t.Fatalf("--expose nodeport should be rejected, got nil error; output:\n%s", np.String())
	}
	if !strings.Contains(err.Error(), "nodeport") || !strings.Contains(err.Error(), "loadbalancer") {
		t.Errorf("the rejection should name the invalid value and the valid ones:\n%v", err)
	}
}

func TestResolveExposeAuto(t *testing.T) {
	ctx := context.Background()

	// A known cloud provider (DigitalOcean) supports LoadBalancer, so auto picks loadbalancer and
	// reports the cloud provider (a billable LB).
	cloud := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "digitalocean://123"}},
	)
	expose, provider, err := resolveExpose(ctx, exposeAuto, cloud)
	if err != nil {
		t.Fatalf("resolveExpose cloud: %v", err)
	}
	if expose != exposeLoadBalancer {
		t.Errorf("auto on a cloud provider should pick loadbalancer, got %q", expose)
	}
	if provider != "digitalocean" {
		t.Errorf("auto on DigitalOcean should report the digitalocean provider, got %q", provider)
	}
	if !billableLoadBalancer(provider) {
		t.Errorf("a cloud provider LoadBalancer should be billable, got provider %q", provider)
	}

	// k3s (servicelb) also supports LoadBalancer, so auto picks loadbalancer but reports the free
	// servicelb provider — no cloud LB to pay for.
	k3s := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "k3s://n1"}},
	)
	expose, provider, err = resolveExpose(ctx, exposeAuto, k3s)
	if err != nil {
		t.Fatalf("resolveExpose k3s: %v", err)
	}
	if expose != exposeLoadBalancer {
		t.Errorf("auto on k3s (servicelb) should pick loadbalancer, got %q", expose)
	}
	if provider != lbProviderServiceLB {
		t.Errorf("auto on k3s should report the servicelb provider, got %q", provider)
	}
	if billableLoadBalancer(provider) {
		t.Errorf("a servicelb LoadBalancer must not be billable, got provider %q", provider)
	}

	// Bare-metal (no recognized providerID, no servicelb, no MetalLB) has no LoadBalancer provider, so
	// auto does NOT fall back to NodePort (dropped by ADR-0043): it errors, guiding the operator to
	// install MetalLB.
	bare := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	if _, _, err := resolveExpose(ctx, exposeAuto, bare); err == nil {
		t.Fatal("auto on a cluster with no LoadBalancer provider should error, not fall back to NodePort")
	} else if !strings.Contains(err.Error(), "MetalLB") {
		t.Errorf("the no-LoadBalancer error should guide toward MetalLB, not NodePort:\n%v", err)
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

	// A cloud-provider loadbalancer plan is printed and carries the billable cost notice, and its
	// manifest-variant line reads "cloud".
	var lb bytes.Buffer
	writeIngressPlan(&lb, o, exposeLoadBalancer, "digitalocean", ingressNginxManifest, false, false)
	if !strings.Contains(lb.String(), "Plan (expose: loadbalancer)") {
		t.Errorf("loadbalancer plan should be printed:\n%s", lb.String())
	}
	if !strings.Contains(lb.String(), costNoticeMarker) {
		t.Errorf("cloud loadbalancer plan should carry the billable cost notice:\n%s", lb.String())
	}
	if !strings.Contains(lb.String(), "cloud, LoadBalancer Service") {
		t.Errorf("cloud loadbalancer plan should label the cloud LoadBalancer variant:\n%s", lb.String())
	}

	// An explicit --expose loadbalancer with no probe (empty provider) is treated conservatively as a
	// billable cloud LB: the cost notice still prints so the cost gate is not silently dropped.
	var unprobed bytes.Buffer
	writeIngressPlan(&unprobed, o, exposeLoadBalancer, "", ingressNginxManifest, false, false)
	if !strings.Contains(unprobed.String(), costNoticeMarker) {
		t.Errorf("an unprobed loadbalancer should keep the billable cost notice:\n%s", unprobed.String())
	}

	// A servicelb (k3s) loadbalancer plan says FREE, names servicelb, and omits the billable cost note.
	var slb bytes.Buffer
	writeIngressPlan(&slb, o, exposeLoadBalancer, lbProviderServiceLB, ingressNginxManifest, false, false)
	if strings.Contains(slb.String(), costNoticeMarker) {
		t.Errorf("a servicelb loadbalancer plan must not carry the billable cost notice:\n%s", slb.String())
	}
	if !strings.Contains(slb.String(), "free") || !strings.Contains(slb.String(), "servicelb") {
		t.Errorf("a servicelb loadbalancer plan should say it is free and name servicelb:\n%s", slb.String())
	}

	// A MetalLB loadbalancer plan says FREE, names MetalLB, and omits the billable cost note.
	var mlb bytes.Buffer
	writeIngressPlan(&mlb, o, exposeLoadBalancer, lbProviderMetalLB, ingressNginxManifest, false, false)
	if strings.Contains(mlb.String(), costNoticeMarker) {
		t.Errorf("a MetalLB loadbalancer plan must not carry the billable cost notice:\n%s", mlb.String())
	}
	if !strings.Contains(mlb.String(), "free") || !strings.Contains(mlb.String(), "MetalLB") {
		t.Errorf("a MetalLB loadbalancer plan should say it is free and name MetalLB:\n%s", mlb.String())
	}

	// Issue #268: when ingress-nginx is already present this run provisions no new LoadBalancer Service,
	// so the plan must omit the billable cost note (and any LoadBalancer notice) even for a billable
	// cloud provider — it would imply a charge that will not be incurred. The skip line still prints.
	var present bytes.Buffer
	writeIngressPlan(&present, o, exposeLoadBalancer, "digitalocean", ingressNginxManifest, true, true)
	if strings.Contains(present.String(), costNoticeMarker) {
		t.Errorf("no cost note should print when ingress-nginx is already present (no LoadBalancer created):\n%s", present.String())
	}
	if !strings.Contains(present.String(), "ingress-nginx: already present, skip.") {
		t.Errorf("plan should record ingress-nginx as already present:\n%s", present.String())
	}
}

func TestConfirmInstallAutoResolvedGate(t *testing.T) {
	ctx := context.Background()

	// The gate keys off the resolved mode, so auto flows through resolveExpose first: a cloud
	// provider resolves to the billable loadbalancer (gated, refused non-interactively), while a
	// cluster with no LoadBalancer provider errors at resolution (guiding to MetalLB, ADR-0043).
	origTerm := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = origTerm })
	stdinIsTerminal = func(io.Reader) bool { return false } // non-interactive

	cloud := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "digitalocean://123"}},
	)
	expose, provider, err := resolveExpose(ctx, exposeAuto, cloud)
	if err != nil {
		t.Fatalf("resolveExpose cloud: %v", err)
	}
	var cb bytes.Buffer
	if _, err := confirmInstall(ingressOptions{}, expose, provider, false, strings.NewReader(""), &cb); err == nil {
		t.Errorf("auto resolving to a billable cloud loadbalancer should be gated non-interactively without --approve")
	}

	// k3s (servicelb) auto-resolves to loadbalancer but the LB is free, so the install must NOT be
	// gated: it proceeds non-interactively with no --approve (the dogfooding bug — a free servicelb LB
	// was wrongly gated behind --approve).
	k3s := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "k3s://n1"}},
	)
	expose, provider, err = resolveExpose(ctx, exposeAuto, k3s)
	if err != nil {
		t.Fatalf("resolveExpose k3s: %v", err)
	}
	var kb bytes.Buffer
	ok, err := confirmInstall(ingressOptions{}, expose, provider, false, strings.NewReader(""), &kb)
	if err != nil {
		t.Fatalf("auto resolving to a free servicelb loadbalancer should not error: %v", err)
	}
	if !ok {
		t.Errorf("auto resolving to a free servicelb loadbalancer should proceed without approval")
	}

	// A cluster with no LoadBalancer provider no longer resolves to a NodePort install; it errors so
	// the operator installs MetalLB first (ADR-0043), and the gate is never reached.
	bare := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}})
	if _, _, err := resolveExpose(ctx, exposeAuto, bare); err == nil {
		t.Errorf("auto on a cluster with no LoadBalancer provider should error rather than resolve to NodePort")
	}
}

func TestApplyDetail(t *testing.T) {
	// The captured non-verbose apply summary condenses to just the count phrase.
	for _, tc := range []struct {
		in, want string
	}{
		{"✓ Applied 19 resource(s): 13 created, 6 configured.\n", "13 created, 6 configured"},
		{"✓ Applied 1 resource(s): 1 unchanged.\n", "1 unchanged"},
		// An unexpected shape falls back to the trimmed text rather than an empty detail.
		{"  something else  ", "something else"},
	} {
		if got := applyDetail(tc.in); got != tc.want {
			t.Errorf("applyDetail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIngressReporterDone(t *testing.T) {
	// A bytes.Buffer is non-terminal, so the reporter prints only the final aligned line with a plain
	// ✓ (no ANSI, no carriage return) and pads the component name into a column so lines scan.
	var b bytes.Buffer
	r := ingressReporter{w: &b}
	r.working("ingress-nginx", "installing") // no-op on non-terminal
	r.done("ingress-nginx", "installed (13 created, 6 configured), controller ready")
	r.done("cert-manager", "already present, webhook ready")
	r.done("ClusterIssuer", `"letsencrypt" applied (Let's Encrypt production)`)
	s := b.String()

	if strings.ContainsAny(s, "\r\x1b") {
		t.Errorf("non-terminal reporter output must have no carriage return or ANSI escape:\n%q", s)
	}
	for _, want := range []string{
		"  ✓ ingress-nginx  installed (13 created, 6 configured), controller ready\n",
		"  ✓ cert-manager   already present, webhook ready\n",
		`  ✓ ClusterIssuer  "letsencrypt" applied (Let's Encrypt production)` + "\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("reporter output missing per-component line %q:\n%s", want, s)
		}
	}
	// The status text starts at the same column on every line (names aligned).
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	col := strings.Index(lines[0], "installed")
	if col <= 0 || strings.Index(lines[1], "already") != col {
		t.Errorf("component status columns are not aligned:\n%s", s)
	}
}

func TestWriteIngressDone(t *testing.T) {
	// Without --email the done block carries the actionable note that names how to add one later, plus
	// the next-step hints.
	var noEmail bytes.Buffer
	writeIngressDone(&noEmail, ingressOptions{})
	s := noEmail.String()
	for _, want := range []string{
		"Ingress and TLS are set up.",
		"no --email set",
		"burrow cluster ingress install --email <you@example.com>",
		"burrow app publish <app> --host <name> --port <n> --tls",
		"burrow app reachability <app>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("done block missing %q:\n%s", want, s)
		}
	}

	// With --email the note is suppressed; the next-step hints still print.
	var withEmail bytes.Buffer
	writeIngressDone(&withEmail, ingressOptions{email: "me@example.com"})
	if strings.Contains(withEmail.String(), "no --email set") {
		t.Errorf("the no-email note must not print when --email is given:\n%s", withEmail.String())
	}
	if !strings.Contains(withEmail.String(), "burrow app publish") {
		t.Errorf("done block should still print the next-step hints with --email:\n%s", withEmail.String())
	}
}

func TestConfirmInstall(t *testing.T) {
	// The gate keys off the RESOLVED expose mode: only the billable loadbalancer path is gated.

	// A billable cloud loadbalancer + --approve proceeds without prompting: returns true without reading
	// stdin or printing "Proceed?". It never depends on whether stdin is a terminal.
	var out bytes.Buffer
	ok, err := confirmInstall(ingressOptions{approve: true}, exposeLoadBalancer, "digitalocean", false, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatalf("confirmInstall approve: %v", err)
	}
	if !ok {
		t.Errorf("loadbalancer + --approve should proceed without prompting")
	}
	if strings.Contains(out.String(), "Proceed?") {
		t.Errorf("--approve should not print the prompt:\n%s", out.String())
	}

	// A billable cloud loadbalancer + non-interactive (no terminal) without --approve must refuse: an
	// error, no prompt, no proceed. strings.Reader is not a terminal, so the default stdinIsTerminal
	// seam already reports false.
	var nb bytes.Buffer
	ok, err = confirmInstall(ingressOptions{}, exposeLoadBalancer, "digitalocean", false, strings.NewReader(""), &nb)
	if err == nil {
		t.Fatalf("non-interactive loadbalancer confirmInstall without --approve should error")
	}
	if ok {
		t.Errorf("non-interactive loadbalancer confirmInstall without --approve should not proceed")
	}
	if !strings.Contains(err.Error(), "--approve") {
		t.Errorf("the non-interactive error should point at --approve:\n%v", err)
	}
	if !strings.Contains(err.Error(), "billable") {
		t.Errorf("the non-interactive error should be about the billable load balancer:\n%v", err)
	}
	if strings.Contains(nb.String(), "Proceed?") {
		t.Errorf("non-interactive confirmInstall should not prompt:\n%s", nb.String())
	}

	// The non-billable paths are NEVER gated: they proceed with no error, no prompt, and no --approve,
	// whether interactive or not. This covers a free servicelb / MetalLB LoadBalancer (the dogfooding
	// bug — a free servicelb LB installed non-interactively must not error asking for --approve), and a
	// run where ingress-nginx is already present so no LoadBalancer Service is created at all even for a
	// billable cloud provider (issue #268).
	origTerm := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = origTerm })
	stdinIsTerminal = func(io.Reader) bool { return false } // non-interactive
	for _, tc := range []struct {
		name     string
		o        ingressOptions
		expose   string
		provider string
		hasNginx bool
	}{
		{"servicelb loadbalancer is free, not gated", ingressOptions{}, exposeLoadBalancer, lbProviderServiceLB, false},
		{"metallb loadbalancer is free, not gated", ingressOptions{}, exposeLoadBalancer, lbProviderMetalLB, false},
		{"ingress already present creates no LB, not gated", ingressOptions{}, exposeLoadBalancer, "digitalocean", true},
	} {
		var b bytes.Buffer
		ok, err := confirmInstall(tc.o, tc.expose, tc.provider, tc.hasNginx, strings.NewReader(""), &b)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if !ok {
			t.Errorf("%s: should proceed", tc.name)
		}
		if strings.Contains(b.String(), "Proceed?") {
			t.Errorf("%s: should not prompt:\n%s", tc.name, b.String())
		}
	}

	// A billable cloud loadbalancer on an interactive terminal (forced via the seam) and without
	// --approve: the prompt is shown and the typed answer decides.
	stdinIsTerminal = func(io.Reader) bool { return true }
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
		ok, err := confirmInstall(ingressOptions{}, exposeLoadBalancer, "digitalocean", false, strings.NewReader(tc.in), &b)
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
