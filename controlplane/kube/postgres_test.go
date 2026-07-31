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
	info, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment)
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

	// A data-deleting removal takes the superuser Secret with the volume: the Secret only opens the
	// volume, so once the volume is gone it is dead weight.
	if _, err := a.DeleteAddon(ctx, "burrow-postgres", true); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.CoreV1().Secrets(addonNS).Get(ctx, PostgresSecretName, metav1.GetOptions{}); err == nil {
		t.Error("the superuser secret should be removed by a data-deleting uninstall")
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
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
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
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
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
	// POSTGRES_DB=postgres pins the maintenance database so the init script creates the extension in
	// the same database the exporter and control plane read; without it the image would default the
	// database to the POSTGRES_USER name and the extension would land in the wrong place (ADR-0051).
	var sawDB bool
	for _, ev := range main.Env {
		if ev.Name == "POSTGRES_DB" {
			if ev.Value != "postgres" {
				t.Errorf("POSTGRES_DB = %q, want postgres", ev.Value)
			}
			sawDB = true
		}
	}
	if !sawDB {
		t.Errorf("postgres env missing POSTGRES_DB=postgres: %+v", main.Env)
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

	// A data-deleting removal takes the init ConfigMap with the volume: it only ever runs during
	// initdb on a fresh volume.
	if _, err := a.DeleteAddon(ctx, "burrow-postgres", true); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, PostgresInitConfigMap, metav1.GetOptions{}); err == nil {
		t.Error("the init configmap should be removed by a data-deleting uninstall")
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

// indexOf returns the first index of s in xs, or -1.
func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// TestDeleteAddonKeepsVolumeAndCredentialByDefault is the adapter-level half of the data-survival
// rule: a removal that did not ask for the data destroys the workload and nothing else. The
// superuser Secret is checked alongside the volume on purpose — the official postgres image only
// honours POSTGRES_PASSWORD during initdb, so a reinstall over a retained volume never re-runs it.
// Deleting the Secret while keeping the volume would leave a database nobody — not burrowd, not the
// exporter, not the backup Jobs — can authenticate to: data that is present and permanently
// unreachable, which is worse than losing it cleanly (ADR-0031).
func TestDeleteAddonKeepsVolumeAndCredentialByDefault(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonPostgres)
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	removal, err := a.DeleteAddon(ctx, "burrow-postgres", false)
	if err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-postgres", metav1.GetOptions{}); err == nil {
		t.Error("the workload should be gone")
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, "burrow-postgres", metav1.GetOptions{}); err != nil {
		t.Errorf("the data volume should survive a removal that did not ask for it: %v", err)
	}
	if _, err := client.CoreV1().Secrets(addonNS).Get(ctx, PostgresSecretName, metav1.GetOptions{}); err != nil {
		t.Errorf("the superuser secret must survive with the volume it opens: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, PostgresInitConfigMap, metav1.GetOptions{}); err != nil {
		t.Errorf("the init configmap should survive with the volume: %v", err)
	}
	if removal.DataDeleted {
		t.Error("removal reports DataDeleted for a data-keeping removal")
	}
	if removal.RetainedDataVolume != "burrow-postgres" || removal.Namespace != addonNS {
		t.Errorf("removal = %+v, want the retained volume named in %s", removal, addonNS)
	}
}

// TestDeleteAddonDeleteDataDestroysVolume asserts the explicit opt-in destroys the volume and the
// credentials that only exist to open it.
func TestDeleteAddonDeleteDataDestroysVolume(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonPostgres)
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	removal, err := a.DeleteAddon(ctx, "burrow-postgres", true)
	if err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, "burrow-postgres", metav1.GetOptions{}); err == nil {
		t.Error("--delete-data did not destroy the data volume")
	}
	if !removal.DataDeleted || removal.RetainedDataVolume != "" {
		t.Errorf("removal = %+v, want DataDeleted with no retained volume", removal)
	}
}

// TestDeleteAddonKeepsBackupVolume asserts the backup PVC survives a data-deleting removal and is
// reported (ADR-0032): the dumps are the safety net that makes destroying the database survivable, so
// they deliberately outlive it. It is reported rather than silently left so the operator knows the
// storage is still allocated.
func TestDeleteAddonKeepsBackupVolume(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonPostgres)
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	if err := a.ensureBackupPVC(ctx, controlplane.DefaultEnvironment, controlplane.PostgresBackupVolume); err != nil {
		t.Fatalf("ensureBackupPVC: %v", err)
	}

	removal, err := a.DeleteAddon(ctx, "burrow-postgres", true)
	if err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, controlplane.PostgresBackupVolume, metav1.GetOptions{}); err != nil {
		t.Errorf("the backup volume must outlive the database: %v", err)
	}
	if removal.RetainedBackupVolume != controlplane.PostgresBackupVolume {
		t.Errorf("RetainedBackupVolume = %q, want %q", removal.RetainedBackupVolume, controlplane.PostgresBackupVolume)
	}
}

// TestDeleteAddonReportsNoBackupVolumeWhenAbsent asserts a cluster that never backed up is not told
// it retained a backup volume that does not exist — the report has to be a fact, not a template.
func TestDeleteAddonReportsNoBackupVolumeWhenAbsent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonPostgres)
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	removal, err := a.DeleteAddon(ctx, "burrow-postgres", false)
	if err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if removal.RetainedBackupVolume != "" {
		t.Errorf("RetainedBackupVolume = %q, want empty when no backup was ever taken", removal.RetainedBackupVolume)
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

// TestDeployPostgresPerEnvironmentIsASeparateInstance asserts installing Postgres for a second
// environment creates a WHOLE second instance — its own Deployment, Service, volume, superuser
// Secret, and init ConfigMap — and leaves the first environment's untouched (ADR-0067 §1). The
// superuser credential is per instance because it is what opens that server's data directory.
func TestDeployPostgresPerEnvironmentIsASeparateInstance(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)
	spec, _ := controlplane.LookupAddon(controlplane.AddonPostgres)

	def, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("DeployAddon(default): %v", err)
	}
	stg, err := a.DeployAddon(ctx, spec, "staging")
	if err != nil {
		t.Fatalf("DeployAddon(staging): %v", err)
	}
	if def.Name != "burrow-postgres" {
		t.Errorf("default-environment instance = %q, want the name an existing install already has", def.Name)
	}
	if stg.Name != "burrow-postgres-staging" || stg.Endpoint == def.Endpoint {
		t.Fatalf("staging instance = %q endpoint %q, want its own instance and endpoint", stg.Name, stg.Endpoint)
	}

	for _, name := range []string{def.Name, stg.Name} {
		if _, derr := client.AppsV1().Deployments(addonNS).Get(ctx, name, metav1.GetOptions{}); derr != nil {
			t.Errorf("Deployment %q: %v", name, derr)
		}
		if _, serr := client.CoreV1().Services(addonNS).Get(ctx, name, metav1.GetOptions{}); serr != nil {
			t.Errorf("Service %q: %v", name, serr)
		}
		if _, verr := client.CoreV1().PersistentVolumeClaims(addonNS).Get(ctx, name, metav1.GetOptions{}); verr != nil {
			t.Errorf("volume %q: %v", name, verr)
		}
		if _, cerr := client.CoreV1().ConfigMaps(addonNS).Get(ctx, postgresInitConfigMap(name), metav1.GetOptions{}); cerr != nil {
			t.Errorf("init ConfigMap for %q: %v", name, cerr)
		}
	}

	// Two superuser Secrets with DIFFERENT passwords: one server's credential never opens another's.
	defSec, err := client.CoreV1().Secrets(addonNS).Get(ctx, def.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("default superuser secret: %v", err)
	}
	stgSec, err := client.CoreV1().Secrets(addonNS).Get(ctx, stg.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("staging superuser secret: %v", err)
	}
	if string(defSec.Data[PostgresPasswordKey]) == string(stgSec.Data[PostgresPasswordKey]) {
		t.Error("both instances share a superuser password — the credential is not per instance")
	}

	// Removing staging's instance with its data takes only staging's credential and init script.
	if _, rerr := a.DeleteAddon(ctx, stg.Name, true); rerr != nil {
		t.Fatalf("DeleteAddon(staging): %v", rerr)
	}
	if _, gerr := client.CoreV1().Secrets(addonNS).Get(ctx, stg.Name, metav1.GetOptions{}); gerr == nil {
		t.Error("staging's superuser secret survived a data-deleting removal of staging's instance")
	}
	if _, gerr := client.CoreV1().Secrets(addonNS).Get(ctx, def.Name, metav1.GetOptions{}); gerr != nil {
		t.Errorf("removing staging's instance deleted the DEFAULT environment's superuser secret: %v", gerr)
	}
	if _, gerr := client.AppsV1().Deployments(addonNS).Get(ctx, def.Name, metav1.GetOptions{}); gerr != nil {
		t.Errorf("removing staging's instance deleted the default environment's Deployment: %v", gerr)
	}
}
