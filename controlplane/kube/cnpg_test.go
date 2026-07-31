// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// cnpgOperatorDeployment builds a CloudNativePG operator Deployment carrying the label the release
// manifest and the Helm chart both set, with the given operator image and ready replicas.
func cnpgOperatorDeployment(namespace, image string, readyReplicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cnpg-controller-manager",
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "cloudnative-pg"},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: image}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas},
	}
}

// cnpgGroupServed is the discovery response of a cluster whose CloudNativePG CRDs are installed.
func cnpgGroupServed() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{{GroupVersion: "postgresql.cnpg.io/v1"}, {GroupVersion: "apps/v1"}}
}

// TestDetectCloudNativePGAbsent asserts a cluster with neither CRDs nor a controller reports the
// operator absent, and still names the release Burrow targets — "what would be installed" is an
// answer the probe can always give, since it is a constant rather than a cluster read.
func TestDetectCloudNativePGAbsent(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}

	got, err := kube.DetectCloudNativePG(context.Background(), client)
	if err != nil {
		t.Fatalf("DetectCloudNativePG: %v", err)
	}
	if got.Present || got.Ready || got.Version != "" {
		t.Errorf("cloudnative-pg = %+v, want absent with no version", got)
	}
	if got.Pinned != kube.CNPGVersion {
		t.Errorf("pinned = %q, want the pinned release %q even on a cluster with no operator", got.Pinned, kube.CNPGVersion)
	}
}

// TestDetectCloudNativePGOrphanedCRDs asserts the case the whole Present/Ready split exists for: the
// CRDs are served but no controller runs. A `Cluster` written against that cluster is ACCEPTED and
// then reconciled by nothing, so reporting it as installed would make the failure silent — the same
// mistake an orphan IngressClass produced on the ingress path.
func TestDetectCloudNativePGOrphanedCRDs(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.Resources = cnpgGroupServed()

	got, err := kube.DetectCloudNativePG(context.Background(), client)
	if err != nil {
		t.Fatalf("DetectCloudNativePG: %v", err)
	}
	if !got.Present {
		t.Errorf("present = false, want true: the postgresql.cnpg.io group is served")
	}
	if got.Ready {
		t.Errorf("ready = true with no controller Deployment; CRDs outlive the operator that installed them")
	}
}

// TestDetectCloudNativePGRunning asserts a running operator is reported ready with its release, read
// from the operator container's image tag, wherever its namespace — a Helm release does not put it
// in cnpg-system.
func TestDetectCloudNativePGRunning(t *testing.T) {
	client := fake.NewSimpleClientset(
		cnpgOperatorDeployment("postgres-operator", "ghcr.io/cloudnative-pg/cloudnative-pg:"+kube.CNPGVersion, 1),
	)
	client.Resources = cnpgGroupServed()

	got, err := kube.DetectCloudNativePG(context.Background(), client)
	if err != nil {
		t.Fatalf("DetectCloudNativePG: %v", err)
	}
	if !got.Present || !got.Ready {
		t.Errorf("cloudnative-pg = %+v, want present and ready", got)
	}
	if got.Version != kube.CNPGVersion {
		t.Errorf("version = %q, want %q read from the operator image tag", got.Version, kube.CNPGVersion)
	}
}

// TestDetectCloudNativePGVersionIsReportedNotAsserted asserts an operator on a release OTHER than the
// pin is reported as what it is, beside the pin. The placement translation is a claim about a
// specific release's schema (ADR-0077 §3), so the mismatch has to be visible rather than corrected
// or hidden.
func TestDetectCloudNativePGVersionIsReportedNotAsserted(t *testing.T) {
	client := fake.NewSimpleClientset(
		cnpgOperatorDeployment("cnpg-system", "ghcr.io/cloudnative-pg/cloudnative-pg:1.22.0", 1),
	)
	client.Resources = cnpgGroupServed()

	got, err := kube.DetectCloudNativePG(context.Background(), client)
	if err != nil {
		t.Fatalf("DetectCloudNativePG: %v", err)
	}
	if got.Version != "1.22.0" || got.Pinned != kube.CNPGVersion {
		t.Errorf("cloudnative-pg = %+v, want the installed 1.22.0 reported beside the pinned %s", got, kube.CNPGVersion)
	}
}

// TestDetectCloudNativePGUnknownVersion asserts an operator Burrow cannot read a release off — a
// digest-pinned image, or a Deployment with no operator container — reports ready with an EMPTY
// version rather than a guessed one.
func TestDetectCloudNativePGUnknownVersion(t *testing.T) {
	for _, c := range []struct {
		name  string
		image string
		cname string
	}{
		{"digest-pinned", "ghcr.io/cloudnative-pg/cloudnative-pg@sha256:" + strings.Repeat("a", 64), "manager"},
		{"no operator container", "ghcr.io/example/sidecar:1.0.0", "sidecar"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := cnpgOperatorDeployment("cnpg-system", c.image, 1)
			d.Spec.Template.Spec.Containers[0].Name = c.cname
			client := fake.NewSimpleClientset(d)
			client.Resources = cnpgGroupServed()

			got, err := kube.DetectCloudNativePG(context.Background(), client)
			if err != nil {
				t.Fatalf("DetectCloudNativePG: %v", err)
			}
			if !got.Ready {
				t.Errorf("ready = false, want true: the controller is running whether or not its release is legible")
			}
			if got.Version != "" {
				t.Errorf("version = %q, want empty rather than a guess", got.Version)
			}
		})
	}
}

// TestDetectCapabilitiesCarriesCloudNativePG asserts the operator reaches the capability survey the
// agent and `burrow cluster` read, rather than only the standalone detector.
func TestDetectCapabilitiesCarriesCloudNativePG(t *testing.T) {
	client := fake.NewSimpleClientset(
		cnpgOperatorDeployment("cnpg-system", "ghcr.io/cloudnative-pg/cloudnative-pg:"+kube.CNPGVersion, 1),
		node("node-1", ""),
	)
	client.Resources = cnpgGroupServed()

	caps, err := kube.DetectCapabilities(context.Background(), client)
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	if !caps.CloudNativePG.Present || !caps.CloudNativePG.Ready || caps.CloudNativePG.Version != kube.CNPGVersion {
		t.Errorf("caps.CloudNativePG = %+v, want present, ready and versioned", caps.CloudNativePG)
	}
}

// TestCNPGManifestURLNamesThePin asserts the artifact an install applies is the pinned release's own
// — the same artifact the placement schema is recorded from, so the schema Burrow validates a
// `Cluster` against is the schema the cluster ends up holding.
func TestCNPGManifestURLNamesThePin(t *testing.T) {
	url := kube.CNPGManifestURL(kube.CNPGVersion)
	if !strings.Contains(url, "v"+kube.CNPGVersion+"/") || !strings.HasSuffix(url, "cnpg-"+kube.CNPGVersion+".yaml") {
		t.Errorf("CNPGManifestURL(%s) = %q, want the release artifact of that exact version", kube.CNPGVersion, url)
	}
}
