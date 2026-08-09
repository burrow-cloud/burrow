// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), nil); err != nil {
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
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), nil); err != nil {
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
		if _, err := p.EnsureAppDatabase(ctx, name, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("EnsureAppDatabase(%q) err = %v, want ErrInvalid", name, err)
		}
		if err := p.RevokeAppDatabase(ctx, name, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RevokeAppDatabase(%q) err = %v, want ErrInvalid", name, err)
		}
		if err := p.DropAppDatabase(ctx, name, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); !errors.Is(err, controlplane.ErrInvalid) {
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
		if _, err := p.EnsureAppDatabase(ctx, name, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
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
		if _, err := p.EnsureAppDatabase(ctx, "web", env, testInstance(env)); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("EnsureAppDatabase(web, %q) err = %v, want ErrInvalid", env, err)
		}
		if err := p.RevokeAppDatabase(ctx, "web", env, testInstance(env)); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("RevokeAppDatabase(web, %q) err = %v, want ErrInvalid", env, err)
		}
		if err := p.DropAppDatabase(ctx, "web", env, testInstance(env)); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("DropAppDatabase(web, %q) err = %v, want ErrInvalid", env, err)
		}
		if _, err := p.ListAppDatabases(ctx, env, testInstance(env)); !errors.Is(err, controlplane.ErrInvalid) {
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

	defHost, err := p.instanceHost(controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
	if err != nil {
		t.Fatalf("instanceHost(default): %v", err)
	}
	if defHost != PostgresSecretName+"."+addonNS+".svc" {
		t.Errorf("default-environment host = %q, want the pre-existing %s.%s.svc", defHost, PostgresSecretName, addonNS)
	}
	stgHost, err := p.instanceHost("staging", testInstance("staging"))
	if err != nil {
		t.Fatalf("instanceHost(staging): %v", err)
	}
	if stgHost == defHost {
		t.Fatalf("staging and the default environment dial the same host %q", stgHost)
	}

	// Attaching the same app name in staging provisions against STAGING's instance. The objects are
	// separate objects naming a separate `Cluster`, so there is no state either attach could adopt
	// from the other.
	if _, err := p.EnsureAppDatabase(ctx, "web", "staging", testInstance("staging")); err != nil {
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

	target, err := unconfigured(controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
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
	staging, err := unconfigured("staging", testInstance("staging"))
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if got := staging.Host(); got != "burrow-postgres-staging.burrow-addons.svc" {
		t.Errorf("staging host = %q, want burrow-postgres-staging.burrow-addons.svc", got)
	}

	// An install that DID set the add-on namespace is targeted at the namespace it set.
	elsewhere, err := AddonInstanceTarget("other-addons")(controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
	if err != nil {
		t.Fatalf("configured namespace: %v", err)
	}
	if got := elsewhere.Host(); got != "burrow-postgres.other-addons.svc" {
		t.Errorf("host with a configured add-on namespace = %q, want burrow-postgres.other-addons.svc", got)
	}

	// And the address the provisioner actually dials is that host on the add-on port.
	p := NewPostgresProvisioner(fake.NewSimpleClientset(), nil, unconfigured)
	hostPort, err := p.dialHostPort(controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
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
	p.target = func(string, string) (PostgresTarget, error) {
		return PostgresTarget{Instance: "tenant-postgres", Namespace: "tenant-addons"}, nil
	}

	host, err := p.instanceHost(controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
	if err != nil {
		t.Fatalf("instanceHost: %v", err)
	}
	if host != "tenant-postgres.tenant-addons.svc" {
		t.Errorf("host = %q, want the configured tenant-postgres.tenant-addons.svc", host)
	}

	// Provisioning lands on the configured instance, in the configured namespace — never on the
	// default-named one sitting right there in the cluster the client is pointed at.
	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
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

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("EnsureAppDatabase err = %v, want ErrInvalid", err)
	}
	if err := p.RevokeAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("RevokeAppDatabase err = %v, want ErrInvalid", err)
	}
	if err := p.DropAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("DropAppDatabase err = %v, want ErrInvalid", err)
	}
	if _, err := p.ListAppDatabases(ctx, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("ListAppDatabases err = %v, want ErrInvalid", err)
	}
}

// appNameOfLen builds a legal app name of exactly n characters: a leading letter, as appIdentifier
// (and DNS-1123) requires, and letters after it.
func appNameOfLen(n int) string { return "a" + strings.Repeat("b", n-1) }

// truncateIdentifier is what the SERVER does to an identifier longer than 63 bytes: it does not
// refuse it, it silently keeps the first 63. Every assertion about two role names being distinct has
// to be made through this, because two names that differ are still one role if they differ only
// past the cut.
func truncateIdentifier(s string) string {
	if len(s) > maxIdentifierLen {
		return s[:maxIdentifierLen]
	}
	return s
}

// TestNoAttachableAppNameCollapsesItsTwoRoles is issue #534's property, asserted over the whole legal
// length range rather than for a sampled name: for EVERY app name Burrow will attach a database to,
// the login role and the login-less data role are two different roles on the server.
//
// It walks the length axis because that is the axis the defect lives on — the two names differ as
// strings at every length, and stop differing as ROLES only once the server's truncation reaches
// back into the shared prefix. A name at any other length would pass while the boundary was broken.
//
// The refusals are pinned too, in both directions. A length the budget admits must attach, and a
// length it refuses must refuse as ErrInvalid — so widening the budget to admit a name that
// truncates, or narrowing it until it refuses a name that would have been fine, both fail here.
func TestNoAttachableAppNameCollapsesItsTwoRoles(t *testing.T) {
	// The upper bound is the app name's own limit: a DNS-1123 label, so 63 characters
	// (controlplane.App.Validate). Nothing longer can reach the provisioner as an app at all.
	for n := 1; n <= 63; n++ {
		app := appNameOfLen(n)
		err := validatePostgresAppName(app)
		if n > MaxPostgresAppNameLen {
			if !errors.Is(err, controlplane.ErrInvalid) {
				t.Errorf("an app name of %d characters is over the %d-character budget but validated with err = %v, want ErrInvalid", n, MaxPostgresAppNameLen, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("an app name of %d characters is within the %d-character budget but was refused: %v", n, MaxPostgresAppNameLen, err)
		}
		login, data := roleName(app), dataRoleName(app)
		// Nothing an admitted name derives is truncated at all, which is the strong form of the
		// property: a name the server never cuts cannot be cut onto another one.
		if len(login) > maxIdentifierLen {
			t.Errorf("an app name of %d characters derives a %d-byte login role, past PostgreSQL's %d", n, len(login), maxIdentifierLen)
		}
		if len(data) > maxIdentifierLen {
			t.Errorf("an app name of %d characters derives a %d-byte data role, past PostgreSQL's %d", n, len(data), maxIdentifierLen)
		}
		if truncateIdentifier(login) == truncateIdentifier(data) {
			t.Errorf("an app name of %d characters gives the login role and the data role the same name %q: one role cannot be both able and unable to log in", n, truncateIdentifier(login))
		}
	}
}

// TestTheAppNameBudgetIsExactlyTheDataRole pins the budget to the name that binds it. It is one
// assertion in two halves: the longest admitted name spends the identifier limit to the last byte,
// and one character more spills over it. A budget with slack in it would pass the property test
// above while refusing names it never needed to.
func TestTheAppNameBudgetIsExactlyTheDataRole(t *testing.T) {
	longest := appNameOfLen(MaxPostgresAppNameLen)
	if got := len(dataRoleName(longest)); got != maxIdentifierLen {
		t.Errorf("the longest admitted app name derives a %d-byte data role, want exactly %d: the budget is meant to be the identifier limit, not less", got, maxIdentifierLen)
	}
	if got := len(dataRoleName(appNameOfLen(MaxPostgresAppNameLen + 1))); got <= maxIdentifierLen {
		t.Errorf("one character past the budget still derives a %d-byte data role, so the budget is not where the limit is", got)
	}
}

// TestTheRefusedLengthsCoverTheCollapse names the hazard the budget exists for, so that a later
// widening of it fails on the reason rather than on the number. At the length where app_<app> first
// fills the identifier limit, the naive app_<app>_data truncates onto it EXACTLY — the two roles
// become one — and that length must be refused.
func TestTheRefusedLengthsCoverTheCollapse(t *testing.T) {
	// 63 − len("app_") = 59: the shortest app name whose login role already spends the whole limit, so
	// there is nothing left of it for the suffix to occupy.
	collapse := maxIdentifierLen - len(rolePrefix)
	app := appNameOfLen(collapse)
	if truncateIdentifier(roleName(app)+dataRoleSuffix) != truncateIdentifier(roleName(app)) {
		t.Fatalf("an app name of %d characters no longer collapses its two roles; this test is asserting about the wrong length", collapse)
	}
	if collapse <= MaxPostgresAppNameLen {
		t.Fatalf("the budget admits %d characters, at which the two role names truncate onto one role", collapse)
	}
	if err := validatePostgresAppName(app); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("validatePostgresAppName(%d characters) err = %v, want ErrInvalid", collapse, err)
	}
}

// TestAttachRefusesAnAppNameItCannotServe is the refusal where the acceptance list puts it: at
// attach, before anything is written. The longest servable name goes all the way through and gets
// two roles that differ in the one property they exist to differ in; one character more is refused
// with the limit named, and leaves the instance untouched.
func TestAttachRefusesAnAppNameItCannotServe(t *testing.T) {
	ctx := context.Background()
	p, dyn, _ := provisionerFor(t, addonNS)

	longest := appNameOfLen(MaxPostgresAppNameLen)
	if _, err := p.EnsureAppDatabase(ctx, longest, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
		t.Fatalf("the longest servable app name failed to attach: %v", err)
	}
	roles := dyn.Resource(cnpgDatabaseRoleGVR).Namespace(addonNS)
	login, err := roles.Get(ctx, provisioningObjectName(PostgresSecretName, longest), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the login role object: %v", err)
	}
	data, err := roles.Get(ctx, dataRoleObjectName(PostgresSecretName, longest), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the data role object: %v", err)
	}
	// Two objects is not the property; two ROLES is. The names the server is handed must differ, and
	// they must carry opposite login dispositions — the pair collapsing is exactly one role being
	// asked for both.
	loginRole := nestedString(t, login.Object, "spec", "name")
	dataRole := nestedString(t, data.Object, "spec", "name")
	if loginRole == dataRole {
		t.Fatalf("both DatabaseRole objects name the role %q", loginRole)
	}
	if can, _, _ := unstructured.NestedBool(login.Object, "spec", "login"); !can {
		t.Error("the login role is described as unable to log in")
	}
	if can, _, _ := unstructured.NestedBool(data.Object, "spec", "login"); can {
		t.Error("the data role is described as able to log in")
	}

	// One character more. The count is taken before the call so the assertion is about what THIS
	// attach wrote, not about the fake being empty.
	before := len(dyn.Actions())
	tooLong := appNameOfLen(MaxPostgresAppNameLen + 1)
	_, err = p.EnsureAppDatabase(ctx, tooLong, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("EnsureAppDatabase(%d characters) err = %v, want ErrInvalid", len(tooLong), err)
	}
	// The refusal has to be readable by whoever hit it: the length that failed and the length that
	// would not have.
	for _, want := range []string{tooLong, strconv.Itoa(MaxPostgresAppNameLen)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if got := len(dyn.Actions()); got != before {
		t.Errorf("a refused attach performed %d cluster operations, want none: the length is refused before anything is provisioned", got-before)
	}
	if secrets, _ := p.client.CoreV1().Secrets(addonNS).List(ctx, metav1.ListOptions{}); len(secrets.Items) != 1 {
		t.Errorf("a refused attach left %d password secrets, want only the servable app's one", len(secrets.Items))
	}
}

// TestDetachStillReachesAnAppNameAttachWouldRefuse is the other side of putting the budget on the
// attach path alone. An attachment provisioned by an earlier release under a longer name still has
// its database and its roles under the names it was created with, and both teardown verbs have to be
// able to take it apart — a length refused here would strand exactly the attachments the budget
// exists to stop anyone from making.
func TestDetachStillReachesAnAppNameAttachWouldRefuse(t *testing.T) {
	ctx := context.Background()
	p, _, _ := provisionerFor(t, addonNS)

	tooLong := appNameOfLen(MaxPostgresAppNameLen + 1)
	if err := p.RevokeAppDatabase(ctx, tooLong, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
		t.Errorf("a detach of an over-budget app name failed: %v", err)
	}
	if err := p.DropAppDatabase(ctx, tooLong, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
		t.Errorf("a --delete-data detach of an over-budget app name failed: %v", err)
	}
}
