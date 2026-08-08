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
	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// stubClusterPostgresClientset substitutes the clientset the cluster-postgres subcommands act with,
// so nothing in these tests reaches a live cluster or reads an ambient kubeconfig.
func stubClusterPostgresClientset(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	orig := clusterPostgresClientset
	clusterPostgresClientset = func(string, string) (kubernetes.Interface, error) { return cs, nil }
	t.Cleanup(func() { clusterPostgresClientset = orig })
}

// recordAppliedURL substitutes the remote-manifest apply seam with a recorder, so a test asserts
// which upstream artifact is applied without fetching it.
func recordAppliedURL(t *testing.T) *string {
	t.Helper()
	var applied string
	orig := applyURLFn
	applyURLFn = func(_ context.Context, _, _, url string, _ bool, stdout, _ io.Writer) error {
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
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
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
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
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
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--context", testCluster}, &out, &errb); err != nil {
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
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
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

// stubPostgresPrerequisites substitutes the backup plugin's two detection seams, so a test can put
// the cluster in any of the plugin's three states without building discovery fixtures for two API
// groups.
func stubPostgresPrerequisites(t *testing.T, pluginReady, certManagerPresent bool) {
	t.Helper()
	origPlugin, origCerts := detectPgBackRestFn, detectCertManagerFn
	detectPgBackRestFn = func(context.Context, kubernetes.Interface) (controlplane.PgBackRestCapability, error) {
		return controlplane.PgBackRestCapability{Present: pluginReady, Ready: pluginReady, Pinned: kube.PgBackRestVersion}, nil
	}
	detectCertManagerFn = func(kubernetes.Interface) (controlplane.CertManagerCapability, error) {
		return controlplane.CertManagerCapability{Present: certManagerPresent}, nil
	}
	t.Cleanup(func() { detectPgBackRestFn, detectCertManagerFn = origPlugin, origCerts })
}

// TestClusterPostgresPlanLeadsWithComponents asserts the plan's first lines say WHAT is installed and
// what each component is for, and that neither manifest URL appears by default. A URL is the longest
// thing on the line, and led with, it pushes the version — the value a reader scans for — off the end
// of the terminal (issue #461).
func TestClusterPostgresPlanLeadsWithComponents(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(false))
	stubPostgresPrerequisites(t, false, true)
	recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	s := out.String()
	plan, _, _ := strings.Cut(s, "\nInstalling:")
	for _, want := range []string{
		"CloudNativePG " + kube.CNPGVersion,
		"runs and manages Postgres instances",
		"archives write-ahead logs to object storage",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
	for _, url := range []string{kube.CNPGManifestURL(kube.CNPGVersion), kube.PgBackRestManifestURL(kube.PgBackRestVersion)} {
		if strings.Contains(plan, url) {
			t.Errorf("plan leads with the manifest URL %q, which is what --verbose is for:\n%s", url, plan)
		}
	}
	if !strings.Contains(plan, "--verbose") {
		t.Errorf("the cluster-admin note must say where the manifests can be read, or the provenance is unreachable:\n%s", plan)
	}
}

// TestClusterPostgresPlanVerboseShowsManifests asserts --verbose puts each manifest URL on its own
// indented line beneath its component. Applying a remote manifest with cluster-admin is a trust
// decision and the artifact has to stay reachable; it just does not belong on the component's line.
func TestClusterPostgresPlanVerboseShowsManifests(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(false))
	stubPostgresPrerequisites(t, false, true)
	recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--verbose", "--context", testCluster}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install --verbose: %v\n%s", err, errb.String())
	}
	plan, _, _ := strings.Cut(out.String(), "\nInstalling:")
	for _, want := range []string{
		"\n      " + kube.CNPGManifestURL(kube.CNPGVersion) + "\n",
		"\n      " + kube.PgBackRestManifestURL(kube.PgBackRestVersion) + "\n",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("verbose plan missing the indented manifest line %q:\n%s", want, plan)
		}
	}
}

// TestClusterPostgresNamesTheBackupPluginBothWays asserts the backup component is described by what it
// DOES and still carries its product name. Dropping the name would hide a second component installed
// with cluster-admin from anyone auditing what landed on their cluster.
func TestClusterPostgresNamesTheBackupPluginBothWays(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(false))
	stubPostgresPrerequisites(t, false, true)
	recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	plan, _, _ := strings.Cut(out.String(), "\nInstalling:")
	for _, want := range []string{"Backup support", "archives write-ahead logs to object storage", "pgBackRest"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan must name the backup component by what it does AND by product (%q):\n%s", want, plan)
		}
	}
}

// TestClusterPostgresProgressTicksUnchanged pins the install-phase status lines. They were the one
// part of this output that already worked, and the reshaping around them must leave them alone.
func TestClusterPostgresProgressTicksUnchanged(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(false))
	stubPostgresPrerequisites(t, false, true)
	recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	for _, want := range []string{
		"  " + okGlyph + " CloudNativePG  installed " + kube.CNPGVersion + " (240 created)\n",
		"  " + okGlyph + " pgBackRest plugin  installed " + kube.PgBackRestVersion + " (240 created)\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("progress tick %q changed:\n%s", want, out.String())
		}
	}
}

// TestClusterPostgresOrderingReasonSitsWithTheCommands asserts the reason the two next commands are in
// that order is inside the next-steps block, not below it as a separate note. Printed apart it reads as
// an unrelated caveat, and somebody who skims the commands and stops has missed the only thing that
// makes their order matter.
func TestClusterPostgresOrderingReasonSitsWithTheCommands(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(false))
	stubPostgresPrerequisites(t, false, true)
	recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	s := out.String()
	_, next, ok := strings.Cut(s, "Next, in this order:")
	if !ok {
		t.Fatalf("the closing block must name the next steps as an ordered sequence:\n%s", s)
	}
	reason, _, ok := strings.Cut(next, "restoring a whole instance")
	if !ok {
		t.Fatalf("the unbuilt whole-instance restore must still be stated:\n%s", s)
	}
	if !strings.Contains(reason, "The order matters") || !strings.Contains(reason, "BEFORE it is installed") {
		t.Errorf("the ordering reason must sit inside the next-steps block:\n%s", next)
	}
	// The ordering reason is an explanation of the step above it, not a third warning competing with
	// the one fact a reader most needs to have read.
	if n := strings.Count(s, "Note:"); n > 2 {
		t.Errorf("the output carries %d advisory notes; they flatten each other:\n%s", n, s)
	}
}

// TestClusterPostgresInstallsTheBackupPluginOverARunningOperator asserts a cluster that ALREADY runs
// CloudNativePG still gets the backup plugin. The command used to return as soon as it found a running
// operator, so on such a cluster it reported success having installed nothing, and every instance made
// afterwards archived nowhere.
func TestClusterPostgresInstallsTheBackupPluginOverARunningOperator(t *testing.T) {
	cs := cnpgFakeClientset(true, cnpgOperatorFixture("ghcr.io/cloudnative-pg/cloudnative-pg:"+kube.CNPGVersion, 1))
	stubClusterPostgresClientset(t, cs)
	stubPostgresPrerequisites(t, false, true)
	applied := recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	if want := kube.PgBackRestManifestURL(kube.PgBackRestVersion); *applied != want {
		t.Errorf("applied %q, want the backup plugin %q to be installed over a running operator", *applied, want)
	}
	if !strings.Contains(out.String(), "already running "+kube.CNPGVersion) {
		t.Errorf("the running operator must still be left alone and named:\n%s", out.String())
	}
}

// TestClusterPostgresPlanReportsASkippedBackupPlugin asserts the plan says up front that the plugin
// will be skipped for a missing cert-manager. Announcing an install and then skipping it further down
// describes a run that did not happen.
func TestClusterPostgresPlanReportsASkippedBackupPlugin(t *testing.T) {
	stubClusterPostgresClientset(t, cnpgFakeClientset(false))
	stubPostgresPrerequisites(t, false, false)
	recordAppliedURL(t)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "postgres", "install", "--wait=false", "--context", testCluster}, &out, &errb); err != nil {
		t.Fatalf("cluster postgres install: %v\n%s", err, errb.String())
	}
	plan, _, _ := strings.Cut(out.String(), "\nInstalling:")
	if !strings.Contains(plan, "skipped: needs cert-manager") {
		t.Errorf("the plan must say the backup plugin is skipped, and why:\n%s", plan)
	}
	if !strings.Contains(plan, "burrow cluster ingress install") {
		t.Errorf("the plan must name the command that fixes it:\n%s", plan)
	}
}
