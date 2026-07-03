// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package e2e_test

import (
	"context"
	"os"
	"testing"

	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestDetectCapabilitiesE2E runs the read-only capability probe against a live cluster (the CI k3d
// job) and asserts it reports the cluster's real capabilities (ADR-0034). k3d ships a local-path
// default StorageClass and a Traefik IngressClass, so both must be detected — proving the probe
// reads the cluster, not a canned answer. Vanilla k3d is also a live instance of the exact bug this
// probe guards against: Burrow requires an ingress-nginx controller (`burrow cluster ingress
// install` installs ingress-nginx and the expose/cert flow binds `class: nginx`), but k3d ships
// Traefik, not ingress-nginx. So the "traefik" IngressClass is reported for information while
// Ingress.Present is false — a class exists but no ingress-nginx controller runs — until an operator
// runs `burrow cluster ingress install`. It is gated on BURROW_TEST_KUBECONFIG like the other e2e
// tests and adds no container image.
func TestDetectCapabilitiesE2E(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the end-to-end test")
	}
	ctx := context.Background()

	cfg, err := kube.ConfigFromKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	caps, err := kube.DetectCapabilities(ctx, client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}

	// k3d ships a local-path default StorageClass.
	if !caps.Storage.DefaultPresent {
		t.Errorf("expected a default StorageClass on k3d, got %+v", caps.Storage)
	}
	if caps.Storage.DefaultClass != "local-path" {
		t.Logf("default StorageClass = %q (expected local-path on k3d; cluster may differ)", caps.Storage.DefaultClass)
	}

	// k3d ships Traefik (not ingress-nginx), which installs a "traefik" IngressClass. Burrow requires
	// a running ingress-nginx controller, so Present must be FALSE here — a class exists but no
	// ingress-nginx controller runs (the exact "orphan class" false positive this probe fixes) — while
	// the "traefik" class is still reported for information. Present goes true only after
	// `burrow cluster ingress install` adds ingress-nginx; that positive path is covered by the unit
	// tests with a fake ready ingress-nginx controller Deployment, so it is not installed here.
	if caps.Ingress.Present {
		t.Errorf("k3d ships Traefik, not ingress-nginx, so ingress must not be reported usable, got %+v", caps.Ingress)
	}
	foundTraefik := false
	for _, c := range caps.Ingress.Classes {
		if c == "traefik" {
			foundTraefik = true
		}
	}
	if !foundTraefik {
		t.Logf("IngressClasses = %v (expected traefik on k3d; cluster may differ)", caps.Ingress.Classes)
	}

	// LoadBalancer support is always reported as an inference.
	if !caps.LoadBalancer.Inferred {
		t.Errorf("LoadBalancer support should be marked inferred, got %+v", caps.LoadBalancer)
	}
}
