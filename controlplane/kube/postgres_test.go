// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

const addonNS = "burrow-addons"

// TestMetricsCollectorDiscoversAppAndAddonNamespaces asserts vmagent's scrape config discovers pods
// in both the app namespace and the add-on namespace, so the always-on Postgres exporter is scraped
// whichever add-on is installed first (ADR-0051).
func TestMetricsCollectorDiscoversAppAndAddonNamespaces(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: vmagentServiceAccount, Namespace: addonNS},
	})
	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonMetrics)
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("collector config: %v", err)
	}
	scrape := cm.Data["scrape.yml"]
	if !strings.Contains(scrape, "names: [apps, "+addonNS+"]") {
		t.Errorf("scrape config namespace list does not cover both app and add-on namespaces:\n%s", scrape)
	}
}

// TestMetricsCollectorDedupesWhenNamespacesEqual asserts a single-namespace install lists that one
// namespace once (no double-scrape) — the dedupe branch of scrapeNamespaces (ADR-0051).
func TestMetricsCollectorDedupesWhenNamespacesEqual(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: vmagentServiceAccount, Namespace: addonNS},
	})
	a := New(client, addonNS).WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonMetrics)
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	cm, _ := client.CoreV1().ConfigMaps(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{})
	scrape := cm.Data["scrape.yml"]
	if !strings.Contains(scrape, "names: ["+addonNS+"]") {
		t.Errorf("scrape config should list the single namespace once:\n%s", scrape)
	}
	if strings.Count(scrape, addonNS) != 1 {
		t.Errorf("namespace %q appears %d times, want exactly once (deduped):\n%s", addonNS, strings.Count(scrape, addonNS), scrape)
	}
}

// TestProvisionerRejectsBadIdentifiers asserts every provisioning method rejects SQL-injection-shaped
// and malformed names as ErrInvalid BEFORE anything is written (ADR-0031).
func TestProvisionerRejectsBadIdentifiers(t *testing.T) {
	ctx := context.Background()
	// Nothing provisioned anywhere: a rejection must come from validation, before any I/O.
	p, _, _ := provisionerFor(t, addonNS)

	bad := []string{"a; DROP DATABASE x", "App", "1x", "", "-web", "web name", "web\"; --", "WEB", "web_db", "web;"}
	for _, name := range bad {
		if _, err := p.EnsureAppDatabase(ctx, name, controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("EnsureAppDatabase(%q) err = %v, want ErrInvalid", name, err)
		}
		if err := p.RevokeAppDatabase(ctx, name, controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RevokeAppDatabase(%q) err = %v, want ErrInvalid", name, err)
		}
		if err := p.DropAppDatabase(ctx, name, controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("DropAppDatabase(%q) err = %v, want ErrInvalid", name, err)
		}
	}
}

// TestProvisionerAcceptsValidIdentifiers asserts a well-formed app name passes validation and is
// provisioned all the way through, which is only observable now that provisioning is objects: the
// harness applies the status CloudNativePG would.
func TestProvisionerAcceptsValidIdentifiers(t *testing.T) {
	ctx := context.Background()
	p, dyn, _ := provisionerFor(t, addonNS)
	for _, name := range []string{"web", "my-app", "a", "web2", "a1b2-c3"} {
		if _, err := p.EnsureAppDatabase(ctx, name, controlplane.DefaultEnvironment); err != nil {
			t.Errorf("EnsureAppDatabase(%q): %v", name, err)
			continue
		}
		if _, err := dyn.Resource(cnpgDatabaseGVR).Namespace(addonNS).
			Get(ctx, provisioningObjectName(PostgresSecretName, name), metav1.GetOptions{}); err != nil {
			t.Errorf("EnsureAppDatabase(%q) wrote no Database object: %v", name, err)
		}
	}
}

// TestQuoteIdent checks the SQL-quoting helper doubles embedded quotes. It is the last quoting
// helper in the package: no generated password reaches a statement any more, so the literal quoter
// that existed for `CREATE ROLE ... PASSWORD` went with the statement.
func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Errorf("quoteIdent = %q", got)
	}
}

// TestProvisionerRequiresAnEnvironment asserts every provisioning method refuses an unnamed or
// malformed environment as ErrInvalid BEFORE any Secret read or connection (ADR-0067 §1). This is
// the seam-level statement of "a signature that can omit it is a signature that will omit it": there
// is no environment value that means "whichever instance is there", so a caller that forgets cannot
// silently land on another environment's server.
func TestProvisionerRequiresAnEnvironment(t *testing.T) {
	ctx := context.Background()
	// The DEFAULT environment's instance is fully installed, so a call that fell back to it would get
	// past validation and provision something rather than being refused. That is what distinguishes
	// "refused" from "quietly defaulted".
	p, _, _ := provisionerFor(t, addonNS, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	})

	for _, env := range []string{"", "Staging", "not a label", "staging/prod"} {
		if _, err := p.EnsureAppDatabase(ctx, "web", env); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("EnsureAppDatabase(web, %q) err = %v, want ErrInvalid", env, err)
		}
		if err := p.RevokeAppDatabase(ctx, "web", env); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RevokeAppDatabase(web, %q) err = %v, want ErrInvalid", env, err)
		}
		if err := p.DropAppDatabase(ctx, "web", env); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("DropAppDatabase(web, %q) err = %v, want ErrInvalid", env, err)
		}
		if _, err := p.ListAppDatabases(ctx, env); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("ListAppDatabases(%q) err = %v, want ErrInvalid", env, err)
		}
	}
}

// TestProvisionerReachesTheEnvironmentsOwnInstance asserts the environment selects the host AND the
// objects together: the default environment resolves to the instance an existing install already
// has, and another environment provisions against its own — never against the default's
// (ADR-0067 §1).
func TestProvisionerReachesTheEnvironmentsOwnInstance(t *testing.T) {
	ctx := context.Background()
	p, dyn, _ := provisionerFor(t, addonNS, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	})

	defHost, err := p.instanceHost(controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("instanceHost(default): %v", err)
	}
	if defHost != PostgresSecretName+"."+addonNS+".svc" {
		t.Errorf("default-environment host = %q, want the pre-existing %s.%s.svc", defHost, PostgresSecretName, addonNS)
	}
	stgHost, err := p.instanceHost("staging")
	if err != nil {
		t.Fatalf("instanceHost(staging): %v", err)
	}
	if stgHost == defHost {
		t.Fatalf("staging and the default environment dial the same host %q", stgHost)
	}

	// Attaching the same app name in staging provisions against STAGING's instance. The objects are
	// separate objects naming a separate `Cluster`, so there is no state either attach could adopt
	// from the other.
	if _, err := p.EnsureAppDatabase(ctx, "web", "staging"); err != nil {
		t.Fatalf("EnsureAppDatabase(web, staging): %v", err)
	}
	obj, err := dyn.Resource(cnpgDatabaseGVR).Namespace(addonNS).
		Get(ctx, provisioningObjectName("burrow-postgres-staging", "web"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("staging's Database object: %v", err)
	}
	if got := nestedString(t, obj.Object, "spec", "cluster", "name"); got != "burrow-postgres-staging" {
		t.Errorf("staging's Database names cluster %q, want staging's own instance", got)
	}
	if _, err := dyn.Resource(cnpgDatabaseGVR).Namespace(addonNS).
		Get(ctx, provisioningObjectName(PostgresSecretName, "web"), metav1.GetOptions{}); err == nil {
		t.Error("attaching in staging also provisioned against the default environment's instance")
	}
}

// TestAddonInstanceTargetIsTheInstanceAnExistingInstallAlreadyHas is the upgrade guarantee of issue
// #519: making the instance an explicit target must not MOVE it. The target burrowd is wired with —
// including for an install that never set BURROW_ADDON_NAMESPACE, which is every default install —
// names the same instance, in the same namespace, at the same host the provisioner used to derive
// for itself. The expected values are spelled out literally rather than recomputed from the helpers
// under test, so a change to either side fails here instead of quietly agreeing with itself.
func TestAddonInstanceTargetIsTheInstanceAnExistingInstallAlreadyHas(t *testing.T) {
	// What burrowd builds when BURROW_ADDON_NAMESPACE is unset.
	unconfigured := AddonInstanceTarget("")

	target, err := unconfigured(controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("default environment: %v", err)
	}
	if target.Instance != "burrow-postgres" || target.Namespace != "burrow-addons" {
		t.Errorf("default target = %s/%s, want burrow-addons/burrow-postgres — an existing install's instance moved", target.Namespace, target.Instance)
	}
	if got := target.Host(); got != "burrow-postgres.burrow-addons.svc" {
		t.Errorf("default host = %q, want burrow-postgres.burrow-addons.svc — every existing DATABASE_URL names that host", got)
	}

	// A second environment keeps its own instance, beside the first rather than on top of it
	// (ADR-0067 §1) — the environment still selects, it just selects within a configured set.
	staging, err := unconfigured("staging")
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if got := staging.Host(); got != "burrow-postgres-staging.burrow-addons.svc" {
		t.Errorf("staging host = %q, want burrow-postgres-staging.burrow-addons.svc", got)
	}

	// An install that DID set the add-on namespace is targeted at the namespace it set.
	elsewhere, err := AddonInstanceTarget("other-addons")(controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("configured namespace: %v", err)
	}
	if got := elsewhere.Host(); got != "burrow-postgres.other-addons.svc" {
		t.Errorf("host with a configured add-on namespace = %q, want burrow-postgres.other-addons.svc", got)
	}

	// And the address the provisioner actually dials is that host on the add-on port.
	p := NewPostgresProvisioner(fake.NewSimpleClientset(), nil, unconfigured)
	hostPort, err := p.dialHostPort(controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("dialHostPort: %v", err)
	}
	if hostPort != "burrow-postgres.burrow-addons.svc:5432" {
		t.Errorf("dialed address = %q, want burrow-postgres.burrow-addons.svc:5432", hostPort)
	}
}

// TestProvisionerActsOnlyOnTheInstanceItWasGiven asserts a provisioner told to act on an instance
// elsewhere neither dials nor reads the credential of a default-named instance sitting right there
// in the cluster its client is pointed at. That is issue #519 in miniature: a name derived from the
// caller's surroundings made a same-named database nearby indistinguishable from the intended one,
// and only a password mismatch kept the two apart. The surroundings can no longer supply a name.
func TestProvisionerActsOnlyOnTheInstanceItWasGiven(t *testing.T) {
	ctx := context.Background()
	// A fully installed default-named instance, in the default add-on namespace — the thing the old
	// derivation would have found and used.
	p, dyn, _ := provisionerFor(t, "tenant-addons", &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	})
	p.target = func(string) (PostgresTarget, error) {
		return PostgresTarget{Instance: "tenant-postgres", Namespace: "tenant-addons"}, nil
	}

	host, err := p.instanceHost(controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("instanceHost: %v", err)
	}
	if host != "tenant-postgres.tenant-addons.svc" {
		t.Errorf("host = %q, want the configured tenant-postgres.tenant-addons.svc", host)
	}

	// Provisioning lands on the configured instance, in the configured namespace — never on the
	// default-named one sitting right there in the cluster the client is pointed at.
	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("EnsureAppDatabase: %v", err)
	}
	obj, err := dyn.Resource(cnpgDatabaseGVR).Namespace("tenant-addons").
		Get(ctx, provisioningObjectName("tenant-postgres", "web"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the configured instance's Database object: %v", err)
	}
	if got := nestedString(t, obj.Object, "spec", "cluster", "name"); got != "tenant-postgres" {
		t.Errorf("Database names cluster %q, want the configured tenant-postgres", got)
	}
	if _, err := dyn.Resource(cnpgDatabaseGVR).Namespace(addonNS).
		Get(ctx, provisioningObjectName(PostgresSecretName, "web"), metav1.GetOptions{}); err == nil {
		t.Error("the provisioner reached the default-named instance it was not configured with")
	}
}

// TestProvisionerWithNoInstanceProvisionsNothing asserts a provisioner built with no target refuses
// every operation as ErrInvalid instead of falling back to a derived name. The default-named
// instance is present so a fallback would get all the way to a connection: "refused" and "quietly
// defaulted" are only distinguishable when defaulting would have worked.
func TestProvisionerWithNoInstanceProvisionsNothing(t *testing.T) {
	ctx := context.Background()
	p, _, _ := provisionerFor(t, addonNS, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	})
	p.target = nil

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("EnsureAppDatabase err = %v, want ErrInvalid", err)
	}
	if err := p.RevokeAppDatabase(ctx, "web", controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("RevokeAppDatabase err = %v, want ErrInvalid", err)
	}
	if err := p.DropAppDatabase(ctx, "web", controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("DropAppDatabase err = %v, want ErrInvalid", err)
	}
	if _, err := p.ListAppDatabases(ctx, controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("ListAppDatabases err = %v, want ErrInvalid", err)
	}
}
