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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

const addonNS = "burrow-addons"

// TestDeployPostgresCreatesSuperuserSecretBeforeDeployment asserts the install path creates the
// burrow-postgres superuser Secret BEFORE the Deployment, generates a strong password into it, and
// wires the Postgres container env to it via a secretKeyRef — never inlining the password into the
// pod spec, never returning it in AddonInfo (ADR-0031).
func TestDeployPostgresCreatesSuperuserSecretBeforeDeployment(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	// Record the order resources are created so we can prove the Secret precedes the Deployment.
	var order []string
	client.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		order = append(order, action.GetResource().Resource)
		return false, nil, nil // fall through to the default tracker
	})

	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonPostgres)
	info, err := a.DeployAddon(ctx, spec)
	if err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	// The superuser Secret exists in the add-on namespace and holds a non-trivial password.
	sec, err := client.CoreV1().Secrets(addonNS).Get(ctx, PostgresSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("superuser secret: %v", err)
	}
	pw := string(sec.Data[PostgresPasswordKey])
	if len(pw) < 20 {
		t.Errorf("generated password is too short (%d chars) — not a strong random password", len(pw))
	}

	// The Secret was created before the Deployment.
	secretIdx, depIdx := indexOf(order, "secrets"), indexOf(order, "deployments")
	if secretIdx < 0 || depIdx < 0 || secretIdx > depIdx {
		t.Errorf("create order = %v, want the secret created before the deployment", order)
	}

	// The Postgres container wires POSTGRES_USER (literal) and POSTGRES_PASSWORD (secretKeyRef),
	// and the password is NOT inlined anywhere in the pod spec.
	dep, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-postgres", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	c := dep.Spec.Template.Spec.Containers[0]

	// The add-on Postgres is tuned for a low-traffic store with the same lean settings as the
	// control-plane Postgres: `-c key=value` args the official image forwards to the server.
	argline := strings.Join(c.Args, " ")
	for _, want := range LeanPostgresSettings {
		if !strings.Contains(argline, "-c "+want) {
			t.Errorf("postgres args missing tuning setting %q; got %v", want, c.Args)
		}
	}
	// It declares a memory footprint (request + limit) so it fits a small VPS predictably.
	if got := c.Resources.Requests.Memory().String(); got != "96Mi" {
		t.Errorf("postgres memory request = %q, want 96Mi", got)
	}
	if got := c.Resources.Limits.Memory().String(); got != "320Mi" {
		t.Errorf("postgres memory limit = %q, want 320Mi", got)
	}
	if got := c.Resources.Requests.Cpu().String(); got != "50m" {
		t.Errorf("postgres cpu request = %q, want 50m", got)
	}
	// No CPU limit — throttling a database hurts latency.
	if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Errorf("postgres should declare no CPU limit, got %v", c.Resources.Limits.Cpu())
	}

	var sawUser, sawPasswordRef bool
	for _, ev := range c.Env {
		switch ev.Name {
		case "POSTGRES_USER":
			if ev.Value != PostgresSuperuser {
				t.Errorf("POSTGRES_USER = %q, want %q", ev.Value, PostgresSuperuser)
			}
			sawUser = true
		case "POSTGRES_PASSWORD":
			if ev.Value != "" {
				t.Errorf("POSTGRES_PASSWORD is inlined as a literal (%q) — it must use secretKeyRef", ev.Value)
			}
			if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil ||
				ev.ValueFrom.SecretKeyRef.Name != PostgresSecretName || ev.ValueFrom.SecretKeyRef.Key != PostgresPasswordKey {
				t.Errorf("POSTGRES_PASSWORD valueFrom = %+v, want secretKeyRef into %s/%s", ev.ValueFrom, PostgresSecretName, PostgresPasswordKey)
			}
			sawPasswordRef = true
		}
		// Defense-in-depth: no env var anywhere carries the generated password as a literal.
		if ev.Value == pw {
			t.Errorf("env %q inlines the generated superuser password", ev.Name)
		}
	}
	if !sawUser || !sawPasswordRef {
		t.Errorf("postgres env missing POSTGRES_USER and/or POSTGRES_PASSWORD secretKeyRef: %+v", c.Env)
	}

	// AddonInfo never carries the password.
	if strings.Contains(info.Image+info.Endpoint+info.Name, pw) {
		t.Error("AddonInfo leaks the generated password")
	}

	// DeleteAddon removes the superuser Secret too.
	if err := a.DeleteAddon(ctx, "burrow-postgres"); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.CoreV1().Secrets(addonNS).Get(ctx, PostgresSecretName, metav1.GetOptions{}); err == nil {
		t.Error("the superuser secret should be removed on uninstall")
	}
}

// TestDeployPostgresReusesExistingSecret asserts a re-install does not regenerate the password (the
// running database keeps working): an existing burrow-postgres Secret is left untouched.
func TestDeployPostgresReusesExistingSecret(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("preexisting-password")},
	})
	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonPostgres)
	if _, err := a.DeployAddon(ctx, spec); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	sec, _ := client.CoreV1().Secrets(addonNS).Get(ctx, PostgresSecretName, metav1.GetOptions{})
	if string(sec.Data[PostgresPasswordKey]) != "preexisting-password" {
		t.Error("re-install regenerated the superuser password — it must reuse the existing one")
	}
}

// TestDeployPostgresAlwaysExportsMetrics asserts the Postgres add-on ships an always-on
// postgres_exporter sidecar on :9187, carries the prometheus.io scrape annotations, preloads
// pg_stat_statements, mounts the extension init script, and wires the exporter's password via
// secretKeyRef — never inlining it (ADR-0051, ADR-0031).
func TestDeployPostgresAlwaysExportsMetrics(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonPostgres)
	if _, err := a.DeployAddon(ctx, spec); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	dep, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-postgres", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	pod := dep.Spec.Template

	// The scrape annotations point vmagent at the exporter's /metrics on :9187.
	ann := pod.ObjectMeta.Annotations
	if ann["prometheus.io/scrape"] != "true" || ann["prometheus.io/port"] != "9187" || ann["prometheus.io/path"] != "/metrics" {
		t.Errorf("pod scrape annotations = %v, want scrape=true port=9187 path=/metrics", ann)
	}

	// The main postgres container preloads pg_stat_statements (must be set at server start).
	main := pod.Spec.Containers[0]
	if !strings.Contains(strings.Join(main.Args, " "), "-c shared_preload_libraries=pg_stat_statements") {
		t.Errorf("postgres args missing shared_preload_libraries=pg_stat_statements; got %v", main.Args)
	}
	// It mounts the init-script ConfigMap at the official image's initdb hook directory.
	var sawInitMount bool
	for _, m := range main.VolumeMounts {
		if m.MountPath == "/docker-entrypoint-initdb.d" {
			sawInitMount = true
		}
	}
	if !sawInitMount {
		t.Errorf("postgres container is missing the /docker-entrypoint-initdb.d init mount; got %v", main.VolumeMounts)
	}
	cm, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, PostgresInitConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("init configmap: %v", err)
	}
	var sawCreateExtension bool
	for _, sql := range cm.Data {
		if strings.Contains(sql, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements") {
			sawCreateExtension = true
		}
	}
	if !sawCreateExtension {
		t.Errorf("init configmap does not create the pg_stat_statements extension; got %v", cm.Data)
	}

	// The exporter sidecar is present, on :9187, with the stat_statements collector enabled.
	if len(pod.Spec.Containers) < 2 {
		t.Fatalf("expected an exporter sidecar alongside postgres; got %d container(s)", len(pod.Spec.Containers))
	}
	exp := pod.Spec.Containers[1]
	if exp.Image != postgresExporterImage {
		t.Errorf("exporter image = %q, want %q", exp.Image, postgresExporterImage)
	}
	if len(exp.Ports) != 1 || exp.Ports[0].ContainerPort != postgresExporterPort {
		t.Errorf("exporter ports = %v, want a single :%d", exp.Ports, postgresExporterPort)
	}
	if !strings.Contains(strings.Join(exp.Args, " "), "--collector.stat_statements") {
		t.Errorf("exporter args missing --collector.stat_statements; got %v", exp.Args)
	}

	// The exporter password comes from the burrow-postgres Secret via secretKeyRef, never inlined.
	sec, _ := client.CoreV1().Secrets(addonNS).Get(ctx, PostgresSecretName, metav1.GetOptions{})
	pw := string(sec.Data[PostgresPasswordKey])
	var sawUser, sawPassRef bool
	for _, ev := range exp.Env {
		switch ev.Name {
		case "DATA_SOURCE_USER":
			if ev.Value != PostgresSuperuser {
				t.Errorf("DATA_SOURCE_USER = %q, want %q", ev.Value, PostgresSuperuser)
			}
			sawUser = true
		case "DATA_SOURCE_PASS":
			if ev.Value != "" {
				t.Errorf("DATA_SOURCE_PASS is inlined (%q) — it must use secretKeyRef", ev.Value)
			}
			if ev.ValueFrom == nil || ev.ValueFrom.SecretKeyRef == nil ||
				ev.ValueFrom.SecretKeyRef.Name != PostgresSecretName || ev.ValueFrom.SecretKeyRef.Key != PostgresPasswordKey {
				t.Errorf("DATA_SOURCE_PASS valueFrom = %+v, want secretKeyRef into %s/%s", ev.ValueFrom, PostgresSecretName, PostgresPasswordKey)
			}
			sawPassRef = true
		}
		if pw != "" && ev.Value == pw {
			t.Errorf("exporter env %q inlines the generated superuser password", ev.Name)
		}
	}
	if !sawUser || !sawPassRef {
		t.Errorf("exporter env missing DATA_SOURCE_USER and/or DATA_SOURCE_PASS secretKeyRef: %+v", exp.Env)
	}

	// DeleteAddon removes the init ConfigMap too.
	if err := a.DeleteAddon(ctx, "burrow-postgres"); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, PostgresInitConfigMap, metav1.GetOptions{}); err == nil {
		t.Error("the init configmap should be removed on uninstall")
	}
}

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
	if _, err := a.DeployAddon(ctx, spec); err != nil {
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
	if _, err := a.DeployAddon(ctx, spec); err != nil {
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
		if _, err := p.EnsureAppDatabase(ctx, name); !errors.Is(err, controlplane.ErrInvalid) {
			t.Errorf("EnsureAppDatabase(%q) err = %v, want ErrInvalid", name, err)
		}
		if err := p.DropAppDatabase(ctx, name); !errors.Is(err, controlplane.ErrInvalid) {
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
		_, err := p.EnsureAppDatabase(ctx, name)
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

// indexOf returns the first index of s in xs, or -1.
func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}
