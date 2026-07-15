// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/connect"
)

// TestRenderRegistryManifest asserts the standalone in-cluster registry manifest (ADR-0053 §5) renders
// Zot's PVC, its config with garbage collection, a Deployment on the pinned image, and a NodePort
// Service at the pinned port, all in the control-plane namespace.
func TestRenderRegistryManifest(t *testing.T) {
	out, err := renderRegistryManifest("burrow")
	if err != nil {
		t.Fatalf("renderRegistryManifest: %v", err)
	}
	for _, want := range []string{
		"name: burrow-registry",                       // the registry Deployment/Service/PVC/ConfigMap
		"namespace: burrow",                           // in the control-plane namespace
		"kind: PersistentVolumeClaim",                 // its persistent volume (ADR-0053 Consequences)
		"storage: 5Gi",                                // ... sized for accumulating build layers
		"image: ghcr.io/project-zot/zot-linux-amd64:", // the pinned Zot image
		`"gc": true`,                                  // garbage collection so layers do not fill disk
		`"deleteUntagged": true`,                      // ... including orphaned untagged manifests
		"type: NodePort",                              // reachable by the node's containerd at a pinned port
		"nodePort: 30500",                             // ... the pinned NodePort
	} {
		if !strings.Contains(out, want) {
			t.Errorf("registry manifest missing %q", want)
		}
	}
	// The registry manifest must NOT carry control-plane resources — it is applied standalone, separate
	// from `burrow install` (ADR-0054).
	for _, unwanted := range []string{"name: burrowd", "kind: Namespace"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the standalone registry manifest must not contain %q", unwanted)
		}
	}
}

// TestBuildRegistriesConfig asserts the k3s containerd registry config (ADR-0053 §5) mirrors the
// in-cluster registry reference — the exact host a build pushes to and a deploy pulls by — to the
// pinned NodePort on localhost, and marks it insecure (the in-cluster registry serves plain HTTP).
func TestBuildRegistriesConfig(t *testing.T) {
	got := buildRegistriesConfig("burrow")
	for _, want := range []string{
		`"burrow-registry.burrow.svc.cluster.local:5000"`, // the mirror host = connect.RegistryEndpoint
		"mirrors:",
		"endpoint:",
		"http://127.0.0.1:30500", // the pinned NodePort the node reaches it at
		"insecure_skip_verify: true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("registries.yaml missing %q:\n%s", want, got)
		}
	}
	// The mirror host must equal the reference burrowd defaults its push target to, or the node would
	// pull a reference it has no mirror for.
	if !strings.Contains(got, connect.RegistryEndpoint("burrow")) {
		t.Errorf("registries.yaml mirror host must equal connect.RegistryEndpoint:\n%s", got)
	}
}

// TestWriteRegistriesConfig asserts the real writer creates the k3s registries.yaml (and its parent
// directory) with the rendered mirror config.
func TestWriteRegistriesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rancher", "k3s", "registries.yaml")
	if err := writeRegistriesConfig("burrow", path); err != nil {
		t.Fatalf("writeRegistriesConfig: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written config: %v", err)
	}
	if !strings.Contains(string(b), "burrow-registry.burrow.svc.cluster.local:5000") {
		t.Errorf("written registries.yaml missing the mirror host:\n%s", b)
	}
}

// burrowdDeploymentFixture returns a fake burrowd Deployment with the base env the install manifests
// give it, so a test can assert `cluster registry install`/`uninstall` add or remove the build-registry
// env in place.
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

// stubClusterRegistryClientset substitutes the cluster-registry clientset seam with the given fake and
// restores it on cleanup.
func stubClusterRegistryClientset(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	orig := clusterRegistryClientset
	clusterRegistryClientset = func(string) (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { clusterRegistryClientset = orig })
}

// burrowdBuildRegistryEnv returns the BURROW_BUILD_REGISTRY value on the burrowd container, or "" when
// it is absent, so tests can assert the wiring.
func burrowdBuildRegistryEnv(t *testing.T, cs kubernetes.Interface, ns string) string {
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
			if e.Name == "BURROW_BUILD_REGISTRY" {
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
// registry's in-cluster address, its node address, and — via the mirror seam — that the k3s node's
// containerd is wired to it.
func TestClusterRegistryStatusPresent(t *testing.T) {
	stubClusterRegistryClientset(t, fake.NewSimpleClientset(registryDeploymentFixture(connect.DefaultNamespace)))
	origMirror := k3sMirrorConfiguredFn
	k3sMirrorConfiguredFn = func(string) bool { return true }
	t.Cleanup(func() { k3sMirrorConfiguredFn = origMirror })

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry: %v\n%s", err, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"installed",
		connect.RegistryEndpoint(connect.DefaultNamespace), // the in-cluster address
		"http://127.0.0.1:30500",                           // the node address
		"wired",                                            // the containerd mirror is present
	} {
		if !strings.Contains(s, want) {
			t.Errorf("status missing %q:\n%s", want, s)
		}
	}
}

// TestClusterRegistryInstallOnK3sNode asserts `burrow cluster registry install` on a k3s node applies
// the registry manifest, wires burrowd's default build push target env, and writes the k3s
// registries.yaml so the node's containerd can pull from it (ADR-0053 §5).
func TestClusterRegistryInstallOnK3sNode(t *testing.T) {
	cs := fake.NewSimpleClientset(burrowdDeploymentFixture(connect.DefaultNamespace))
	stubClusterRegistryClientset(t, cs)

	var appliedManifests string
	origApply := applyFn
	applyFn = func(_ context.Context, _, _, manifests string, _ bool, _, _ io.Writer) error {
		appliedManifests = manifests
		return nil
	}
	var gotNamespace, gotPath string
	origWrite := writeRegistriesConfigFn
	writeRegistriesConfigFn = func(namespace, path string) error {
		gotNamespace, gotPath = namespace, path
		return nil
	}
	origNode := k3sNodePresentFn
	k3sNodePresentFn = func() bool { return true }
	t.Cleanup(func() {
		applyFn = origApply
		writeRegistriesConfigFn = origWrite
		k3sNodePresentFn = origNode
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "install"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry install: %v\n%s", err, errb.String())
	}

	if !strings.Contains(appliedManifests, "name: burrow-registry") {
		t.Errorf("install must apply the in-cluster registry manifest, got:\n%s", appliedManifests)
	}
	if gotNamespace != connect.DefaultNamespace || gotPath != k3sRegistriesPath {
		t.Errorf("registries.yaml written for (%q, %q), want (%q, %q)", gotNamespace, gotPath, connect.DefaultNamespace, k3sRegistriesPath)
	}
	if got := burrowdBuildRegistryEnv(t, cs, connect.DefaultNamespace); got != connect.RegistryEndpoint(connect.DefaultNamespace) {
		t.Errorf("burrowd BURROW_BUILD_REGISTRY = %q, want the in-cluster registry endpoint %q", got, connect.RegistryEndpoint(connect.DefaultNamespace))
	}
}

// TestClusterRegistryInstallOffK3sNode asserts that off a k3s node install still applies the manifest
// and wires burrowd, but does not write a node registries.yaml — it prints a note about the node's
// container runtime instead.
func TestClusterRegistryInstallOffK3sNode(t *testing.T) {
	cs := fake.NewSimpleClientset(burrowdDeploymentFixture(connect.DefaultNamespace))
	stubClusterRegistryClientset(t, cs)

	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }
	wrote := false
	origWrite := writeRegistriesConfigFn
	writeRegistriesConfigFn = func(string, string) error { wrote = true; return nil }
	origNode := k3sNodePresentFn
	k3sNodePresentFn = func() bool { return false }
	t.Cleanup(func() {
		applyFn = origApply
		writeRegistriesConfigFn = origWrite
		k3sNodePresentFn = origNode
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "install"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry install: %v\n%s", err, errb.String())
	}
	if wrote {
		t.Error("off a k3s node, install must not write a node registries.yaml")
	}
	if !strings.Contains(out.String(), "not a k3s node") {
		t.Errorf("off a k3s node, install must note the node's container runtime was not configured:\n%s", out.String())
	}
	if got := burrowdBuildRegistryEnv(t, cs, connect.DefaultNamespace); got == "" {
		t.Error("install must still wire burrowd's default build push target off a k3s node")
	}
}

// TestClusterRegistryInstallWithoutBurrowd asserts install stops clearly when burrowd is not installed
// rather than leaving a half-wired registry.
func TestClusterRegistryInstallWithoutBurrowd(t *testing.T) {
	stubClusterRegistryClientset(t, fake.NewSimpleClientset())
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error { return nil }
	origNode := k3sNodePresentFn
	k3sNodePresentFn = func() bool { return false }
	t.Cleanup(func() {
		applyFn = origApply
		k3sNodePresentFn = origNode
	})

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"cluster", "registry", "install"}, &out, &errb)
	if err == nil {
		t.Fatal("install must fail when burrowd is not installed")
	}
	if !strings.Contains(err.Error(), "burrowd is not installed") {
		t.Errorf("error should name the missing burrowd, got: %v", err)
	}
}

// TestClusterRegistryUninstall asserts uninstall deletes the registry resources, unsets burrowd's
// default build push target, and removes the k3s containerd mirror on a k3s node.
func TestClusterRegistryUninstall(t *testing.T) {
	ns := connect.DefaultNamespace
	burrowd := burrowdDeploymentFixture(ns)
	burrowd.Spec.Template.Spec.Containers[0].Env = append(burrowd.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BURROW_BUILD_REGISTRY", Value: connect.RegistryEndpoint(ns)})
	cs := fake.NewSimpleClientset(
		burrowd,
		registryDeploymentFixture(ns),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "burrow-registry", Namespace: ns}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "burrow-registry-config", Namespace: ns}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "burrow-registry", Namespace: ns}},
	)
	stubClusterRegistryClientset(t, cs)

	origNode := k3sNodePresentFn
	k3sNodePresentFn = func() bool { return true }
	origMirror := k3sMirrorConfiguredFn
	k3sMirrorConfiguredFn = func(string) bool { return true }
	removedPath := ""
	origRemove := removeRegistriesConfigFn
	removeRegistriesConfigFn = func(path string) error { removedPath = path; return nil }
	t.Cleanup(func() {
		k3sNodePresentFn = origNode
		k3sMirrorConfiguredFn = origMirror
		removeRegistriesConfigFn = origRemove
	})

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "uninstall"}, &out, &errb); err != nil {
		t.Fatalf("cluster registry uninstall: %v\n%s", err, errb.String())
	}

	if _, err := cs.AppsV1().Deployments(ns).Get(context.Background(), "burrow-registry", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("uninstall must delete the registry Deployment, get err = %v", err)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), "burrow-registry", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("uninstall must delete the registry PVC, get err = %v", err)
	}
	if got := burrowdBuildRegistryEnv(t, cs, ns); got != "" {
		t.Errorf("uninstall must unset burrowd's build push target, still %q", got)
	}
	if removedPath != k3sRegistriesPath {
		t.Errorf("uninstall must remove the k3s registries.yaml (%q), removed %q", k3sRegistriesPath, removedPath)
	}
}

// TestClusterRegistryUninstallIdempotent asserts uninstall on a cluster with nothing to remove (no
// registry, no burrowd env) succeeds without error — every deletion tolerates already-gone resources.
func TestClusterRegistryUninstallIdempotent(t *testing.T) {
	cs := fake.NewSimpleClientset(burrowdDeploymentFixture(connect.DefaultNamespace))
	stubClusterRegistryClientset(t, cs)
	origNode := k3sNodePresentFn
	k3sNodePresentFn = func() bool { return false }
	t.Cleanup(func() { k3sNodePresentFn = origNode })

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "registry", "uninstall"}, &out, &errb); err != nil {
		t.Fatalf("uninstall must be idempotent, got: %v\n%s", err, errb.String())
	}
}
