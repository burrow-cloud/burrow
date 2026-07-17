// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/connect"
)

// fakeRegistryDNS is a substitute registryDNSClient: it reports a fixed set of configured providers and
// records the AddDomain calls the registry install makes, so a test can assert the guarded DNS write
// happens (or does not) without a live control plane.
type fakeRegistryDNS struct {
	providers []client.Provider
	result    client.DomainResult
	err       error
	added     []addDomainCall
}

type addDomainCall struct {
	host, provider, address, app string
	confirm                      bool
}

func (f *fakeRegistryDNS) Providers(context.Context) ([]client.Provider, error) {
	return f.providers, nil
}

func (f *fakeRegistryDNS) AddDomain(_ context.Context, host, provider, address, app string, confirm bool) (client.DomainResult, error) {
	f.added = append(f.added, addDomainCall{host, provider, address, app, confirm})
	if f.err != nil {
		return client.DomainResult{}, f.err
	}
	return f.result, nil
}

// stubRegistryDNSClient substitutes the DNS-client seam with the given fake and restores it on cleanup,
// so install's DNS step never reaches a live burrowd.
func stubRegistryDNSClient(t *testing.T, f registryDNSClient) {
	t.Helper()
	orig := registryDNSClientFn
	registryDNSClientFn = func(context.Context, clusterRegistryOptions) (registryDNSClient, error) { return f, nil }
	t.Cleanup(func() { registryDNSClientFn = orig })
}

// ingressLBServiceFixture returns the ingress-nginx controller LoadBalancer Service assigned an external
// address, so ingressLoadBalancerAddress has an address to point the registry's DNS record at.
func ingressLBServiceFixture(address string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ingress-nginx-controller",
			Namespace: "ingress-nginx",
			Labels:    map[string]string{"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/component": "controller"},
		},
		Spec:   corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: address}}}},
	}
}

// TestRenderRegistryManifest asserts the standalone in-cluster registry manifest (ADR-0054) renders
// Zot's PVC, its config with garbage collection, a Deployment on the pinned image, an INTERNAL
// ClusterIP Service at the pinned port, and a PUBLIC Ingress vhost at the host — annotated to use the
// existing letsencrypt issuer, to lift nginx's body cap for large layers, and to require basic auth —
// all in the control-plane namespace.
func TestRenderRegistryManifest(t *testing.T) {
	out, err := renderRegistryManifest("burrow", "registry.example.com")
	if err != nil {
		t.Fatalf("renderRegistryManifest: %v", err)
	}
	for _, want := range []string{
		"name: burrow-registry",                            // the registry Deployment/Service/PVC/ConfigMap/Ingress
		"namespace: burrow",                                // in the control-plane namespace
		"kind: PersistentVolumeClaim",                      // its persistent volume (ADR-0053 Consequences)
		"storage: 5Gi",                                     // ... sized for accumulating build layers
		"image: ghcr.io/project-zot/zot-linux-amd64:",      // the pinned Zot image
		`"gc": true`,                                       // garbage collection so layers do not fill disk
		`"deleteUntagged": true`,                           // ... including orphaned untagged manifests
		"type: ClusterIP",                                  // the INTERNAL push endpoint (in-cluster only)
		"kind: Ingress",                                    // the PUBLIC pull endpoint
		"host: \"registry.example.com\"",                   // ... at the requested host
		`cert-manager.io/cluster-issuer: "letsencrypt"`,    // TLS via the existing HTTP-01 issuer
		`nginx.ingress.kubernetes.io/proxy-body-size: "0"`, // the load-bearing large-layer annotation
		"nginx.ingress.kubernetes.io/auth-type: basic",     // the public endpoint is authenticated
		`nginx.ingress.kubernetes.io/auth-secret: "burrow-registry-auth"`,
		"secretName: \"burrow-registry-tls\"", // cert-manager fills this
	} {
		if !strings.Contains(out, want) {
			t.Errorf("registry manifest missing %q", want)
		}
	}
	// The node-editing design is gone: no NodePort, no localhost mirror (ADR-0054 §5).
	for _, unwanted := range []string{"NodePort", "nodePort", "127.0.0.1", "registries.yaml"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the registry manifest must not contain the dropped node-editing artifact %q", unwanted)
		}
	}
	// The registry manifest must NOT carry control-plane resources — it is applied standalone (ADR-0054).
	for _, unwanted := range []string{"name: burrowd", "kind: Namespace"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the standalone registry manifest must not contain %q", unwanted)
		}
	}
}

// burrowdDeploymentFixture returns a fake burrowd Deployment with the base env the install manifests
// give it, so a test can assert `cluster registry install`/`uninstall` add or remove the build-registry
// env in place and resolve the app namespace from BURROW_NAMESPACE.
func burrowdDeploymentFixture(ns string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "burrowd", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "burrowd",
						Env:  []corev1.EnvVar{{Name: "BURROW_NAMESPACE", Value: "burrow-apps"}},
					}},
				},
			},
		},
	}
}

// registryDeploymentFixture returns a fake in-cluster registry Deployment, so a status test can drive
// the installed-present path.
func registryDeploymentFixture(ns string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "burrow-registry", Namespace: ns}}
}

// ingressStackFixtures returns the fakes verifyIngressStack's typed checks need to pass: a ready
// ingress-nginx controller Deployment and the cert-manager controller Deployment. The letsencrypt
// ClusterIssuer is a CRD, so its presence is faked through clusterIssuerPresentFn instead.
func ingressStackFixtures() []runtime.Object {
	nginx := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ingress-nginx-controller",
			Namespace: "ingress-nginx",
			Labels:    map[string]string{"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/component": "controller"},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	certManager := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-manager", Namespace: "cert-manager"},
	}
	return []runtime.Object{nginx, certManager}
}

// defaultSAFixture returns the app namespace's default ServiceAccount, which the pull-secret path
// (registryLogin -> setPullSecretOnDefaultSA) reads and attaches the credential to.
func defaultSAFixture(ns string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns}}
}

// pullSecretFixture returns a burrow-registry dockerconfigjson Secret in the app namespace with one
// entry for host, so a status test drives the "pull credential present" branch.
func pullSecretFixture(t *testing.T, ns, host string) *corev1.Secret {
	t.Helper()
	cfg := dockerConfig{Auths: map[string]dockerAuth{host: {Username: "burrow", Password: "x"}}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling pull secret: %v", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registrySecretName, Namespace: ns},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
}

// stubClusterRegistryClientset substitutes the cluster-registry clientset seam with the given fake and
// restores it on cleanup.
func stubClusterRegistryClientset(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	orig := clusterRegistryClientset
	clusterRegistryClientset = func(string) (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { clusterRegistryClientset = orig })
}

// stubClusterIssuerPresent forces the ClusterIssuer presence seam so verifyIngressStack's CRD check
// does not reach a live cluster.
func stubClusterIssuerPresent(t *testing.T, present bool) {
	t.Helper()
	orig := clusterIssuerPresentFn
	clusterIssuerPresentFn = func(context.Context, string, string) (bool, error) { return present, nil }
	t.Cleanup(func() { clusterIssuerPresentFn = orig })
}

// burrowdEnvValue returns a named env value on the burrowd container, or "" when absent, so tests can
// assert the wiring.
func burrowdEnvValue(t *testing.T, cs kubernetes.Interface, ns, name string) string {
	t.Helper()
	dep, err := cs.AppsV1().Deployments(ns).Get(context.Background(), "burrowd", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting burrowd deployment: %v", err)
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != "burrowd" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value
			}
		}
	}
	return ""
}

// TestClusterRegistryStatusAbsent asserts the bare `burrow cluster registry` reports the registry is
// not installed and prints the one-line install hint.
func TestClusterRegistryStatusAbsent(t *testing.T) {
	stubClusterRegistryClientset(t, fake.NewSimpleClientset())

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry: %v\n%s", err, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "not installed") {
		t.Errorf("status must report the registry is not installed:\n%s", s)
	}
	if !strings.Contains(s, "burrow cluster registry install") {
		t.Errorf("status must hint at installing it:\n%s", s)
	}
}

// TestClusterRegistryStatusPresent asserts the bare `burrow cluster registry` reports the installed
// registry's internal push endpoint, its public pull host (from burrowd's env), TLS certificate
// readiness (the TLS Secret is present), and that the pull credential is present in the app namespace.
func TestClusterRegistryStatusPresent(t *testing.T) {
	ns := connect.DefaultNamespace
	const host = "registry.example.com"
	burrowd := burrowdDeploymentFixture(ns)
	burrowd.Spec.Template.Spec.Containers[0].Env = append(burrowd.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: buildPublicRegistryEnv, Value: host})
	cs := fake.NewSimpleClientset(
		burrowd,
		registryDeploymentFixture(ns),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: registryTLSSecretName, Namespace: ns}},
		pullSecretFixture(t, "burrow-apps", host),
	)
	stubClusterRegistryClientset(t, cs)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"installed",
		connect.RegistryEndpoint(ns), // the internal push endpoint
		"https://" + host,            // the public pull host
		"TLS certificate:         ready",
		"Pull credential:         present",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("status missing %q:\n%s", want, s)
		}
	}
}

// TestClusterRegistryInstall asserts install verifies the ingress stack, applies the manifest
// (Deployment/Service/Ingress/PVC and its annotations), creates the basic-auth Secret guarding the
// public endpoint, installs the pull credential in the app namespace, and wires burrowd's internal
// push endpoint and public pull host (ADR-0054 §5).
func TestClusterRegistryInstall(t *testing.T) {
	ns := connect.DefaultNamespace
	const host = "registry.example.com"
	objs := append(ingressStackFixtures(), burrowdDeploymentFixture(ns), defaultSAFixture("burrow-apps"))
	cs := fake.NewSimpleClientset(objs...)
	stubClusterRegistryClientset(t, cs)
	stubClusterIssuerPresent(t, true)
	stubRegistryDNSClient(t, &fakeRegistryDNS{}) // no DNS provider configured

	var appliedManifests string
	origApply := applyFn
	applyFn = func(_ context.Context, _, _, manifests string, _ bool, _, _ io.Writer) error {
		appliedManifests = manifests
		return nil
	}
	t.Cleanup(func() { applyFn = origApply })

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "install", "--host", host}, &out, &errb); err != nil {
		t.Fatalf("cluster registry install: %v\n%s", err, errb.String())
	}

	for _, want := range []string{"name: burrow-registry", "kind: Ingress", "host: \"" + host + "\"", `proxy-body-size: "0"`} {
		if !strings.Contains(appliedManifests, want) {
			t.Errorf("install must apply the registry manifest with %q, got:\n%s", want, appliedManifests)
		}
	}
	// The basic-auth Secret guarding the public endpoint exists with an htpasswd `auth` entry.
	authSec, err := cs.CoreV1().Secrets(ns).Get(context.Background(), registryAuthSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("install must create the basic-auth secret: %v", err)
	}
	if !strings.HasPrefix(string(authSec.Data["auth"]), registryPullUsername+":{SHA}") {
		t.Errorf("basic-auth secret `auth` = %q, want a %s:{SHA}... htpasswd entry", authSec.Data["auth"], registryPullUsername)
	}
	// The same credential is installed as a pull Secret in the app namespace for the public host.
	hosts, err := registryList(context.Background(), cs, "burrow-apps")
	if err != nil {
		t.Fatalf("reading app-namespace pull secret: %v", err)
	}
	if !containsString(hosts, host) {
		t.Errorf("install must add a pull credential for %s in the app namespace, have %v", host, hosts)
	}
	// burrowd is wired: internal push endpoint and public pull host.
	if got := burrowdEnvValue(t, cs, ns, buildRegistryEnv); got != connect.RegistryEndpoint(ns) {
		t.Errorf("%s = %q, want the internal endpoint %q", buildRegistryEnv, got, connect.RegistryEndpoint(ns))
	}
	if got := burrowdEnvValue(t, cs, ns, buildPublicRegistryEnv); got != host {
		t.Errorf("%s = %q, want the public host %q", buildPublicRegistryEnv, got, host)
	}
}

// installFixtureClientset returns a fake clientset with the ingress stack, burrowd, and the app
// namespace's default ServiceAccount present — the objects an install run needs to reach the output and
// DNS stages. Extra objects (e.g. an ingress LoadBalancer Service) are appended.
func installFixtureClientset(ns string, extra ...runtime.Object) *fake.Clientset {
	objs := append(ingressStackFixtures(), burrowdDeploymentFixture(ns), defaultSAFixture("burrow-apps"))
	objs = append(objs, extra...)
	return fake.NewSimpleClientset(objs...)
}

// TestClusterRegistryInstallCondensedOutput asserts the default (non-verbose) install prints the
// condensed view (#272): a clean success line, the push-vs-pull endpoints, and the TLS note — and none
// of the verbose wiring chatter.
func TestClusterRegistryInstallCondensedOutput(t *testing.T) {
	ns := connect.DefaultNamespace
	const host = "registry.example.com"
	cs := installFixtureClientset(ns)
	stubClusterRegistryClientset(t, cs)
	stubClusterIssuerPresent(t, true)
	stubRegistryDNSClient(t, &fakeRegistryDNS{}) // no DNS provider configured
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() { applyFn = origApply })

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "install", "--host", host}, &out, &errb); err != nil {
		t.Fatalf("cluster registry install: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Registry installed at https://" + host, // the clean success line
		"Push (in-cluster, plain HTTP): " + connect.RegistryEndpoint(ns),
		"Pull (public, over TLS):       " + host,
		"the TLS certificate can take a few minutes to issue", // the prominent TLS note
	} {
		if !strings.Contains(s, want) {
			t.Errorf("condensed install output missing %q:\n%s", want, s)
		}
	}
	// The verbose-only wiring chatter must be suppressed by default.
	for _, unwanted := range []string{
		"Installing the in-cluster registry:",
		"Wired burrowd's build push endpoint",
		"external registries remain fully supported",
		"Installed a pull credential",
	} {
		if strings.Contains(s, unwanted) {
			t.Errorf("default output must not contain the verbose detail %q:\n%s", unwanted, s)
		}
	}
}

// TestClusterRegistryInstallVerbose asserts --verbose restores the operational detail behind the
// condensed default (#272): the wiring lines and the zero-target-build explainer.
func TestClusterRegistryInstallVerbose(t *testing.T) {
	ns := connect.DefaultNamespace
	const host = "registry.example.com"
	cs := installFixtureClientset(ns)
	stubClusterRegistryClientset(t, cs)
	stubClusterIssuerPresent(t, true)
	stubRegistryDNSClient(t, &fakeRegistryDNS{})
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() { applyFn = origApply })

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "install", "--host", host, "--verbose"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry install --verbose: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"Installing the in-cluster registry:",
		"Wired burrowd's build push endpoint",
		"external registries remain fully supported",
		"Registry installed at https://" + host, // the success line still prints
	} {
		if !strings.Contains(s, want) {
			t.Errorf("verbose install output missing %q:\n%s", want, s)
		}
	}
}

// TestClusterRegistryInstallCreatesDNSRecord asserts that, with a DNS provider configured and the ingress
// assigned an external address, install creates the registry's public DNS record through the guarded
// provider seam (#273), forwarding --confirm and the ingress address.
func TestClusterRegistryInstallCreatesDNSRecord(t *testing.T) {
	ns := connect.DefaultNamespace
	const host = "registry.example.com"
	const lbAddr = "203.0.113.10"
	cs := installFixtureClientset(ns, ingressLBServiceFixture(lbAddr))
	stubClusterRegistryClientset(t, cs)
	stubClusterIssuerPresent(t, true)
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() { applyFn = origApply })

	dns := &fakeRegistryDNS{
		providers: []client.Provider{{Name: "digitalocean", Type: "digitalocean", Capabilities: []string{"dns"}}},
		result:    client.DomainResult{Host: host, Provider: "digitalocean", Type: "A", Address: lbAddr},
	}
	stubRegistryDNSClient(t, dns)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "install", "--host", host, "--confirm"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry install: %v\n%s", err, errb.String())
	}
	if len(dns.added) != 1 {
		t.Fatalf("install must create exactly one DNS record via the provider seam, made %d calls: %+v", len(dns.added), dns.added)
	}
	got := dns.added[0]
	if got.host != host || got.address != lbAddr || !got.confirm {
		t.Errorf("AddDomain call = %+v, want host=%s address=%s confirm=true", got, host, lbAddr)
	}
	s := out.String()
	if !strings.Contains(s, "DNS record created: "+host+" → "+lbAddr+" (A) via digitalocean") {
		t.Errorf("install must report the created DNS record:\n%s", s)
	}
}

// TestClusterRegistryInstallNoProviderManualNote asserts that with no DNS provider configured, install
// prints the manual-record note (naming the ingress address to point at) and makes no DNS write (#273).
func TestClusterRegistryInstallNoProviderManualNote(t *testing.T) {
	ns := connect.DefaultNamespace
	const host = "registry.example.com"
	const lbAddr = "203.0.113.10"
	cs := installFixtureClientset(ns, ingressLBServiceFixture(lbAddr))
	stubClusterRegistryClientset(t, cs)
	stubClusterIssuerPresent(t, true)
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() { applyFn = origApply })

	dns := &fakeRegistryDNS{} // no providers
	stubRegistryDNSClient(t, dns)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "install", "--host", host}, &out, &errb); err != nil {
		t.Fatalf("cluster registry install: %v\n%s", err, errb.String())
	}
	if len(dns.added) != 0 {
		t.Errorf("install must make no DNS write with no provider configured, made: %+v", dns.added)
	}
	s := out.String()
	if !strings.Contains(s, "point "+host+" at your cluster's ingress load balancer ("+lbAddr+")") {
		t.Errorf("install must print the manual-record note naming the ingress address:\n%s", s)
	}
}

// TestClusterRegistryInstallRequiresHost asserts install fails clearly with no --host: the public pull
// path needs a hostname (ADR-0054 §5, the documented no-domain limitation).
func TestClusterRegistryInstallRequiresHost(t *testing.T) {
	cs := fake.NewSimpleClientset(burrowdDeploymentFixture(connect.DefaultNamespace))
	stubClusterRegistryClientset(t, cs)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"cluster", "registry", "install"}, &out, &errb)
	if err == nil {
		t.Fatal("install must fail without --host")
	}
	if !strings.Contains(err.Error(), "--host") {
		t.Errorf("error should name the missing --host, got: %v", err)
	}
}

// TestClusterRegistryInstallRequiresIngress asserts install fails and points at `burrow cluster
// ingress install` when the ingress stack (here the ClusterIssuer) is absent — the registry depends on
// it for its public TLS endpoint (ADR-0054 §5).
func TestClusterRegistryInstallRequiresIngress(t *testing.T) {
	ns := connect.DefaultNamespace
	cs := fake.NewSimpleClientset(append(ingressStackFixtures(), burrowdDeploymentFixture(ns))...)
	stubClusterRegistryClientset(t, cs)
	stubClusterIssuerPresent(t, false) // the issuer is missing

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"cluster", "registry", "install", "--host", "registry.example.com"}, &out, &errb)
	if err == nil {
		t.Fatal("install must fail when the ingress stack is incomplete")
	}
	if !strings.Contains(err.Error(), "burrow cluster ingress install") {
		t.Errorf("error should point at ingress install, got: %v", err)
	}
}

// TestClusterRegistryInstallWithoutBurrowd asserts install stops clearly when burrowd is not installed
// rather than leaving a half-wired registry.
func TestClusterRegistryInstallWithoutBurrowd(t *testing.T) {
	cs := fake.NewSimpleClientset(ingressStackFixtures()...)
	stubClusterRegistryClientset(t, cs)
	stubClusterIssuerPresent(t, true)
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() { applyFn = origApply })

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"cluster", "registry", "install", "--host", "registry.example.com"}, &out, &errb)
	if err == nil {
		t.Fatal("install must fail when burrowd is not installed")
	}
	if !strings.Contains(err.Error(), "burrowd") {
		t.Errorf("error should name the missing burrowd, got: %v", err)
	}
}

// TestClusterRegistryUninstall asserts uninstall deletes the registry resources (Deployment, Service,
// Ingress, PVC, auth Secret), removes the pull credential from the app namespace, and unsets burrowd's
// internal push endpoint and public pull host.
func TestClusterRegistryUninstall(t *testing.T) {
	ns := connect.DefaultNamespace
	const host = "registry.example.com"
	burrowd := burrowdDeploymentFixture(ns)
	burrowd.Spec.Template.Spec.Containers[0].Env = append(burrowd.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: buildRegistryEnv, Value: connect.RegistryEndpoint(ns)},
		corev1.EnvVar{Name: buildPublicRegistryEnv, Value: host})
	cs := fake.NewSimpleClientset(
		burrowd,
		registryDeploymentFixture(ns),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "burrow-registry", Namespace: ns}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "burrow-registry", Namespace: ns}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "burrow-registry-config", Namespace: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: registryAuthSecretName, Namespace: ns}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "burrow-registry", Namespace: ns}},
		defaultSAFixture("burrow-apps"),
		pullSecretFixture(t, "burrow-apps", host),
	)
	stubClusterRegistryClientset(t, cs)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "uninstall"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry uninstall: %v\n%s", err, errb.String())
	}

	if _, err := cs.AppsV1().Deployments(ns).Get(context.Background(), "burrow-registry", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("uninstall must delete the registry Deployment, get err = %v", err)
	}
	if _, err := cs.NetworkingV1().Ingresses(ns).Get(context.Background(), "burrow-registry", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("uninstall must delete the registry Ingress, get err = %v", err)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), "burrow-registry", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("uninstall must delete the registry PVC, get err = %v", err)
	}
	if got := burrowdEnvValue(t, cs, ns, buildRegistryEnv); got != "" {
		t.Errorf("uninstall must unset %s, still %q", buildRegistryEnv, got)
	}
	if got := burrowdEnvValue(t, cs, ns, buildPublicRegistryEnv); got != "" {
		t.Errorf("uninstall must unset %s, still %q", buildPublicRegistryEnv, got)
	}
	// The pull credential for the host is removed from the app namespace.
	hosts, err := registryList(context.Background(), cs, "burrow-apps")
	if err != nil {
		t.Fatalf("reading app-namespace pull secret: %v", err)
	}
	if containsString(hosts, host) {
		t.Errorf("uninstall must remove the pull credential for %s, still have %v", host, hosts)
	}
}

// TestClusterRegistryUninstallIdempotent asserts uninstall on a cluster with nothing to remove (no
// registry, no burrowd env) succeeds without error — every deletion tolerates already-gone resources.
func TestClusterRegistryUninstallIdempotent(t *testing.T) {
	cs := fake.NewSimpleClientset(burrowdDeploymentFixture(connect.DefaultNamespace))
	stubClusterRegistryClientset(t, cs)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "uninstall"}, &out, &errb); err != nil {
		t.Fatalf("uninstall must be idempotent, got: %v\n%s", err, errb.String())
	}
}
