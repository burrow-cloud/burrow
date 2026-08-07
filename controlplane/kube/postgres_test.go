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

// TestProvisionerRejectsBadIdentifiers asserts both EnsureAppDatabase and DropAppDatabase reject
// SQL-injection-shaped and malformed names as ErrInvalid BEFORE any connection/SQL (ADR-0031).
func TestProvisionerRejectsBadIdentifiers(t *testing.T) {
	ctx := context.Background()
	// No Secret and no database: a rejection must come from validation, before any I/O. (If
	// validation let a name through, the call would instead fail trying to read the Secret.)
	client := fake.NewSimpleClientset()
	p := NewPostgresProvisioner(client, AddonInstanceTarget(addonNS))

	bad := []string{"a; DROP DATABASE x", "App", "1x", "", "-web", "web name", "web\"; --", "WEB", "web_db", "web;"}
	for _, name := range bad {
		if _, err := p.EnsureAppDatabase(ctx, name, controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("EnsureAppDatabase(%q) err = %v, want ErrInvalid", name, err)
		}
		if err := p.DropAppDatabase(ctx, name, controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("DropAppDatabase(%q) err = %v, want ErrInvalid", name, err)
		}
	}
}

// TestProvisionerAcceptsValidIdentifiers asserts a well-formed app name passes validation (it then
// fails reaching the absent Secret, which proves validation let it through, not that it connected).
func TestProvisionerAcceptsValidIdentifiers(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	p := NewPostgresProvisioner(client, AddonInstanceTarget(addonNS))
	for _, name := range []string{"web", "my-app", "a", "web2", "a1b2-c3"} {
		_, err := p.EnsureAppDatabase(ctx, name, controlplane.DefaultEnvironment)
		if errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("EnsureAppDatabase(%q) was rejected as invalid, want it accepted", name)
		}
		// It should fail because the superuser Secret is absent — proving validation passed.
		if !errors.Is(err, controlplane.ErrNotFound) {
			t.Errorf("EnsureAppDatabase(%q) err = %v, want it to pass validation and fail on the missing secret", name, err)
		}
	}
}

// TestQuoteIdentAndLiteral checks the SQL-quoting helpers double embedded quotes.
func TestQuoteIdentAndLiteral(t *testing.T) {
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Errorf("quoteIdent = %q", got)
	}
	if got := quoteLiteral(`a'b`); got != `'a''b'` {
		t.Errorf("quoteLiteral = %q", got)
	}
}

// TestProvisionerRequiresAnEnvironment asserts every provisioning method refuses an unnamed or
// malformed environment as ErrInvalid BEFORE any Secret read or connection (ADR-0067 §1). This is
// the seam-level statement of "a signature that can omit it is a signature that will omit it": there
// is no environment value that means "whichever instance is there", so a caller that forgets cannot
// silently land on another environment's server.
func TestProvisionerRequiresAnEnvironment(t *testing.T) {
	ctx := context.Background()
	// A Secret for the DEFAULT environment's instance exists, so a call that fell back to it would
	// get past validation and fail later (on the connection) rather than as ErrInvalid. That is what
	// distinguishes "refused" from "quietly defaulted".
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	})
	p := NewPostgresProvisioner(client, AddonInstanceTarget(addonNS))

	for _, env := range []string{"", "Staging", "not a label", "staging/prod"} {
		if _, err := p.EnsureAppDatabase(ctx, "web", env); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("EnsureAppDatabase(web, %q) err = %v, want ErrInvalid", env, err)
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
// credential together: the default environment resolves to the instance an existing install already
// has, and another environment resolves to its own — never to the default's (ADR-0067 §1).
func TestProvisionerReachesTheEnvironmentsOwnInstance(t *testing.T) {
	ctx := context.Background()
	// Only the DEFAULT environment's instance is installed.
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	})
	p := NewPostgresProvisioner(client, AddonInstanceTarget(addonNS))

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

	// Staging has no instance installed, so provisioning there fails closed — naming staging's own
	// Secret — rather than falling back to the instance that does exist.
	_, err = p.EnsureAppDatabase(ctx, "web", "staging")
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("EnsureAppDatabase(web, staging) err = %v, want ErrNotFound for staging's absent instance", err)
	}
	if !strings.Contains(err.Error(), "burrow-postgres-staging") {
		t.Errorf("error %q does not name staging's own instance", err)
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
	p := NewPostgresProvisioner(fake.NewSimpleClientset(), unconfigured)
	hostPort, err := p.adminHostPort(controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("adminHostPort: %v", err)
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
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	})
	p := NewPostgresProvisioner(client, func(string) (PostgresTarget, error) {
		return PostgresTarget{Instance: "tenant-postgres", Namespace: "tenant-addons"}, nil
	})

	host, err := p.instanceHost(controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("instanceHost: %v", err)
	}
	if host != "tenant-postgres.tenant-addons.svc" {
		t.Errorf("host = %q, want the configured tenant-postgres.tenant-addons.svc", host)
	}

	// The configured instance is not installed here, so the attach fails closed naming IT — rather
	// than succeeding against the one that is.
	_, err = p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment)
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("EnsureAppDatabase err = %v, want ErrNotFound for the configured instance", err)
	}
	if !strings.Contains(err.Error(), "tenant-addons/tenant-postgres") {
		t.Errorf("error %q does not name the configured instance", err)
	}
	if strings.Contains(err.Error(), addonNS+"/"+PostgresSecretName) {
		t.Errorf("error %q reached the default-named instance the provisioner was not configured with", err)
	}
}

// TestProvisionerWithNoInstanceProvisionsNothing asserts a provisioner built with no target refuses
// every operation as ErrInvalid instead of falling back to a derived name. The default-named
// instance is present so a fallback would get all the way to a connection: "refused" and "quietly
// defaulted" are only distinguishable when defaulting would have worked.
func TestProvisionerWithNoInstanceProvisionsNothing(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	})
	p := NewPostgresProvisioner(client, nil)

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("EnsureAppDatabase err = %v, want ErrInvalid", err)
	}
	if err := p.DropAppDatabase(ctx, "web", controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("DropAppDatabase err = %v, want ErrInvalid", err)
	}
	if _, err := p.ListAppDatabases(ctx, controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("ListAppDatabases err = %v, want ErrInvalid", err)
	}
}
