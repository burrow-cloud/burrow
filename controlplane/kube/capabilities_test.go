// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane/kube"
)

func ingressClass(name string) *networkingv1.IngressClass {
	return &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// ingressControllerDeployment builds an ingress-nginx controller Deployment carrying the standard
// recommended labels the prober selects on, with readyReplicas set — the signal that a controller
// is actually running (not just that an orphan IngressClass lingers).
func ingressControllerDeployment(namespace string, readyReplicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ingress-nginx-controller",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "ingress-nginx",
				"app.kubernetes.io/component": "controller",
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas},
	}
}

func storageClass(name string, isDefault bool) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if isDefault {
		sc.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	}
	return sc
}

func node(name, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

// metalLBController builds a MetalLB controller Deployment carrying the given labels, so a test can
// seed either the Helm-chart or the static-manifest label scheme the prober matches.
func metalLBController(namespace string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: namespace, Labels: labels},
	}
}

func TestDetectCapabilitiesFullCluster(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		ingressClass("nginx"),
		ingressControllerDeployment("ingress-nginx", 1),
		storageClass("do-block-storage", true),
		storageClass("legacy", false),
		node("node-1", "digitalocean://12345"),
	)
	// cert-manager and metrics-server are detected via API-group discovery (no RBAC); seed their
	// groups.
	client.Resources = []*metav1.APIResourceList{
		{GroupVersion: "cert-manager.io/v1"},
		{GroupVersion: "metrics.k8s.io/v1beta1"},
		{GroupVersion: "apps/v1"},
	}

	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}

	if !caps.Ingress.Present || len(caps.Ingress.Classes) != 1 || caps.Ingress.Classes[0] != "nginx" {
		t.Errorf("ingress = %+v, want present with class nginx", caps.Ingress)
	}
	if !caps.Storage.DefaultPresent || caps.Storage.DefaultClass != "do-block-storage" {
		t.Errorf("storage default = %+v, want do-block-storage", caps.Storage)
	}
	if len(caps.Storage.Classes) != 2 {
		t.Errorf("storage classes = %v, want both classes listed", caps.Storage.Classes)
	}
	if caps.Provider.Cloud != "digitalocean" || caps.Provider.Name != "DigitalOcean" {
		t.Errorf("provider = %+v, want digitalocean/DigitalOcean", caps.Provider)
	}
	// A known cloud → LoadBalancer support is inferred, and the LB provider names the cloud.
	if !caps.LoadBalancer.Supported || !caps.LoadBalancer.Inferred || caps.LoadBalancer.Provider != "digitalocean" {
		t.Errorf("load balancer = %+v, want supported+inferred with provider digitalocean on a known cloud", caps.LoadBalancer)
	}
	if !caps.CertManager.Present {
		t.Errorf("cert-manager = %+v, want present (cert-manager.io group served)", caps.CertManager)
	}
	if !caps.MetricsServer.Present {
		t.Errorf("metrics-server = %+v, want present (metrics.k8s.io group served)", caps.MetricsServer)
	}
	// DNS is filled by the engine from the registry, not the cluster probe.
	if caps.DNS.Configured {
		t.Errorf("DNS should be unset by the cluster probe, got %+v", caps.DNS)
	}
}

func TestDetectCapabilitiesBareMetal(t *testing.T) {
	ctx := context.Background()
	// A truly bare cluster: no IngressClass, no default StorageClass, an unrecognized/empty
	// providerID (no cloud, no k3s), no MetalLB, no cert-manager group.
	client := fake.NewSimpleClientset(
		storageClass("local-path", false),
		node("node-1", ""),
	)
	client.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}

	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}

	if caps.Ingress.Present {
		t.Errorf("ingress should be absent with no IngressClass, got %+v", caps.Ingress)
	}
	if caps.Storage.DefaultPresent {
		t.Errorf("storage default should be absent (none annotated), got %+v", caps.Storage)
	}
	if caps.Provider.Cloud != "" {
		t.Errorf("provider should be unknown on a bare cluster, got %+v", caps.Provider)
	}
	// No LoadBalancer provider at all → unsupported.
	if caps.LoadBalancer.Supported {
		t.Errorf("load balancer should be unsupported with no provider, got %+v", caps.LoadBalancer)
	}
	if caps.LoadBalancer.Provider != "" {
		t.Errorf("load balancer provider should be empty when none is detected, got %+v", caps.LoadBalancer)
	}
	if !caps.LoadBalancer.Inferred {
		t.Errorf("load balancer support is always an inference, got %+v", caps.LoadBalancer)
	}
	if caps.CertManager.Present {
		t.Errorf("cert-manager should be absent, got %+v", caps.CertManager)
	}
	if caps.MetricsServer.Present {
		t.Errorf("metrics-server should be absent (metrics.k8s.io not served), got %+v", caps.MetricsServer)
	}
}

// TestDetectLoadBalancerServiceLB proves the gap fix for k3s: a k3s node (its "k3s" providerID
// scheme) means k3s's built-in servicelb runs, so a LoadBalancer Service gets a real external IP even
// with no cloud provider (proven on k3d by the e2e). Detection must report Supported=true with
// provider "servicelb" — not the pre-fix Supported=false.
func TestDetectLoadBalancerServiceLB(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		storageClass("local-path", false),
		node("node-1", "k3s://k3d-burrow-server-0"),
	)
	client.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}

	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	// The cloud provider is still unknown (k3s is not a billable cloud)...
	if caps.Provider.Cloud != "" {
		t.Errorf("provider should be unknown for a k3s node, got %+v", caps.Provider)
	}
	// ...but servicelb services LoadBalancers, so support is detected and named.
	if !caps.LoadBalancer.Supported || !caps.LoadBalancer.Inferred || caps.LoadBalancer.Provider != "servicelb" {
		t.Errorf("load balancer = %+v, want supported+inferred with provider servicelb on k3s", caps.LoadBalancer)
	}
}

// TestDetectLoadBalancerMetalLB proves detection on a non-cloud cluster running MetalLB: its
// controller Deployment (either the Helm-chart or the static-manifest label scheme) means MetalLB
// services LoadBalancers, so Supported=true with provider "metallb".
func TestDetectLoadBalancerMetalLB(t *testing.T) {
	ctx := context.Background()

	t.Run("helm chart labels", func(t *testing.T) {
		client := fake.NewSimpleClientset(
			node("node-1", ""), // no cloud, no k3s
			metalLBController("metallb-system", map[string]string{
				"app.kubernetes.io/name":      "metallb",
				"app.kubernetes.io/component": "controller",
			}),
		)
		caps, err := kube.DetectCapabilities(ctx, client)
		if err != nil {
			t.Fatalf("DetectCapabilities: %v", err)
		}
		if !caps.LoadBalancer.Supported || !caps.LoadBalancer.Inferred || caps.LoadBalancer.Provider != "metallb" {
			t.Errorf("load balancer = %+v, want supported+inferred with provider metallb", caps.LoadBalancer)
		}
	})

	t.Run("static manifest labels", func(t *testing.T) {
		client := fake.NewSimpleClientset(
			node("node-1", ""),
			metalLBController("metallb-system", map[string]string{
				"app":       "metallb",
				"component": "controller",
			}),
		)
		caps, err := kube.DetectCapabilities(ctx, client)
		if err != nil {
			t.Fatalf("DetectCapabilities: %v", err)
		}
		if !caps.LoadBalancer.Supported || caps.LoadBalancer.Provider != "metallb" {
			t.Errorf("load balancer = %+v, want supported with provider metallb", caps.LoadBalancer)
		}
	})
}

func TestDetectCapabilitiesBetaDefaultAnnotation(t *testing.T) {
	ctx := context.Background()
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
		Name:        "standard",
		Annotations: map[string]string{"storageclass.beta.kubernetes.io/is-default-class": "true"},
	}}
	client := fake.NewSimpleClientset(sc)

	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	if !caps.Storage.DefaultPresent || caps.Storage.DefaultClass != "standard" {
		t.Errorf("storage default = %+v, want the beta-annotated default detected", caps.Storage)
	}
}

func TestDetectCapabilitiesEmptyCluster(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	if caps.Ingress.Present || caps.Storage.DefaultPresent || caps.CertManager.Present || caps.Provider.Cloud != "" {
		t.Errorf("an empty cluster should report no capabilities, got %+v", caps)
	}
}

// TestDetectIngressOrphanClassNoController is the exact bug scenario: an IngressClass survives after
// its controller (and namespace) were deleted. The class alone must NOT report ingress as usable —
// Present is false — while the class name is still reported so an Ingress can name it.
func TestDetectIngressOrphanClassNoController(t *testing.T) {
	ctx := context.Background()
	// The "nginx" IngressClass lingers, but there is no controller Deployment.
	client := fake.NewSimpleClientset(ingressClass("nginx"))

	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	if caps.Ingress.Present {
		t.Errorf("an orphan IngressClass with no controller must not report ingress usable, got %+v", caps.Ingress)
	}
	if len(caps.Ingress.Classes) != 1 || caps.Ingress.Classes[0] != "nginx" {
		t.Errorf("the orphan class name should still be reported, got %+v", caps.Ingress)
	}
}

// TestDetectIngressControllerReadiness checks Present tracks a running controller, not the class:
// a controller Deployment with zero ready replicas is not usable, and one with a ready replica is.
func TestDetectIngressControllerReadiness(t *testing.T) {
	ctx := context.Background()

	t.Run("controller present but not ready", func(t *testing.T) {
		client := fake.NewSimpleClientset(
			ingressClass("nginx"),
			ingressControllerDeployment("ingress-nginx", 0),
		)
		caps, err := kube.DetectCapabilities(ctx, client)
		if err != nil {
			t.Fatalf("DetectCapabilities: %v", err)
		}
		if caps.Ingress.Present {
			t.Errorf("a controller with no ready replicas must not report ingress usable, got %+v", caps.Ingress)
		}
	})

	t.Run("controller ready", func(t *testing.T) {
		client := fake.NewSimpleClientset(
			ingressClass("nginx"),
			ingressControllerDeployment("ingress-nginx", 1),
		)
		caps, err := kube.DetectCapabilities(ctx, client)
		if err != nil {
			t.Fatalf("DetectCapabilities: %v", err)
		}
		if !caps.Ingress.Present {
			t.Errorf("a ready controller must report ingress usable, got %+v", caps.Ingress)
		}
		if len(caps.Ingress.Classes) != 1 || caps.Ingress.Classes[0] != "nginx" {
			t.Errorf("ingress classes = %+v, want [nginx]", caps.Ingress.Classes)
		}
	})

	t.Run("controller ready in a non-conventional namespace", func(t *testing.T) {
		// The controller Deployment is found wherever it lives, via its labels — not by namespace.
		client := fake.NewSimpleClientset(
			ingressClass("nginx"),
			ingressControllerDeployment("platform", 2),
		)
		caps, err := kube.DetectCapabilities(ctx, client)
		if err != nil {
			t.Fatalf("DetectCapabilities: %v", err)
		}
		if !caps.Ingress.Present {
			t.Errorf("a ready controller in any namespace must report ingress usable, got %+v", caps.Ingress)
		}
	})
}
