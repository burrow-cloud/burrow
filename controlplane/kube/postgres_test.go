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
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
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
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
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
	p := NewPostgresProvisioner(client, addonNS)

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
	p := NewPostgresProvisioner(client, addonNS)
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
	p := NewPostgresProvisioner(client, addonNS)

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
	p := NewPostgresProvisioner(client, addonNS)

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
