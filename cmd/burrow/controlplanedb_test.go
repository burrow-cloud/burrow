// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestValidateDatabase pins the flag's three answers. An unrecognized value is refused rather than
// resolved to either shape: which database an install ends up with is the last thing anybody should
// have to infer from a typo.
func TestValidateDatabase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", databaseCNPG},
		{databaseCNPG, databaseCNPG},
		{databasePlain, databasePlain},
	} {
		got, err := validateDatabase(tc.in)
		if err != nil {
			t.Fatalf("validateDatabase(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("validateDatabase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	_, err := validateDatabase("postgres")
	if err == nil {
		t.Fatal("an unknown --database should be refused")
	}
	for _, want := range []string{"cnpg", "plain"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q, got: %v", want, err)
		}
	}
}

// TestRenderManifestsDatabaseShapes asserts the two install shapes against each other: each renders
// its own database and NOT the other one's. Rendering both would leave an install with an empty
// database in front of the one holding its state.
func TestRenderManifestsDatabaseShapes(t *testing.T) {
	render := func(t *testing.T, database string) string {
		t.Helper()
		out, err := renderManifests(installOptions{
			Namespace: "burrow", AppNamespace: "apps", Image: "registry.example.com/burrowd:1",
			Token: "tok", DBPassword: "pw-456", Port: 8080, Database: database,
		})
		if err != nil {
			t.Fatalf("renderManifests(%q): %v", database, err)
		}
		return out
	}

	cnpg := render(t, databaseCNPG)
	for _, want := range []string{
		"apiVersion: postgresql.cnpg.io/v1",
		"kind: Cluster",
		"name: postgres",
		"instances: 1",
		"database: burrow",
		"owner: burrow",
		"name: burrowd-db-owner",
		"type: kubernetes.io/basic-auth",
		"selectorType: rw",
		// The URL is unchanged between the two shapes, which is what the managed Service named
		// `postgres` is for: burrowd's connection string does not know which shape it is talking to.
		"postgres://burrow:pw-456@postgres:5432/burrow",
	} {
		if !strings.Contains(cnpg, want) {
			t.Errorf("the cnpg manifests are missing %q", want)
		}
	}
	if strings.Contains(cnpg, "image: postgres:18") {
		t.Error("the cnpg manifests must not render the plain Deployment's postgres container")
	}
	if strings.Contains(cnpg, "name: postgres-data") {
		t.Error("the cnpg manifests must not render the plain Deployment's PersistentVolumeClaim: CloudNativePG provisions its own")
	}

	plain := render(t, databasePlain)
	for _, want := range []string{
		"image: postgres:18",
		"name: postgres-data",
		"postgres://burrow:pw-456@postgres:5432/burrow",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("the plain manifests are missing %q", want)
		}
	}
	if strings.Contains(plain, "apiVersion: postgresql.cnpg.io/v1") {
		t.Error("--database plain must render no CloudNativePG Cluster: it exists for a cluster that will not accept the CustomResourceDefinitions")
	}

	// The grant that lets burrowd report which shape it is running is rendered either way, so a
	// plain install is one manifest away from the report rather than one manifest and an RBAC change.
	for name, out := range map[string]string{"cnpg": cnpg, "plain": plain} {
		if !strings.Contains(out, "name: burrowd-controlplane-db") {
			t.Errorf("the %s manifests should grant burrowd the read of its own database", name)
		}
	}
}

// TestInstallDatabasePlanStatesTheCost covers ADR-0086 §5: the plan names the cluster-scoped
// CustomResourceDefinitions and the longer install before anything is applied, and the plain plan
// names what it gives up.
func TestInstallDatabasePlanStatesTheCost(t *testing.T) {
	var cnpg bytes.Buffer
	writeInstallDatabasePlan(&cnpg, false, databaseCNPG, cloudNativePGState{})
	for _, want := range []string{
		"CloudNativePG " + kube.CNPGVersion,
		"CustomResourceDefinitions",
		"cluster-admin",
		"longer",
		"--database plain",
	} {
		if !strings.Contains(cnpg.String(), want) {
			t.Errorf("the cnpg plan should state %q:\n%s", want, cnpg.String())
		}
	}

	var running bytes.Buffer
	writeInstallDatabasePlan(&running, false, databaseCNPG, cloudNativePGState{ready: true, version: kube.CNPGVersion})
	if !strings.Contains(running.String(), "already running") {
		t.Errorf("an operator that is already running should be reported as skipped:\n%s", running.String())
	}

	var plain bytes.Buffer
	writeInstallDatabasePlan(&plain, false, databasePlain, cloudNativePGState{})
	if !strings.Contains(plain.String(), "no backups") {
		t.Errorf("the plain plan should state that there are no backups:\n%s", plain.String())
	}
	if strings.Contains(plain.String(), "CustomResourceDefinitions") {
		t.Errorf("the plain plan should not claim to install CustomResourceDefinitions:\n%s", plain.String())
	}
}

// TestInstallDoesNotFallBackToPlain is the one this record exists for (ADR-0086 §2). A cluster that
// refuses the CustomResourceDefinitions fails the install and is told about the flag; it is NOT
// quietly handed the plain database, because that is a working install, a success message, and no
// backups, with nothing afterwards saying which of the two happened.
func TestInstallDoesNotFallBackToPlain(t *testing.T) {
	restoreDetect := detectCloudNativePGFn
	restoreApply := applyURLFn
	t.Cleanup(func() { detectCloudNativePGFn, applyURLFn = restoreDetect, restoreApply })

	detectCloudNativePGFn = func(context.Context, kubernetes.Interface) (controlplane.CloudNativePGCapability, error) {
		return controlplane.CloudNativePGCapability{}, nil
	}
	applied := 0
	applyURLFn = func(_ context.Context, _, _, _ string, _ bool, _, _ io.Writer) error {
		applied++
		return errors.New(`customresourcedefinitions.apiextensions.k8s.io is forbidden`)
	}

	var out bytes.Buffer
	a := installArgs{kubeContext: "do-nyc1", namespace: "burrow"}
	err := installControlPlaneDatabaseOperator(context.Background(), a, databaseCNPG, fake.NewSimpleClientset(), &out, io.Discard)
	if err == nil {
		t.Fatal("an install that cannot create the CustomResourceDefinitions must fail")
	}
	for _, want := range []string{"--database plain", "do-nyc1", "forbidden"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q, got: %v", want, err)
		}
	}
	if strings.Contains(out.String(), "installed the plain") {
		t.Error("the install must not report installing anything in CloudNativePG's place")
	}
	if applied != 1 {
		t.Errorf("the operator manifest should have been applied once, got %d", applied)
	}
}

// TestInstallPlainInstallsNoOperator: the flag exists for a cluster that will not have the
// operator, so reaching for it there would defeat the choice.
func TestInstallPlainInstallsNoOperator(t *testing.T) {
	restore := applyURLFn
	t.Cleanup(func() { applyURLFn = restore })
	applyURLFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error {
		t.Fatal("--database plain must apply no operator manifest")
		return nil
	}

	var out bytes.Buffer
	a := installArgs{kubeContext: "do-nyc1", namespace: "burrow"}
	if err := installControlPlaneDatabaseOperator(context.Background(), a, databasePlain, fake.NewSimpleClientset(), &out, io.Discard); err != nil {
		t.Fatalf("plain install: %v", err)
	}
	if !strings.Contains(out.String(), "plain Deployment") {
		t.Errorf("the plan should say which database this install will have:\n%s", out.String())
	}
}

// TestWaitForControlPlaneClusterStages covers the three failures that look identical from the
// outside and need three different fixes (ADR-0086 Consequences: "each needs a message that says
// which stage did").
func TestWaitForControlPlaneClusterStages(t *testing.T) {
	fast := clusterWait{grace: 30 * time.Millisecond, timeout: 200 * time.Millisecond, poll: 5 * time.Millisecond}

	t.Run("serving", func(t *testing.T) {
		ri := fakeClusters(t, cnpgCluster(map[string]any{"readyInstances": int64(1), "phase": "Cluster in healthy state"}))
		if err := waitForControlPlaneCluster(context.Background(), ri, "burrow", io.Discard, fast); err != nil {
			t.Fatalf("a Cluster with an instance serving should be ready: %v", err)
		}
	})

	t.Run("never created", func(t *testing.T) {
		ri := fakeClusters(t)
		err := waitForControlPlaneCluster(context.Background(), ri, "burrow", io.Discard, fast)
		if err == nil || !strings.Contains(err.Error(), "did not appear") {
			t.Fatalf("a Cluster that was never created should say so, got: %v", err)
		}
	})

	t.Run("nothing reconciling it", func(t *testing.T) {
		ri := fakeClusters(t, cnpgCluster(nil))
		err := waitForControlPlaneCluster(context.Background(), ri, "burrow", io.Discard, fast)
		if err == nil || !strings.Contains(err.Error(), "has not started reconciling") {
			t.Fatalf("a Cluster with no status should be reported as unreconciled, got: %v", err)
		}
		if !strings.Contains(err.Error(), kube.CNPGControllerDeployment) {
			t.Errorf("the message should point at the operator's controller, got: %v", err)
		}
	})

	t.Run("bootstrapping but not serving", func(t *testing.T) {
		ri := fakeClusters(t, cnpgCluster(map[string]any{
			"readyInstances": int64(0),
			"phase":          "Setting up primary",
			"conditions":     []any{map[string]any{"message": "no persistent volumes available"}},
		}))
		err := waitForControlPlaneCluster(context.Background(), ri, "burrow", io.Discard, fast)
		if err == nil || !strings.Contains(err.Error(), "did not start accepting connections") {
			t.Fatalf("a bootstrapping Cluster should be reported as not serving, got: %v", err)
		}
		for _, want := range []string{"Setting up primary", "no persistent volumes available"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message should carry the operator's own reason %q, got: %v", want, err)
			}
		}
	})
}

// TestUpgradePreservesTheDatabaseShape: an upgrade re-renders the database that is running, never
// the default. Rendering the default onto an install running the plain Deployment would stand an
// empty CloudNativePG cluster in front of every row the install has.
func TestUpgradePreservesTheDatabaseShape(t *testing.T) {
	ctx := context.Background()

	plain := existingInstall("burrow", "apps")
	if _, err := plain.AppsV1().Deployments("burrow").Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: "burrow"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the plain database: %v", err)
	}
	got, err := databaseOf(ctx, plain, "burrow")
	if err != nil {
		t.Fatalf("databaseOf: %v", err)
	}
	if got != databasePlain {
		t.Errorf("an install running the postgres Deployment is %q, got %q", databasePlain, got)
	}
	opts, err := upgradeOptions(ctx, plain, "burrow", "registry.example.com/burrowd:v2")
	if err != nil {
		t.Fatalf("upgradeOptions: %v", err)
	}
	manifests, err := renderManifests(opts)
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	if strings.Contains(manifests, "apiVersion: postgresql.cnpg.io/v1") {
		t.Error("upgrading an install running the plain database must not write a CloudNativePG Cluster beside it")
	}

	cnpg := existingInstall("burrow", "apps")
	got, err = databaseOf(ctx, cnpg, "burrow")
	if err != nil {
		t.Fatalf("databaseOf: %v", err)
	}
	if got != databaseCNPG {
		t.Errorf("an install with no postgres Deployment is %q, got %q", databaseCNPG, got)
	}
}

// cnpgCluster builds the control-plane database's `Cluster` with the given status, or none at all
// when status is nil — the state that means the operator has not looked at it.
func cnpgCluster(status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": controlPlaneClusterName, "namespace": "burrow"},
		"spec":       map[string]any{"instances": int64(1)},
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

// fakeClusters returns the `Cluster` resource interface over a fake dynamic client holding objs.
func fakeClusters(t *testing.T, objs ...*unstructured.Unstructured) dynamic.ResourceInterface {
	t.Helper()
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{cnpgClusterGVR: "ClusterList"}
	items := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		items = append(items, o)
	}
	c := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, items...)
	return c.Resource(cnpgClusterGVR).Namespace("burrow")
}

// TestControlPlaneDatabaseLine covers what `burrow cluster` says about the database, which is where
// the answer lives once the install output has scrolled away (ADR-0086 §2).
func TestControlPlaneDatabaseLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   controlplane.ControlPlaneDatabaseCapability
		want string
	}{
		{"plain", controlplane.ControlPlaneDatabaseCapability{Kind: controlplane.ControlPlaneDatabasePlain, Ready: true}, "no backups"},
		{"cnpg unbacked", controlplane.ControlPlaneDatabaseCapability{Kind: controlplane.ControlPlaneDatabaseCloudNativePG, Ready: true}, "not archiving anywhere"},
		{"cnpg archiving", controlplane.ControlPlaneDatabaseCapability{Kind: controlplane.ControlPlaneDatabaseCloudNativePG, Ready: true, BackedUp: true}, "archiving to object storage"},
		{"cnpg starting", controlplane.ControlPlaneDatabaseCapability{Kind: controlplane.ControlPlaneDatabaseCloudNativePG}, "no instance serving yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := controlPlaneDatabaseLine(toClientCaps(controlplane.ClusterCapabilities{ControlPlaneDatabase: tc.in}).ControlPlaneDatabase)
			if !strings.Contains(got, tc.want) {
				t.Errorf("line = %q, want it to contain %q", got, tc.want)
			}
		})
	}

	// A control plane that reports nothing prints nothing: inventing an answer here would be a guess
	// about the one fact this line exists to state.
	if got := controlPlaneDatabaseLine(toClientCaps(controlplane.ClusterCapabilities{}).ControlPlaneDatabase); got != "" {
		t.Errorf("an unknown database should print no line, got %q", got)
	}
}

// TestInstallDefaultsToCloudNativePG pins the default at the flag, which is where somebody reading
// `burrow cluster install -h` learns which database they are about to get.
func TestInstallDefaultsToCloudNativePG(t *testing.T) {
	cmd := newInstallCmd()
	f := cmd.Flags().Lookup("database")
	if f == nil {
		t.Fatal("install should carry a --database flag")
	}
	if f.DefValue != databaseCNPG {
		t.Errorf("--database defaults to %q, want %q", f.DefValue, databaseCNPG)
	}
}

// stubCloudNativePGInstalled reports the operator as already running for the duration of a test.
//
// It exists because installing the control plane now installs CloudNativePG first (ADR-0086 §1): a
// test that drives `cluster install` or `cluster bootstrap` end to end through fakes would
// otherwise fetch the operator's release manifest over the network and then wait for an API group
// no fake serves. Tests about those flows want the operator to be a settled fact, which on a real
// re-run of install it usually is; the install path's own behaviour is covered above.
func stubCloudNativePGInstalled(t *testing.T) {
	t.Helper()
	orig := detectCloudNativePGFn
	detectCloudNativePGFn = func(context.Context, kubernetes.Interface) (controlplane.CloudNativePGCapability, error) {
		return controlplane.CloudNativePGCapability{
			Present: true, Ready: true, Version: kube.CNPGVersion, Pinned: kube.CNPGVersion,
		}, nil
	}
	t.Cleanup(func() { detectCloudNativePGFn = orig })
}

// TestBootstrapCarriesTheDatabaseChoice: `burrow cluster bootstrap` installs the control plane
// through the same path, so the choice has to be reachable there too. A single VPS is where the
// trade is sharpest — the operator is another always-on pod on a small box — and an operator who
// wants the plain database there should not have to skip the bootstrap to get it.
func TestBootstrapCarriesTheDatabaseChoice(t *testing.T) {
	f := newBootstrapCmd().Flags().Lookup("database")
	if f == nil {
		t.Fatal("cluster bootstrap should carry a --database flag")
	}
	if f.DefValue != databaseCNPG {
		t.Errorf("--database defaults to %q, want %q", f.DefValue, databaseCNPG)
	}
}
