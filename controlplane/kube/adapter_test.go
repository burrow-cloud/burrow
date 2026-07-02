// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

const ns = "default"

func i32p(v int32) *int32 { return &v }

// TestWithNamespaceRoutesAppResources confirms a namespace-scoped adapter view applies app resources
// into the named namespace (an environment's namespace), while the unscoped view keeps using the
// configured app namespace (ADR-0035 phase 2b).
func TestWithNamespaceRoutesAppResources(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	const envNS = "burrow-apps-staging"
	if err := a.WithNamespace(envNS).ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload(staging): %v", err)
	}
	if _, err := client.AppsV1().Deployments(envNS).Get(ctx, "web", metav1.GetOptions{}); err != nil {
		t.Fatalf("deployment not found in %s: %v", envNS, err)
	}
	if _, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("deployment unexpectedly present in the default namespace %s (err=%v)", ns, err)
	}

	// An empty namespace, or one equal to the configured app namespace, keeps the default behavior.
	if err := a.WithNamespace("").ApplyWorkload(ctx, cp.WorkloadSpec{App: "api", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload(default): %v", err)
	}
	if _, err := client.AppsV1().Deployments(ns).Get(ctx, "api", metav1.GetOptions{}); err != nil {
		t.Errorf("default-namespace deployment missing: %v", err)
	}
}

func TestExposeCreatesServiceAndIngress(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("Expose: %v", err)
	}

	svc, err := client.CoreV1().Services(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get service: %v", err)
	}
	if svc.Spec.Selector["app.kubernetes.io/name"] != "web" {
		t.Errorf("service selector = %v", svc.Spec.Selector)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 8080 {
		t.Errorf("service ports = %+v, want 80->8080", svc.Spec.Ports)
	}

	ing, err := client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ingress: %v", err)
	}
	// The Ingress must name the ingress-nginx class, or the controller (which runs with
	// --ingress-class=nginx) ignores it and it never gets an external address.
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
		t.Errorf("ingress class = %v, want nginx", ing.Spec.IngressClassName)
	}
	rule := ing.Spec.Rules[0]
	if rule.Host != "web.example.com" {
		t.Errorf("ingress host = %q, want web.example.com", rule.Host)
	}
	if b := rule.HTTP.Paths[0].Backend.Service; b.Name != "web" || b.Port.Number != 80 {
		t.Errorf("ingress backend = %+v, want web:80", b)
	}

	// Expose is idempotent (update path).
	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web2.example.com", Port: 8080}); err != nil {
		t.Fatalf("re-Expose: %v", err)
	}
	ing, _ = client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	if ing.Spec.Rules[0].Host != "web2.example.com" {
		t.Errorf("host after update = %q, want web2.example.com", ing.Spec.Rules[0].Host)
	}
}

func TestUnexpose(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	// Unexposing nothing is ErrNotFound.
	if err := a.Unexpose(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("Unexpose missing = %v, want ErrNotFound", err)
	}

	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if err := a.Unexpose(ctx, "web"); err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	if _, err := client.CoreV1().Services(ns).Get(ctx, "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("service should be deleted, got %v", err)
	}
	if _, err := client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("ingress should be deleted, got %v", err)
	}
}

func TestExposeTLS(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080, TLS: true, Issuer: "letsencrypt"}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	ing, err := client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ingress: %v", err)
	}
	if ing.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("issuer annotation = %q, want letsencrypt", ing.Annotations["cert-manager.io/cluster-issuer"])
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "web-tls" ||
		len(ing.Spec.TLS[0].Hosts) != 1 || ing.Spec.TLS[0].Hosts[0] != "web.example.com" {
		t.Errorf("ingress TLS = %+v, want host web.example.com secret web-tls", ing.Spec.TLS)
	}

	// With no certificate Secret yet, the exposure reports TLS requested but the cert not ready.
	st, err := a.ExposureStatus(ctx, "web")
	if err != nil {
		t.Fatalf("ExposureStatus: %v", err)
	}
	if !st.TLS || st.CertReady {
		t.Errorf("before issuance: TLS=%v CertReady=%v, want TLS true, CertReady false", st.TLS, st.CertReady)
	}

	// cert-manager populates the named Secret with the certificate; CertReady then flips true.
	if _, err := client.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create tls secret: %v", err)
	}
	st, err = a.ExposureStatus(ctx, "web")
	if err != nil {
		t.Fatalf("ExposureStatus after issuance: %v", err)
	}
	if !st.CertReady {
		t.Errorf("after issuance: CertReady=false, want true")
	}
}

func TestExposureStatus(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	// Not exposed → zero status, no error.
	if st, err := a.ExposureStatus(ctx, "web"); err != nil || st.Exposed {
		t.Fatalf("unexposed status = %+v err=%v", st, err)
	}

	if err := a.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("expose: %v", err)
	}
	// Before the controller assigns an address, the host is known but the address is empty.
	st, err := a.ExposureStatus(ctx, "web")
	if err != nil || !st.Exposed || st.Host != "web.example.com" || st.Address != "" {
		t.Fatalf("pre-address status = %+v err=%v", st, err)
	}

	// Simulate the ingress controller writing the external address into the Ingress status.
	ing, _ := client.NetworkingV1().Ingresses(ns).Get(ctx, "web", metav1.GetOptions{})
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "1.2.3.4"}}
	if _, err := client.NetworkingV1().Ingresses(ns).UpdateStatus(ctx, ing, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	st, err = a.ExposureStatus(ctx, "web")
	if err != nil || st.Address != "1.2.3.4" {
		t.Errorf("status with address = %+v err=%v", st, err)
	}
}

func TestAddonDeployListDelete(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	// Add-ons land in their own namespace, separate from the app namespace (ADR-0025).
	const addonNS = "burrow-addons"
	a := kube.New(client, ns).WithAddonNamespace(addonNS)

	spec := cp.AddonSpec{Type: cp.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428, StorageGi: 5, Capabilities: []string{"logs"}}
	info, err := a.DeployAddon(ctx, spec)
	if err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	if info.Name != "burrow-logs" || info.Mode != "installed" || len(info.Capabilities) != 1 || info.Capabilities[0] != "logs" {
		t.Errorf("info = %+v, want burrow-logs installed [logs]", info)
	}
	// The endpoint points at the add-on namespace, so burrowd can reach it cross-namespace.
	if info.Endpoint != "burrow-logs."+addonNS+".svc:9428" {
		t.Errorf("endpoint = %q, want it qualified by the add-on namespace", info.Endpoint)
	}

	// A Deployment, Service, and PVC were created in the add-on namespace.
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-logs", metav1.GetOptions{}); err != nil {
		t.Errorf("deployment: %v", err)
	}
	if _, err := client.CoreV1().Services(addonNS).Get(ctx, "burrow-logs", metav1.GetOptions{}); err != nil {
		t.Errorf("service: %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, "burrow-logs", metav1.GetOptions{}); err != nil {
		t.Errorf("pvc: %v", err)
	}
	// They are not in the app namespace.
	if _, err := client.AppsV1().Deployments(ns).Get(ctx, "burrow-logs", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("add-on should not be in the app namespace, got %v", err)
	}
	// A logs add-on also gets a collector DaemonSet + ConfigMap.
	if _, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{}); err != nil {
		t.Errorf("collector daemonset: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{}); err != nil {
		t.Errorf("collector config: %v", err)
	}

	// Backend is carried through from the spec onto the returned info.
	if info.Backend != "victorialogs" {
		t.Errorf("backend = %q, want victorialogs", info.Backend)
	}

	// AddonReady probes the live Deployment: the fake's Deployment has no available replicas,
	// so it reports not-ready, and an unknown add-on is not-ready without error.
	if ready, err := a.AddonReady(ctx, "burrow-logs"); err != nil {
		t.Errorf("AddonReady(burrow-logs) err = %v", err)
	} else if ready {
		t.Errorf("AddonReady(burrow-logs) = true, want false (no available replicas in fake)")
	}
	if ready, err := a.AddonReady(ctx, "nope"); err != nil || ready {
		t.Errorf("AddonReady(nope) = %v err=%v, want false nil", ready, err)
	}

	// Delete removes it; deleting a missing add-on is ErrNotFound.
	if err := a.DeleteAddon(ctx, "burrow-logs"); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-logs", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("deployment should be gone, got %v", err)
	}
	if _, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("collector should be gone, got %v", err)
	}
	if err := a.DeleteAddon(ctx, "nope"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("delete missing = %v, want ErrNotFound", err)
	}
}

func TestAddonMetricsDeployDelete(t *testing.T) {
	ctx := context.Background()
	const addonNS = "burrow-addons"
	// The metrics vmagent scraper's ServiceAccount is pre-provisioned by the CLI at install time
	// (burrowd cannot create RBAC); with it present, the deploy proceeds.
	client := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "burrow-vmagent", Namespace: addonNS},
	})
	a := kube.New(client, ns).WithAddonNamespace(addonNS)

	spec := cp.AddonSpec{Type: cp.AddonMetrics, Backend: "victoriametrics", Image: "victoria-metrics:test", Port: 8428, StorageGi: 10, Capabilities: []string{"metrics"}}
	info, err := a.DeployAddon(ctx, spec)
	if err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	if info.Name != "burrow-metrics" || info.Backend != "victoriametrics" || len(info.Capabilities) != 1 || info.Capabilities[0] != "metrics" {
		t.Errorf("info = %+v, want burrow-metrics victoriametrics [metrics]", info)
	}

	// The store: Deployment, Service, and PVC in the add-on namespace.
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics", metav1.GetOptions{}); err != nil {
		t.Errorf("store deployment: %v", err)
	}
	if _, err := client.CoreV1().Services(addonNS).Get(ctx, "burrow-metrics", metav1.GetOptions{}); err != nil {
		t.Errorf("store service: %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, "burrow-metrics", metav1.GetOptions{}); err != nil {
		t.Errorf("store pvc: %v", err)
	}
	// The collector is a Deployment (vmagent) + ConfigMap, NOT a DaemonSet.
	col, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("collector deployment: %v", err)
	}
	if col.Spec.Template.Spec.ServiceAccountName != "burrow-vmagent" {
		t.Errorf("collector serviceAccount = %q, want burrow-vmagent", col.Spec.Template.Spec.ServiceAccountName)
	}
	if _, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{}); err != nil {
		t.Errorf("collector config: %v", err)
	}
	if _, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("metrics collector should be a Deployment, not a DaemonSet, got %v", err)
	}

	// Delete removes the store and the vmagent collector Deployment + ConfigMap.
	if err := a.DeleteAddon(ctx, "burrow-metrics"); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("store deployment should be gone, got %v", err)
	}
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("collector deployment should be gone, got %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("collector config should be gone, got %v", err)
	}
}

// TestAddonMetricsRequiresVmagentServiceAccount asserts the agent path fails cleanly when the metrics
// vmagent ServiceAccount is absent (the CLI never staged its RBAC): burrowd cannot create RBAC, so it
// returns a clear, typed ErrInvalid error WITHOUT half-deploying any vmagent resources, and the
// message points at running `burrow addon install metrics` from a kubeconfig-holding machine.
func TestAddonMetricsRequiresVmagentServiceAccount(t *testing.T) {
	ctx := context.Background()
	const addonNS = "burrow-addons"
	// No burrow-vmagent ServiceAccount: the kubeconfig-side self-heal never ran.
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns).WithAddonNamespace(addonNS)

	spec := cp.AddonSpec{Type: cp.AddonMetrics, Backend: "victoriametrics", Image: "victoria-metrics:test", Port: 8428, StorageGi: 10, Capabilities: []string{"metrics"}}
	_, err := a.DeployAddon(ctx, spec)
	if err == nil {
		t.Fatal("DeployAddon should fail when the vmagent ServiceAccount is absent")
	}
	// Typed so the API maps it to a 4xx (not a 500) and the agent sees a normal error.
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("error should wrap ErrInvalid, got %v", err)
	}
	for _, want := range []string{"one-time RBAC grant", "burrow addon install metrics"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q, got: %v", want, err)
		}
	}
	// No partial resources: the store Deployment, Service, PVC, and the collector must NOT exist.
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("store deployment must not be created on the failed precheck, got %v", err)
	}
	if _, err := client.CoreV1().Services(addonNS).Get(ctx, "burrow-metrics", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("store service must not be created on the failed precheck, got %v", err)
	}
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("collector must not be created on the failed precheck, got %v", err)
	}
}

func TestAddonCacheDeployDelete(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	const addonNS = "burrow-addons"
	a := kube.New(client, ns).WithAddonNamespace(addonNS)

	// A cache is ephemeral (StorageGi 0) and has no collector — the generic deploy path.
	spec := cp.AddonSpec{Type: cp.AddonCache, Backend: "valkey", Image: "valkey:test", Port: 6379, StorageGi: 0, Capabilities: []string{"cache"}}
	info, err := a.DeployAddon(ctx, spec)
	if err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	if info.Name != "burrow-cache" || info.Backend != "valkey" {
		t.Errorf("info = %+v, want burrow-cache valkey", info)
	}
	// Deployment and Service exist.
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-cache", metav1.GetOptions{}); err != nil {
		t.Errorf("deployment: %v", err)
	}
	if _, err := client.CoreV1().Services(addonNS).Get(ctx, "burrow-cache", metav1.GetOptions{}); err != nil {
		t.Errorf("service: %v", err)
	}
	// No PVC (ephemeral) and no collector of any kind.
	if _, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, "burrow-cache", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("cache should have no PVC, got %v", err)
	}
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-cache-collector", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("cache should have no collector, got %v", err)
	}

	if err := a.DeleteAddon(ctx, "burrow-cache"); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-cache", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("deployment should be gone, got %v", err)
	}
}

func TestListWorkloads(t *testing.T) {
	ctx := context.Background()
	mk := func(name, image string, desired, ready int32, managed bool) *appsv1.Deployment {
		labels := map[string]string{"app.kubernetes.io/name": name}
		if managed {
			labels["app.kubernetes.io/managed-by"] = "burrow"
		}
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: i32p(desired),
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: image}}}},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
		}
	}
	client := fake.NewSimpleClientset(
		mk("web", "nginx:alpine", 2, 2, true),
		mk("api", "api:1", 3, 1, true),
		mk("other", "x:1", 1, 1, false), // not Burrow-managed → excluded
	)
	a := kube.New(client, ns)

	apps, err := a.ListWorkloads(ctx)
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("got %d apps, want 2 (managed only): %+v", len(apps), apps)
	}
	// Sorted by name: api, web.
	if apps[0].App != "api" || apps[1].App != "web" {
		t.Fatalf("apps not sorted by name: %+v", apps)
	}
	if apps[1].Image != "nginx:alpine" || apps[1].DesiredReplicas != 2 || apps[1].ReadyReplicas != 2 || !apps[1].Available {
		t.Errorf("web = %+v, want nginx:alpine 2/2 available", apps[1])
	}
	if apps[0].Available {
		t.Errorf("api is 1/3 ready and should be unavailable: %+v", apps[0])
	}
}

func TestApplyCreatesDeployment(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	spec := cp.WorkloadSpec{
		App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 2,
		Env:     map[string]string{"B": "2", "A": "1"},
		Command: []string{"server", "--port", "8080"},
	}
	if err := a.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}

	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *dep.Spec.Replicas)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "img:1" {
		t.Errorf("image = %q, want img:1", c.Image)
	}
	if len(c.Command) != 3 || c.Command[0] != "server" {
		t.Errorf("command = %v", c.Command)
	}
	// Env is sorted for determinism.
	if len(c.Env) != 2 || c.Env[0].Name != "A" || c.Env[1].Name != "B" {
		t.Errorf("env = %v, want [A B] sorted", c.Env)
	}
	if dep.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "web" {
		t.Errorf("selector = %v", dep.Spec.Selector.MatchLabels)
	}
	// Every workload sources the per-app secret env via an optional envFrom (ADR-0028), so a
	// running app picks up keys from burrow-app-<app>-secrets without the values being inlined.
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil {
		t.Fatalf("envFrom = %+v, want one secretRef", c.EnvFrom)
	}
	ref := c.EnvFrom[0].SecretRef
	if ref.Name != "burrow-app-web-secrets" {
		t.Errorf("envFrom secret name = %q, want burrow-app-web-secrets", ref.Name)
	}
	if ref.Optional == nil || !*ref.Optional {
		t.Errorf("envFrom secretRef must be optional so a workload with no secrets still applies")
	}
}

func TestApplyMetricsPortAnnotatesPod(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1, MetricsPort: 8080}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ann := dep.Spec.Template.Annotations
	if ann["prometheus.io/scrape"] != "true" {
		t.Errorf("prometheus.io/scrape = %q, want true", ann["prometheus.io/scrape"])
	}
	if ann["prometheus.io/port"] != "8080" {
		t.Errorf("prometheus.io/port = %q, want 8080", ann["prometheus.io/port"])
	}
	if ann["prometheus.io/path"] != "/metrics" {
		t.Errorf("prometheus.io/path = %q, want /metrics", ann["prometheus.io/path"])
	}
}

func TestApplyNoMetricsPortAddsNoAnnotations(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := dep.Spec.Template.Annotations["prometheus.io/scrape"]; ok {
		t.Errorf("prometheus.io/scrape present with MetricsPort=0, want none (annotations=%v)", dep.Spec.Template.Annotations)
	}
}

func TestApplyUpdatesDeployment(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	_ = a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1})
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:2", Replicas: 3}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	list, _ := client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if len(list.Items) != 1 {
		t.Fatalf("got %d deployments, want 1 (update, not duplicate)", len(list.Items))
	}
	dep := list.Items[0]
	if dep.Spec.Template.Spec.Containers[0].Image != "img:2" || *dep.Spec.Replicas != 3 {
		t.Errorf("after update: image=%q replicas=%d, want img:2/3", dep.Spec.Template.Spec.Containers[0].Image, *dep.Spec.Replicas)
	}
}

// TestApplyRetriesOnConflict reproduces the resourceVersion race the e2e exposed: the
// first Update returns a 409 Conflict (as it does when the controller has modified the
// live object), and ApplyWorkload must re-read and retry rather than fail.
func TestApplyRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("initial apply: %v", err)
	}

	var updates int
	client.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("the object has been modified"))
		}
		return false, nil, nil // fall through to the default tracker
	})

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:2", Replicas: 2}); err != nil {
		t.Fatalf("apply should retry past the conflict: %v", err)
	}
	if updates < 2 {
		t.Errorf("expected a retry (>= 2 update attempts), got %d", updates)
	}
	dep, _ := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if dep.Spec.Template.Spec.Containers[0].Image != "img:2" {
		t.Errorf("image = %q, want img:2 after retried update", dep.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestApplyRejectsUnsupportedKind(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), ns)
	err := a.ApplyWorkload(context.Background(), cp.WorkloadSpec{App: "db", Kind: cp.WorkloadStatefulSet, Image: "pg:1", Replicas: 1})
	if !errors.Is(err, cp.ErrNotImplemented) {
		t.Fatalf("StatefulSet apply err = %v, want ErrNotImplemented", err)
	}
}

func TestWorkloadStatusMapping(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32p(3),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "img:1"}}}},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:   3,
			UpdatedReplicas: 3,
			Conditions:      []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}},
		},
	}
	a := kube.New(fake.NewSimpleClientset(dep), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.DesiredReplicas != 3 || st.ReadyReplicas != 3 || st.UpdatedReplicas != 3 || !st.Available {
		t.Errorf("status = %+v, want desired=ready=updated=3 available", st)
	}
	if st.Image != "img:1" || st.Kind != cp.WorkloadDeployment {
		t.Errorf("status image/kind = %q/%q", st.Image, st.Kind)
	}
}

func TestWorkloadStatusNotFound(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), ns)
	if _, err := a.WorkloadStatus(context.Background(), "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// unavailableDeployment is a Deployment with no ready replicas, so WorkloadStatus reports it not
// available and looks at the pods for a blocking condition.
func unavailableDeployment(image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32p(1),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: image}}}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 0},
	}
}

func TestWorkloadStatusImagePullIssue(t *testing.T) {
	const image = "ghcr.io/burrow-cloud/website:0.1.1"
	dep := unavailableDeployment(image)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/name": "web"}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "web",
				Image: image,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: cp.ReasonImagePullBackOff, Message: "Back-off pulling image"}},
			}},
		},
	}
	a := kube.New(fake.NewSimpleClientset(dep, pod), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.Available {
		t.Fatalf("status = %+v, want not available", st)
	}
	if st.IssueReason != cp.ReasonImagePullBackOff {
		t.Errorf("issue reason = %q, want %q", st.IssueReason, cp.ReasonImagePullBackOff)
	}
	for _, want := range []string{image, `registry "ghcr.io"`, "burrow config registry login ghcr.io"} {
		if !strings.Contains(st.Issue, want) {
			t.Errorf("issue = %q, want it to contain %q", st.Issue, want)
		}
	}
}

func TestWorkloadStatusTransientReasonNoIssue(t *testing.T) {
	dep := unavailableDeployment("img:1")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/name": "web"}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "web",
				Image: "img:1",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}},
		},
	}
	a := kube.New(fake.NewSimpleClientset(dep, pod), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.Issue != "" || st.IssueReason != "" {
		t.Errorf("transient waiting reason surfaced issue = %q / %q, want empty", st.Issue, st.IssueReason)
	}
}

// TestWorkloadStatusPodListErrorIsBestEffort confirms a failure to list pods during enrichment
// does not fail Status: the workload state is still returned, just without an Issue.
func TestWorkloadStatusPodListErrorIsBestEffort(t *testing.T) {
	dep := unavailableDeployment("img:1")
	client := fake.NewSimpleClientset(dep)
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom: pod list failed")
	})
	a := kube.New(client, ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus must not fail on a pod-list error, got: %v", err)
	}
	if st.Issue != "" || st.IssueReason != "" {
		t.Errorf("issue = %q / %q, want empty when enrichment could not list pods", st.Issue, st.IssueReason)
	}
	if st.DesiredReplicas != 1 {
		t.Errorf("desired = %d, want 1 (base status still populated)", st.DesiredReplicas)
	}
}

func TestScale(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	_ = a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1})

	if err := a.ScaleWorkload(ctx, "web", 4); err != nil {
		t.Fatalf("ScaleWorkload: %v", err)
	}
	dep, _ := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if *dep.Spec.Replicas != 4 {
		t.Errorf("replicas = %d, want 4", *dep.Spec.Replicas)
	}

	if err := a.ScaleWorkload(ctx, "ghost", 2); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("scale missing err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)
	_ = a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1})

	if err := a.DeleteWorkload(ctx, "web"); err != nil {
		t.Fatalf("DeleteWorkload: %v", err)
	}
	if _, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{}); err == nil {
		t.Errorf("deployment should be gone")
	}
	if err := a.DeleteWorkload(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("delete missing err = %v, want ErrNotFound", err)
	}
}

func TestLogs(t *testing.T) {
	ctx := context.Background()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "web-abc", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/name": "web"},
	}}
	a := kube.New(fake.NewSimpleClientset(dep, pod), ns)

	lines, err := a.Logs(ctx, "web", cp.LogOptions{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) == 0 || lines[0].Pod != "web-abc" {
		t.Fatalf("lines = %+v, want at least one line attributed to web-abc", lines)
	}

	if _, err := a.Logs(ctx, "ghost", cp.LogOptions{}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("logs for missing app err = %v, want ErrNotFound", err)
	}
}
