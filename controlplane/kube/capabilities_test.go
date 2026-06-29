// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"testing"

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

func TestDetectCapabilitiesFullCluster(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		ingressClass("nginx"),
		storageClass("do-block-storage", true),
		storageClass("legacy", false),
		node("node-1", "digitalocean://12345"),
	)
	// cert-manager is detected via API-group discovery (no RBAC); seed its group.
	client.Resources = []*metav1.APIResourceList{
		{GroupVersion: "cert-manager.io/v1"},
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
	// A known cloud → LoadBalancer support is inferred.
	if !caps.LoadBalancer.Supported || !caps.LoadBalancer.Inferred {
		t.Errorf("load balancer = %+v, want supported+inferred on a known cloud", caps.LoadBalancer)
	}
	if !caps.CertManager.Present {
		t.Errorf("cert-manager = %+v, want present (cert-manager.io group served)", caps.CertManager)
	}
	// DNS is filled by the engine from the registry, not the cluster probe.
	if caps.DNS.Configured {
		t.Errorf("DNS should be unset by the cluster probe, got %+v", caps.DNS)
	}
}

func TestDetectCapabilitiesBareMetal(t *testing.T) {
	ctx := context.Background()
	// No IngressClass, no default StorageClass, a non-cloud providerID, no cert-manager group.
	client := fake.NewSimpleClientset(
		storageClass("local-path", false),
		node("node-1", "k3s://k3d-burrow-server-0"),
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
		t.Errorf("provider should be unknown for a k3s node, got %+v", caps.Provider)
	}
	// Not a known cloud → NodePort only.
	if caps.LoadBalancer.Supported {
		t.Errorf("load balancer should be unsupported on bare-metal, got %+v", caps.LoadBalancer)
	}
	if !caps.LoadBalancer.Inferred {
		t.Errorf("load balancer support is always an inference, got %+v", caps.LoadBalancer)
	}
	if caps.CertManager.Present {
		t.Errorf("cert-manager should be absent, got %+v", caps.CertManager)
	}
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
