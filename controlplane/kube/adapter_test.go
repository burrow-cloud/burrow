// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	info, err := a.DeployAddon(ctx, spec, cp.DefaultEnvironment, nil)
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
	if _, err := a.DeleteAddon(ctx, "burrow-logs", cp.AddonLogs, true); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-logs", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("deployment should be gone, got %v", err)
	}
	if _, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("collector should be gone, got %v", err)
	}
	if _, err := a.DeleteAddon(ctx, "nope", cp.AddonLogs, true); !errors.Is(err, cp.ErrNotFound) {
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
	info, err := a.DeployAddon(ctx, spec, cp.DefaultEnvironment, nil)
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
	if _, err := a.DeleteAddon(ctx, "burrow-metrics", cp.AddonMetrics, true); err != nil {
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
	_, err := a.DeployAddon(ctx, spec, cp.DefaultEnvironment, nil)
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
	info, err := a.DeployAddon(ctx, spec, cp.DefaultEnvironment, nil)
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

	if _, err := a.DeleteAddon(ctx, "burrow-cache", cp.AddonCache, true); err != nil {
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

func TestApplyStampsReleaseAnnotation(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1, ReleaseID: "rel-abc"}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := dep.Spec.Template.Annotations[cp.ReleaseAnnotation]; got != "rel-abc" {
		t.Errorf("%s = %q, want rel-abc", cp.ReleaseAnnotation, got)
	}
}

func TestApplyReleaseAnnotationCoexistsWithMetrics(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1, MetricsPort: 8080, ReleaseID: "rel-xyz"}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ann := dep.Spec.Template.Annotations
	if ann[cp.ReleaseAnnotation] != "rel-xyz" {
		t.Errorf("%s = %q, want rel-xyz", cp.ReleaseAnnotation, ann[cp.ReleaseAnnotation])
	}
	// The release stamp must not clobber the metrics annotations.
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

func TestApplyNoReleaseIDAddsNoReleaseAnnotation(t *testing.T) {
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
	if _, ok := dep.Spec.Template.Annotations[cp.ReleaseAnnotation]; ok {
		t.Errorf("%s present with empty ReleaseID, want none (annotations=%v)", cp.ReleaseAnnotation, dep.Spec.Template.Annotations)
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

// wedgedRolloutDeployment models issue #307: the new release cannot pull its image, so the newest
// revision is stuck while the PREVIOUS ReplicaSet's pods keep serving. Kubernetes holds the
// DeploymentAvailable condition True (the old pods meet minimum availability) and ReadyReplicas at
// the old count, but UpdatedReplicas has not reached desired — the signal that the current release
// has not rolled out. It carries the Burrow labels so it is included by ListWorkloads too.
func wedgedRolloutDeployment(image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/name": "web", "app.kubernetes.io/managed-by": "burrow"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32p(2),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: image}}}},
		},
		Status: appsv1.DeploymentStatus{
			Replicas:          3, // 2 old + 1 surged new
			ReadyReplicas:     2, // old pods still serving
			AvailableReplicas: 2,
			UpdatedReplicas:   1, // the new ReplicaSet's pod, not yet ready
			Conditions:        []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}},
		},
	}
}

// waitingPod is a pod labelled for app "web" whose container is waiting for the given reason, so
// the adapter's pod inspection finds it.
func waitingPod(image, reason, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-new", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/name": "web"}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "web",
				Image: image,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
			}},
		},
	}
}

// TestWorkloadStatusWedgedRolloutNotAvailable is the regression for issue #307: a new release
// stuck in ImagePullBackOff while the old pods keep serving must report NOT available with the
// actionable Issue — not "available" on the strength of the superseded release. It fails before
// the fix, where WorkloadStatus returned the DeploymentAvailable condition (True) and skipped the
// pod inspection whenever the workload looked available.
func TestWorkloadStatusWedgedRolloutNotAvailable(t *testing.T) {
	const image = "ghcr.io/burrow-cloud/website:0.1.2"
	a := kube.New(fake.NewSimpleClientset(
		wedgedRolloutDeployment(image),
		waitingPod(image, cp.ReasonImagePullBackOff, "Back-off pulling image"),
	), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.Available {
		t.Fatalf("wedged rollout reported available; the old release serves but the new one cannot pull: %+v", st)
	}
	if st.IssueReason != cp.ReasonImagePullBackOff {
		t.Errorf("issue reason = %q, want %q", st.IssueReason, cp.ReasonImagePullBackOff)
	}
	if !strings.Contains(st.Issue, image) {
		t.Errorf("issue = %q, want it to name the image %q", st.Issue, image)
	}
}

// TestWorkloadStatusInProgressRolloutStaysAvailable guards the other side: a normal rolling update
// (old pods serving, the new pod merely ContainerCreating and the deadline not exceeded) is NOT
// flagged as broken — a deploy that will succeed in a few seconds keeps reading available with no
// issue.
func TestWorkloadStatusInProgressRolloutStaysAvailable(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(
		wedgedRolloutDeployment("img:2"),
		waitingPod("img:2", "ContainerCreating", ""),
	), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if !st.Available {
		t.Fatalf("normal in-progress rollout flagged not-available: %+v", st)
	}
	if st.Issue != "" || st.IssueReason != "" {
		t.Errorf("in-progress rollout carries issue = %q / %q, want empty", st.Issue, st.IssueReason)
	}
}

// TestWorkloadStatusProgressDeadlineExceededNotAvailable covers a rollout wedged for a reason other
// than an image pull (e.g. a crash loop): the Progressing condition reports
// ProgressDeadlineExceeded, so the app reads not-available even though the old ReplicaSet keeps the
// Available condition True.
func TestWorkloadStatusProgressDeadlineExceededNotAvailable(t *testing.T) {
	dep := wedgedRolloutDeployment("img:2")
	dep.Status.Conditions = append(dep.Status.Conditions, appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
	})
	a := kube.New(fake.NewSimpleClientset(dep), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.Available {
		t.Fatalf("rollout past its progress deadline reported available: %+v", st)
	}
}

// TestListWorkloadsSurfacesWedgedRollout is the #307 regression on the list path: `burrow app list`
// must report the wedged app not-available and carry the Issue, so a broken deploy is visible
// without opening logs. It fails before the fix, where ListWorkloads ran no pod inspection at all.
func TestListWorkloadsSurfacesWedgedRollout(t *testing.T) {
	const image = "ghcr.io/burrow-cloud/website:0.1.2"
	a := kube.New(fake.NewSimpleClientset(
		wedgedRolloutDeployment(image),
		waitingPod(image, cp.ReasonImagePullBackOff, "Back-off pulling image"),
	), ns)

	apps, err := a.ListWorkloads(context.Background())
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("got %d apps, want 1: %+v", len(apps), apps)
	}
	if apps[0].Available {
		t.Fatalf("list reported wedged app available: %+v", apps[0])
	}
	if apps[0].IssueReason != cp.ReasonImagePullBackOff {
		t.Errorf("issue reason = %q, want %q", apps[0].IssueReason, cp.ReasonImagePullBackOff)
	}
	if !strings.Contains(apps[0].Issue, image) {
		t.Errorf("issue = %q, want it to name the image %q", apps[0].Issue, image)
	}
}

// The tests below cover the blocking classes ADR-0074 §2 added to the Issue vocabulary. Each pins
// the ACTIONABLE part of the message — the taint, the exit code, the missing key, the memory limit
// — because a reason with no actionable detail is the silence the record is about. Before the
// change every one of these produced Available:false with an EMPTY Issue.

// appPod is a pod labelled for app "web", so the adapter's pod inspection selects it.
func appPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/name": "web"},
	}}
}

// unschedulablePod is a pod the scheduler has rejected, carrying the scheduler's own verdict.
// transitioned is how long ago it became unschedulable — the adapter ignores a pod that has only
// just been rejected, since a rolling update and a scaling cluster both produce those transiently.
func unschedulablePod(message string, transitioned time.Duration) *corev1.Pod {
	pod := appPod("web-pending")
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{
			Type:               corev1.PodScheduled,
			Status:             corev1.ConditionFalse,
			Reason:             corev1.PodReasonUnschedulable,
			Message:            message,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-transitioned)),
		}},
	}
	return pod
}

// TestWorkloadStatusUnschedulableIssue: a pod no node can run must say WHAT could not be satisfied
// — the scheduler already names the taint and the resource, and before ADR-0074 §2 Burrow threw
// that away and reported only "not available".
func TestWorkloadStatusUnschedulableIssue(t *testing.T) {
	const verdict = "0/3 nodes are available: 1 node(s) had untolerated taint {workload: gpu}, 2 Insufficient cpu."
	a := kube.New(fake.NewSimpleClientset(unavailableDeployment("img:1"), unschedulablePod(verdict, time.Hour)), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.Available {
		t.Fatalf("status = %+v, want not available", st)
	}
	if st.IssueReason != cp.ReasonUnschedulable {
		t.Errorf("issue reason = %q, want %q", st.IssueReason, cp.ReasonUnschedulable)
	}
	for _, want := range []string{"untolerated taint {workload: gpu}", "Insufficient cpu"} {
		if !strings.Contains(st.Issue, want) {
			t.Errorf("issue = %q, want it to contain %q", st.Issue, want)
		}
	}
}

// TestWorkloadStatusRecentlyUnschedulableNoIssue is the other half of the criterion: a pod the
// scheduler rejected moments ago is a rollout in progress or a cluster adding a node, not a problem
// a person has to fix, so it must not flip a serving app to not-available.
func TestWorkloadStatusRecentlyUnschedulableNoIssue(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(
		wedgedRolloutDeployment("img:2"),
		unschedulablePod("0/3 nodes are available: 3 Insufficient cpu.", time.Second),
	), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if !st.Available {
		t.Fatalf("a pod unschedulable for one second flagged the app not-available: %+v", st)
	}
	if st.Issue != "" || st.IssueReason != "" {
		t.Errorf("issue = %q / %q, want empty inside the grace period", st.Issue, st.IssueReason)
	}
}

// TestWorkloadStatusVolumeUnavailableIssue: the scheduler reports an unbindable claim as
// "unschedulable" too, but the fix is a claim or a StorageClass rather than capacity, so it gets
// its own reason an agent can branch on.
func TestWorkloadStatusVolumeUnavailableIssue(t *testing.T) {
	const verdict = "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims."
	a := kube.New(fake.NewSimpleClientset(unavailableDeployment("img:1"), unschedulablePod(verdict, time.Hour)), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.IssueReason != cp.ReasonVolumeUnavailable {
		t.Errorf("issue reason = %q, want %q", st.IssueReason, cp.ReasonVolumeUnavailable)
	}
	if !strings.Contains(st.Issue, "unbound immediate PersistentVolumeClaims") {
		t.Errorf("issue = %q, want it to carry the scheduler's verdict", st.Issue)
	}
}

// crashLoopPod is a pod whose container is backing off after a failed run, carrying the exit code
// of the run that failed — the fact that separates a rejected config from a signalled process.
func crashLoopPod(exitCode int32) *corev1.Pod {
	pod := appPod("web-crash")
	pod.Spec = corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "img:1"}}}
	pod.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "web",
		Image: "img:1",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: cp.ReasonCrashLoopBackOff, Message: "back-off 5m0s restarting failed container",
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: exitCode,
		}},
	}}}
	return pod
}

// TestWorkloadStatusCrashLoopIssue: the exit code and the previous run's output are what a crash
// loop is actually about, and they are exactly what `burrow status` used to omit.
func TestWorkloadStatusCrashLoopIssue(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(unavailableDeployment("img:1"), crashLoopPod(2)), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.Available {
		t.Fatalf("status = %+v, want not available", st)
	}
	if st.IssueReason != cp.ReasonCrashLoopBackOff {
		t.Errorf("issue reason = %q, want %q", st.IssueReason, cp.ReasonCrashLoopBackOff)
	}
	for _, want := range []string{
		`container "web"`,
		"exited with code 2",
		"application's own output", // the tail is labelled, never implied to be sanitised
		"fake logs",                // what the fake clientset's log stream returns
	} {
		if !strings.Contains(st.Issue, want) {
			t.Errorf("issue = %q, want it to contain %q", st.Issue, want)
		}
	}
}

// TestWorkloadStatusOOMKilledOutranksCrashLoop: an OOM-killed container reports CrashLoopBackOff
// too, and reporting the restart rather than the kill would point the reader at the wrong fix. The
// single Issue field takes the more specific of the pair, and it names the limit (ADR-0074 §5).
func TestWorkloadStatusOOMKilledOutranksCrashLoop(t *testing.T) {
	pod := crashLoopPod(137)
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.Reason = cp.ReasonOOMKilled
	a := kube.New(fake.NewSimpleClientset(unavailableDeployment("img:1"), pod), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.IssueReason != cp.ReasonOOMKilled {
		t.Errorf("issue reason = %q, want %q", st.IssueReason, cp.ReasonOOMKilled)
	}
	if !strings.Contains(st.Issue, "128Mi") {
		t.Errorf("issue = %q, want it to name the memory limit that was hit", st.Issue)
	}
}

// TestWorkloadStatusConfigErrorNamesTheKeyNotTheValue: the missing KEY is the actionable part and
// is safe to print; a value never is (ADR-0074 §9). The kubelet's message carries the key, so this
// also asserts the value the Secret would hold does not appear.
func TestWorkloadStatusConfigErrorNamesTheKeyNotTheValue(t *testing.T) {
	pod := appPod("web-config")
	pod.Status = corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "web",
		Image: "img:1",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  cp.ReasonCreateContainerConfigError,
			Message: `couldn't find key STRIPE_API_KEY in Secret default/burrow-app-web-secrets`,
		}},
	}}}
	a := kube.New(fake.NewSimpleClientset(unavailableDeployment("img:1"), pod), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.IssueReason != cp.ReasonCreateContainerConfigError {
		t.Errorf("issue reason = %q, want %q", st.IssueReason, cp.ReasonCreateContainerConfigError)
	}
	if !strings.Contains(st.Issue, "STRIPE_API_KEY") {
		t.Errorf("issue = %q, want it to name the missing key", st.Issue)
	}
	if strings.Contains(st.Issue, "sk_live") {
		t.Fatalf("issue = %q, must never carry a secret value", st.Issue)
	}
}

// TestWorkloadStatusProgressDeadlineExceededIssue: a rollout that ran out of time with no blocking
// pod condition used to report not-available and nothing else. It now carries the deadline as its
// reason, and says plainly that Burrow found nothing more specific rather than guessing a cause.
func TestWorkloadStatusProgressDeadlineExceededIssue(t *testing.T) {
	dep := wedgedRolloutDeployment("img:2")
	dep.Status.Conditions = append(dep.Status.Conditions, appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
	})
	a := kube.New(fake.NewSimpleClientset(dep), ns)

	st, err := a.WorkloadStatus(context.Background(), "web")
	if err != nil {
		t.Fatalf("WorkloadStatus: %v", err)
	}
	if st.IssueReason != cp.ReasonProgressDeadlineExceeded {
		t.Errorf("issue reason = %q, want %q", st.IssueReason, cp.ReasonProgressDeadlineExceeded)
	}
	if !strings.Contains(st.Issue, "progress deadline") {
		t.Errorf("issue = %q, want it to explain the deadline", st.Issue)
	}
}

// TestListWorkloadsSurfacesCrashLoop is the list-path half of ADR-0074 §2's acceptance: the widened
// vocabulary has to reach `burrow app list` too, not only `burrow app status`, or the cluster-wide
// question still sends the user to kubectl.
func TestListWorkloadsSurfacesCrashLoop(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(wedgedRolloutDeployment("img:2"), crashLoopPod(1)), ns)

	apps, err := a.ListWorkloads(context.Background())
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("got %d apps, want 1: %+v", len(apps), apps)
	}
	if apps[0].Available {
		t.Fatalf("list reported a crash-looping app available: %+v", apps[0])
	}
	if apps[0].IssueReason != cp.ReasonCrashLoopBackOff {
		t.Errorf("issue reason = %q, want %q", apps[0].IssueReason, cp.ReasonCrashLoopBackOff)
	}
	if !strings.Contains(apps[0].Issue, "exited with code 1") {
		t.Errorf("issue = %q, want it to name the exit code", apps[0].Issue)
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

// TestLogsCarryTimestamps is the adapter half of #480: the pod-log request asks Kubernetes to stamp
// every line, and the instant comes back on the LogLine with the prefix stripped out of the message
// rather than left duplicated in the application's text.
func TestLogsCarryTimestamps(t *testing.T) {
	ctx := context.Background()
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "web-abc", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/name": "web"},
	}}
	client := fake.NewSimpleClientset(dep, pod)

	var gotOpts *corev1.PodLogOptions
	client.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		opts, ok := action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		if !ok {
			t.Errorf("log request value = %T, want *corev1.PodLogOptions", action.(k8stesting.GenericAction).GetValue())
			return false, nil, nil
		}
		gotOpts = opts
		body := "2026-08-04T02:49:46.5Z GET /auth/github/callback 500 1.888s\n" +
			"trailing frame with no prefix\n"
		return true, &runtime.Unknown{Raw: []byte(body)}, nil
	})

	lines, err := kube.New(client, ns).Logs(ctx, "web", cp.LogOptions{})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if gotOpts == nil || !gotOpts.Timestamps {
		t.Fatalf("pod log options = %+v, want Timestamps set", gotOpts)
	}

	want := time.Date(2026, 8, 4, 2, 49, 46, 500000000, time.UTC)
	if len(lines) != 2 {
		t.Fatalf("lines = %+v, want 2", lines)
	}
	if !lines[0].Timestamp.Equal(want) || lines[0].Message != "GET /auth/github/callback 500 1.888s" {
		t.Errorf("first line = %+v, want %v / message without the prefix", lines[0], want)
	}
	// The unstamped frame is a continuation: it keeps its raw text and inherits the instant above it.
	if !lines[1].Timestamp.Equal(want) || lines[1].Message != "trailing frame with no prefix" {
		t.Errorf("second line = %+v, want the inherited instant %v and the raw text", lines[1], want)
	}
}

// TestApplyNoPodMutatorLeavesDeploymentUnchanged guards the backward-compatible default of the
// ADR-0061 deploy-path extension point (WithPodMutator): with no mutator wired, the Deployment the
// adapter constructs is byte-for-byte what it was before the hook existed. ADR-0061 §3 makes that a
// test obligation rather than an aspiration, so the whole expected object is spelled out here — any
// accidental change to the deploy path's pod shape fails this test.
func TestApplyNoPodMutatorLeavesDeploymentUnchanged(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns)

	spec := cp.WorkloadSpec{
		App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 2,
		Env:         map[string]string{"B": "2", "A": "1"},
		Command:     []string{"server", "--port", "8080"},
		MetricsPort: 9090,
		ReleaseID:   "rel-1",
	}
	if err := a.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	got, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The fake clientset stamps its own resourceVersion on create; that is the tracker's, not the
	// adapter's, so it is cleared before comparing.
	got.ResourceVersion = ""

	labels := map[string]string{"app.kubernetes.io/name": "web", "app.kubernetes.io/managed-by": "burrow"}
	want := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32p(2),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/port":   "9090",
						"prometheus.io/path":   "/metrics",
						cp.ReleaseAnnotation:   "rel-1",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "web",
						Image:   "img:1",
						Command: []string{"server", "--port", "8080"},
						Env: []corev1.EnvVar{
							{Name: "A", Value: "1"},
							{Name: "B", Value: "2"},
						},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cp.AppSecretName("web")},
								Optional:             boolp(true),
							},
						}},
					}},
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Deployment built with no pod mutator differs from the pre-hook output (ADR-0061 §3)\n got: %#v\nwant: %#v", got, want)
	}
}

// TestApplyPodMutatorAppliedOnCreate asserts the ADR-0061 extension point is honored on the create
// path: a mutator standing in for an operator's cluster requirements — a toleration for a tainted
// node pool and a mandated runtime class — reaches the created Deployment.
func TestApplyPodMutatorAppliedOnCreate(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns).WithPodMutator(tolerationMutator("gpu", "kata"))

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertMutated(t, dep.Spec.Template.Spec, "gpu", "kata")
}

// TestApplyPodMutatorAppliedOnUpdate is the case that would otherwise regress silently: the hook
// must run on every path that writes the pod template, not only on create (ADR-0061 §2). A mutator
// applied only on create is dropped by the first rollout, leaving a long-running workload without
// the toleration or runtime class it was deployed with — a failure that shows up later, under a
// redeploy, as an unschedulable pod rather than as a missing hook.
func TestApplyPodMutatorAppliedOnUpdate(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := kube.New(client, ns).WithPodMutator(tolerationMutator("gpu", "kata"))

	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// The second apply is a rollout: it takes the update path, which rebuilds the pod template.
	if err := a.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Image: "img:2", Replicas: 3}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.Spec.Template.Spec.Containers[0].Image != "img:2" {
		t.Fatalf("image = %q, want img:2 (the rollout did not take the update path)", dep.Spec.Template.Spec.Containers[0].Image)
	}
	assertMutated(t, dep.Spec.Template.Spec, "gpu", "kata")
	// The mutator here is idempotent (it replaces rather than appends), so the rollout must not
	// have accumulated a second copy of the toleration.
	if n := len(dep.Spec.Template.Spec.Tolerations); n != 1 {
		t.Errorf("tolerations after a rollout = %d, want 1", n)
	}
}

// TestApplyPodMutatorSeesConstructedPodSpec asserts the hook runs over the FULLY-constructed pod
// spec, not an empty one: it must be able to read and adjust the containers, env, and command the
// engine built (ADR-0061 §1). A hook applied before construction could not, for example, add a
// volume mount to the app container.
func TestApplyPodMutatorSeesConstructedPodSpec(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	var sawContainers, sawEnv int
	var sawImage string
	a := kube.New(client, ns).WithPodMutator(func(pod *corev1.PodSpec) {
		sawContainers = len(pod.Containers)
		if len(pod.Containers) == 0 {
			return
		}
		sawImage = pod.Containers[0].Image
		sawEnv = len(pod.Containers[0].Env)
		// A mutator that adjusts the constructed container, which is only possible if it is there.
		pod.Containers[0].ImagePullPolicy = corev1.PullAlways
	})

	spec := cp.WorkloadSpec{
		App: "web", Image: "img:1", Replicas: 1,
		Env: map[string]string{"A": "1", "B": "2"},
	}
	if err := a.ApplyWorkload(ctx, spec); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	if sawContainers != 1 {
		t.Errorf("mutator saw %d containers, want 1 (the hook must run over the constructed pod spec)", sawContainers)
	}
	if sawImage != "img:1" {
		t.Errorf("mutator saw image %q, want img:1", sawImage)
	}
	if sawEnv != 2 {
		t.Errorf("mutator saw %d env vars, want 2", sawEnv)
	}
	dep, err := client.AppsV1().Deployments(ns).Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].ImagePullPolicy; got != corev1.PullAlways {
		t.Errorf("imagePullPolicy = %q, want Always (the mutator's edit to the built container)", got)
	}
}

// tolerationMutator stands in for an operator's cluster requirements: a toleration for a tainted
// node pool and a mandated runtime class. It is idempotent — it replaces the toleration slice
// rather than appending to it — as ADR-0061 requires of a hook that runs on every update.
func tolerationMutator(pool, runtimeClass string) func(*corev1.PodSpec) {
	return func(pod *corev1.PodSpec) {
		pod.Tolerations = []corev1.Toleration{{
			Key:      "dedicated",
			Operator: corev1.TolerationOpEqual,
			Value:    pool,
			Effect:   corev1.TaintEffectNoSchedule,
		}}
		rc := runtimeClass
		pod.RuntimeClassName = &rc
	}
}

func assertMutated(t *testing.T, pod corev1.PodSpec, pool, runtimeClass string) {
	t.Helper()
	if len(pod.Tolerations) == 0 {
		t.Fatalf("pod carries no tolerations; the mutator's toleration for the %q pool was dropped", pool)
	}
	if got := pod.Tolerations[0].Value; got != pool {
		t.Errorf("toleration value = %q, want %q", got, pool)
	}
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != runtimeClass {
		t.Errorf("runtimeClassName = %v, want %q", pod.RuntimeClassName, runtimeClass)
	}
}

func boolp(b bool) *bool { return &b }
