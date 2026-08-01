// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// stubClusterPostgresClientset substitutes the clientset the cluster-postgres subcommands act with,
// so nothing in these tests reaches a live cluster or reads an ambient kubeconfig.
func stubClusterPostgresClientset(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	orig := clusterPostgresClientset
	clusterPostgresClientset = func(string) (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { clusterPostgresClientset = orig })
}

// recordAppliedURL substitutes the remote-manifest apply seam with a recorder, so a test asserts
// which upstream artifact is applied without fetching it.
func recordAppliedURL(t *testing.T) *string {
	t.Helper()
	var applied string
	orig := applyURLFn
	applyURLFn = func(_ context.Context, _, url string, _ bool, stdout, _ io.Writer) error {
		applied = url
		_, _ = io.WriteString(stdout, "✓ Applied 240 resource(s): 240 created.\n")
		return nil
	}
	t.Cleanup(func() { applyURLFn = orig })
	return &applied
}

// cnpgOperatorFixture is a CloudNativePG operator Deployment carrying the label detection selects
// on, with the given image and ready replicas.
func cnpgOperatorFixture(image string, readyReplicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kube.CNPGControllerDeployment,
			Namespace: kube.CNPGNamespace,
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

// cnpgFakeClientset builds a fake cluster, optionally serving the CloudNativePG API group.
func cnpgFakeClientset(crdsServed bool, objs ...runtime.Object) *fake.Clientset {
	cs := fake.NewSimpleClientset(objs...)
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}
	if crdsServed {
		cs.Resources = append(cs.Resources, &metav1.APIResourceList{GroupVersion: "postgresql.cnpg.io/v1"})
	}
	return cs
}

// TestClusterPostgresInstallDryRun asserts dry-run prints the plan — the pinned release, the exact
// artifact, and the cluster-admin requirement — without contacting a cluster at all.
func TestClusterPostgresInstallDryRun(t *testing.T) {
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--dry-run"}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install --dry-run: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"dry run",
		kube.CNPGVersion,
		kube.CNPGManifestURL(kube.CNPGVersion),
		"cluster-admin",
		"the agent has no such access",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("dry-run plan missing %q:\n%s", want, s)
		}
	}
}

// TestClusterPostgresInstallApplies asserts a cluster with no operator gets the pinned release's own
// artifact applied, and that the cluster-admin requirement is stated on the real run too — not only
// in dry-run, where an operator may never look.
func TestClusterPostgresInstallApplies(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(false))
	applied := recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false"}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	if want := kube.CNPGManifestURL(kube.CNPGVersion); *applied != want {
		t.Errorf("applied %q, want the pinned release artifact %q", *applied, want)
	}
	s := out.String()
	for _, want := range []string{"installed " + kube.CNPGVersion, "240 created", "cluster-admin"} {
		if !strings.Contains(s, want) {
			t.Errorf("install output missing %q:\n%s", want, s)
		}
	}
}

// TestClusterPostgresInstallRepairsOrphanedCRDs asserts an install PROCEEDS when the CRDs are served
// but no controller runs. Keying the skip off the API group would leave that cluster accepting
// `Cluster` objects nothing reconciles, and the plan says which of the two states it found.
func TestClusterPostgresInstallRepairsOrphanedCRDs(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(true))
	applied := recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false"}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	if *applied == "" {
		t.Fatalf("install must re-apply over CRDs with no running controller, applied nothing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no controller is running") {
		t.Errorf("the plan must say the CRDs are orphaned:\n%s", out.String())
	}
}

// TestClusterPostgresInstallSkipsWhenRunning asserts a running operator is left alone and its release
// named. A skip that says only "already present" hides the one fact that decides whether the pinned
// placement translation matches what the cluster will validate against.
func TestClusterPostgresInstallSkipsWhenRunning(t *testing.T) {
	cs := cnpgFakeClientset(true, cnpgOperatorFixture("ghcr.io/cloudnative-pg/cloudnative-pg:1.22.0", 1))
	stubClusterPostgresClientset(t, cs)
	applied := recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install"}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	if *applied != "" {
		t.Errorf("install must not apply over a running operator, applied %q", *applied)
	}
	s := out.String()
	for _, want := range []string{"already running 1.22.0", "Burrow targets " + kube.CNPGVersion, "skip"} {
		if !strings.Contains(s, want) {
			t.Errorf("skip output missing %q:\n%s", want, s)
		}
	}
}

// TestClusterPostgresInstallIsHonestAboutTheAddon asserts the closing block points at the next step
// and does NOT imply more is built than is. WAL archiving, scheduled and on-demand base backups are
// now real, and they need an object-storage provider registered before the instance is installed;
// RESTORING a whole instance from them is not built. Describing unbuilt behavior as done is the one
// thing ADR-0009 forbids outright, and this is the output an operator reads immediately after
// installing the operator.
func TestClusterPostgresInstallIsHonestAboutTheAddon(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(false))
	recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false"}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	for _, want := range []string{"burrow addon install postgres", "object-storage provider", "is not built yet"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("install output must say %q, so the next step is named and the gap stays stated:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "--cnpg") {
		t.Errorf("install output still offers --cnpg; the add-on has one mechanism:\n%s", out.String())
	}
}

// TestCloudNativePGLine asserts `burrow cluster` says the three states apart: absent, CRDs without a
// controller, and running (naming the release, and the pin when they differ).
func TestCloudNativePGLine(t *testing.T) {
	for _, c := range []struct {
		name string
		cap  client.CloudNativePGCapability
		want string
	}{
		{"absent", client.CloudNativePGCapability{}, "not installed"},
		{"orphaned CRDs", client.CloudNativePGCapability{Present: true, Pinned: "1.30.0"}, "no controller running"},
		{"running the pin", client.CloudNativePGCapability{Present: true, Ready: true, Version: "1.30.0", Pinned: "1.30.0"}, "running 1.30.0"},
		{"running another release", client.CloudNativePGCapability{Present: true, Ready: true, Version: "1.22.0", Pinned: "1.30.0"}, "Burrow targets 1.30.0"},
		{"version unknown", client.CloudNativePGCapability{Present: true, Ready: true, Pinned: "1.30.0"}, "version unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := cloudNativePGLine(c.cap); !strings.Contains(got, c.want) {
				t.Errorf("cloudNativePGLine(%+v) = %q, want it to contain %q", c.cap, got, c.want)
			}
		})
	}
}
