// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestPostgresAddonE2E drives the Postgres add-on through the real Kubernetes adapter and the real
// admin-SQL provisioner against a live cluster (ADR-0031): install the instance, attach an app
// (which provisions an isolated database + role and writes DATABASE_URL into the app's Secret),
// then run an in-cluster Job that connects with that DATABASE_URL and round-trips a row, and finally
// detach (dropping the database). Like the other e2es it runs only when BURROW_TEST_KUBECONFIG
// points at a disposable cluster; it creates its own namespaces and cleans them up. The round-trip
// runs inside the cluster because the add-on Service (burrow-postgres.<ns>.svc) is only reachable
// from in-cluster.
//
// The instance is a CloudNativePG `Cluster` (ADR-0066 §1), which makes the OPERATOR a prerequisite
// of this test rather than a variant of it: there is no second mechanism to fall back to. The
// harness installs it (scripts/with-k3d.sh, and the CI job's own step), and a cluster without it
// skips with the command that fixes it rather than failing on a refusal it cannot do anything about.
// Everything below the install is unchanged by the mechanism, which is the point of asserting it
// here: attach, DATABASE_URL, the round-trip and detach are ADR-0031's contract and they are
// expressed against an endpoint, not against a workload kind.
//
// It then does the whole thing again in a SECOND environment with the SAME app name, which is the
// end-to-end statement of ADR-0067 §1 (issue #339): staging gets its own instance, its own database,
// and its own credential, and the two hold different rows. That collision is invisible to a unit
// test with a fake provisioner in one important respect — the old code did not error, it silently
// resolved to the other environment's live server — so it is worth paying for two real Postgres
// instances here to see the data actually stay apart.
func TestPostgresAddonE2E(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the Postgres add-on end-to-end test")
	}
	ctx := context.Background()

	cfg, err := kube.ConfigFromKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	found, err := kube.DetectCloudNativePG(ctx, client)
	if err != nil {
		t.Fatalf("detecting CloudNativePG: %v", err)
	}
	if !found.Ready {
		t.Skipf("the postgres add-on is a CloudNativePG Cluster (ADR-0066 §1) and no controller is running on this cluster; "+
			"install the operator first (kubectl apply --server-side -f %s)", kube.CNPGManifestURL(kube.CNPGVersion))
	}

	stamp := time.Now().UnixNano()
	appNS := fmt.Sprintf("burrow-pg-app-%d", stamp)
	stagingNS := fmt.Sprintf("burrow-pg-staging-%d", stamp)
	addonNS := fmt.Sprintf("burrow-pg-addons-%d", stamp)
	for _, ns := range []string{appNS, stagingNS, addonNS} {
		if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
		ns := ns
		t.Cleanup(func() { _ = client.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })
	}

	k8s := kube.New(client, appNS).WithAddonNamespace(addonNS).WithDynamicClient(dyn)
	prov := kube.NewPostgresProvisioner(client, kube.AddonInstanceTarget(addonNS))
	db := fake.NewDatabase()
	engine, err := cp.New(cp.Deps{
		Kubernetes:          k8s,
		Database:            db,
		Clock:               fake.NewClock(time.Now()),
		IDs:                 fake.NewIDs(),
		Resolver:            fake.NewResolver(),
		Credentials:         fake.NewCredentials(),
		DNS:                 fake.NewDNSFactory(),
		DatabaseProvisioner: prov,
		// The app namespace is wired into the ENGINE as well as the adapter, exactly as burrowd
		// wires it from BURROW_NAMESPACE (cmd/burrowd/main.go). The engine resolves an operation's
		// environment to a namespace and acts through that view (ADR-0035 phase 2b), so an engine
		// that did not know its own app namespace would route an app-scoped write — including the
		// DATABASE_URL an attach writes — to the literal "default" namespace instead.
		AppNamespace: appNS,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	const app = "shop"

	// Install the Postgres instance and wait for it to become ready. confirm=true clears the
	// addon.install guardrail (the fake DB's default policy holds it for confirmation).
	if _, err := engine.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon postgres: %v", err)
	}
	waitForCond(t, 180*time.Second, "postgres ready", func() (bool, error) {
		return k8s.AddonReady(ctx, "burrow-postgres")
	})

	// This test process runs OUT of the cluster, so it cannot resolve the instance's in-cluster
	// Service DNS name (burrow-postgres.<ns>.svc) that burrowd uses in production. For each admin
	// operation, port-forward the Postgres pod to a local port and point the provisioner's ADMIN
	// connection at it; the app's DATABASE_URL still gets the in-cluster Service name, which the
	// round-trip Job (a pod) resolves. A fresh forward per operation keeps the test robust against
	// a single forward dropping mid-run.
	//
	// With an instance per environment there can be more than one Postgres pod in the add-on
	// namespace, so the forward must name the environment's own instance rather than "a postgres"
	// (ADR-0067 §1). The instance carries the environment as a label.
	pgSelector := "burrow.cloud/addon=postgres,burrow.cloud/environment=" + cp.DefaultEnvironment

	// Attach the app: provisions the database/role and writes DATABASE_URL into the app's Secret.
	var res cp.AttachResult
	withPortForward(t, cfg, client, addonNS, pgSelector, 5432, "attach addon", func(localPort int) error {
		prov.WithAdminEndpoint(fmt.Sprintf("127.0.0.1:%d", localPort))
		var aerr error
		res, aerr = engine.AttachAddon(ctx, cp.AddonPostgres, app, "", "")
		return aerr
	})
	if res.SecretKey != "DATABASE_URL" {
		t.Fatalf("attach SecretKey = %q, want DATABASE_URL", res.SecretKey)
	}
	// Assert the Secret landed in this environment's namespace BEFORE running a Job that mounts it.
	// A Secret written to the wrong namespace does not fail the Job — the pod sits in
	// CreateContainerConfigError and never starts, so the symptom is a three-minute timeout with
	// nothing to read. Checking here turns that into an immediate, legible failure.
	if _, err := client.CoreV1().Secrets(appNS).Get(ctx, cp.AppSecretName(app), metav1.GetOptions{}); err != nil {
		t.Fatalf("attach did not write the app Secret into the environment's namespace %s: %v", appNS, err)
	}

	// Round-trip a row from inside the cluster using the app's DATABASE_URL (sourced from the
	// per-app Secret), proving the credential and the database both work.
	runRoundTripJob(t, ctx, client, appNS, app)

	// ---- The same app name, in a second environment (ADR-0067 §1) ----
	//
	// Register `staging` against its own namespace and install ITS Postgres instance. This is the
	// exact sequence that used to corrupt data: `env add staging`, then attach an app that already
	// exists in the first environment. Provisioning is idempotent, so the second attach did not
	// fail — it found the first environment's `shop` database, rotated the role password, and handed
	// staging a DATABASE_URL pointing at the other environment's live rows.
	if err := db.CreateEnvironment(ctx, "staging", stagingNS); err != nil {
		t.Fatalf("CreateEnvironment(staging): %v", err)
	}
	if _, err := engine.InstallAddon(ctx, cp.AddonPostgres, "staging", cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon postgres (staging): %v", err)
	}
	stagingInstance, err := cp.AddonInstanceName(cp.AddonPostgres, "staging")
	if err != nil {
		t.Fatalf("AddonInstanceName(staging): %v", err)
	}
	if stagingInstance == "burrow-postgres" {
		t.Fatalf("staging resolved to the default environment's instance %q", stagingInstance)
	}
	waitForCond(t, 180*time.Second, "staging postgres ready", func() (bool, error) {
		return k8s.AddonReady(ctx, stagingInstance)
	})

	stagingSelector := "burrow.cloud/addon=postgres,burrow.cloud/environment=staging"
	var stagingRes cp.AttachResult
	withPortForward(t, cfg, client, addonNS, stagingSelector, 5432, "attach addon (staging)", func(localPort int) error {
		prov.WithAdminEndpoint(fmt.Sprintf("127.0.0.1:%d", localPort))
		var aerr error
		stagingRes, aerr = engine.AttachAddon(ctx, cp.AddonPostgres, app, "staging", "")
		return aerr
	})
	if stagingRes.Environment != "staging" {
		t.Errorf("staging attach reported environment %q, want staging", stagingRes.Environment)
	}

	// Staging's DATABASE_URL landed in STAGING's namespace, and it names staging's own instance.
	// Reading the value here is test-only introspection of a Secret this test created; the engine
	// never returns it.
	stagingURL := secretValue(t, ctx, client, stagingNS, cp.AppSecretName(app), "DATABASE_URL")
	defaultURL := secretValue(t, ctx, client, appNS, cp.AppSecretName(app), "DATABASE_URL")
	if stagingURL == defaultURL {
		t.Fatal("both environments were handed the same connection string — staging is pointed at the other environment's data (issue #339)")
	}
	if !strings.Contains(stagingURL, stagingInstance+".") {
		t.Errorf("staging's DATABASE_URL does not name staging's instance %q", stagingInstance)
	}

	// Write a DIFFERENT row through staging's credential, then assert each environment's database
	// holds exactly its own. Sharing one server would show up here as two rows, because both jobs
	// insert into the same table name in a database with the same name.
	runSQLJob(t, ctx, client, stagingNS, app, "roundtrip",
		`psql "$DATABASE_URL" -c "CREATE TABLE IF NOT EXISTS t (id int);"
psql "$DATABASE_URL" -c "INSERT INTO t VALUES (7);"
test "$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM t;")" = "1"
psql "$DATABASE_URL" -tAc "SELECT id FROM t;" | grep -q 7`)
	runSQLJob(t, ctx, client, appNS, app, "isolation",
		`test "$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM t;")" = "1"
psql "$DATABASE_URL" -tAc "SELECT id FROM t;" | grep -q 42`)

	// Detaching in staging drops staging's database only: the default environment's data survives,
	// which is the destructive half of the same property.
	withPortForward(t, cfg, client, addonNS, stagingSelector, 5432, "detach addon (staging)", func(localPort int) error {
		prov.WithAdminEndpoint(fmt.Sprintf("127.0.0.1:%d", localPort))
		return engine.DetachAddon(ctx, cp.AddonPostgres, app, "staging", true)
	})
	runSQLJob(t, ctx, client, appNS, app, "survives",
		`psql "$DATABASE_URL" -tAc "SELECT id FROM t;" | grep -q 42`)

	// Detach: drops the database and role and removes the DATABASE_URL key (also an admin
	// operation, so it runs through a fresh port-forward).
	withPortForward(t, cfg, client, addonNS, pgSelector, 5432, "detach addon", func(localPort int) error {
		prov.WithAdminEndpoint(fmt.Sprintf("127.0.0.1:%d", localPort))
		return engine.DetachAddon(ctx, cp.AddonPostgres, app, cp.DefaultEnvironment, true)
	})
	keys, err := k8s.SecretKeys(ctx, app)
	if err != nil {
		t.Fatalf("SecretKeys after detach: %v", err)
	}
	for _, k := range keys {
		if k == "DATABASE_URL" {
			t.Errorf("DATABASE_URL should be removed from the app's Secret after detach")
		}
	}
}

// TestPostgresBackupRestoreE2E drives on-demand backup and restore through the real Kubernetes
// adapter against a live cluster (ADR-0032): install the instance, attach an app, seed a row,
// BackupAddon (an in-cluster pg_dump Job), drop the row with an in-cluster Job, then RestoreAddon
// (an in-cluster pg_restore Job) and assert the row is back. The backup/restore Jobs run in-cluster,
// so — unlike attach — the engine calls need no port-forward: they create Jobs that reach the
// instance Service directly. It runs only when BURROW_TEST_KUBECONFIG points at a disposable
// cluster; it creates its own namespaces and cleans them up.
func TestPostgresBackupRestoreE2E(t *testing.T) {
	kubeconfig := os.Getenv("BURROW_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set BURROW_TEST_KUBECONFIG to a disposable cluster to run the Postgres backup/restore end-to-end test")
	}
	ctx := context.Background()

	cfg, err := kube.ConfigFromKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	found, err := kube.DetectCloudNativePG(ctx, client)
	if err != nil {
		t.Fatalf("detecting CloudNativePG: %v", err)
	}
	if !found.Ready {
		t.Skipf("the postgres add-on is a CloudNativePG Cluster (ADR-0066 §1) and no controller is running on this cluster; "+
			"install the operator first (kubectl apply --server-side -f %s)", kube.CNPGManifestURL(kube.CNPGVersion))
	}

	stamp := time.Now().UnixNano()
	appNS := fmt.Sprintf("burrow-pgbak-app-%d", stamp)
	addonNS := fmt.Sprintf("burrow-pgbak-addons-%d", stamp)
	for _, ns := range []string{appNS, addonNS} {
		if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
		ns := ns
		t.Cleanup(func() { _ = client.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })
	}

	k8s := kube.New(client, appNS).WithAddonNamespace(addonNS).WithDynamicClient(dyn)
	prov := kube.NewPostgresProvisioner(client, kube.AddonInstanceTarget(addonNS))
	engine, err := cp.New(cp.Deps{
		Kubernetes:          k8s,
		Database:            fake.NewDatabase(),
		Clock:               fake.NewClock(time.Now()),
		IDs:                 fake.NewIDs(),
		Resolver:            fake.NewResolver(),
		Credentials:         fake.NewCredentials(),
		DNS:                 fake.NewDNSFactory(),
		DatabaseProvisioner: prov,
		// As in the attach e2e and in burrowd: the engine resolves an environment to a namespace
		// and acts through that view, so it needs its own app namespace (ADR-0035 phase 2b).
		AppNamespace: appNS,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	const app = "shop"

	if _, err := engine.InstallAddon(ctx, cp.AddonPostgres, "", cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon postgres: %v", err)
	}
	waitForCond(t, 180*time.Second, "postgres ready", func() (bool, error) {
		return k8s.AddonReady(ctx, "burrow-postgres")
	})

	// One instance per environment, so the forward names this environment's own instance
	// (ADR-0067 §1).
	pgSelector := "burrow.cloud/addon=postgres,burrow.cloud/environment=" + cp.DefaultEnvironment

	// Attach the app (an admin-SQL op, so it goes through a port-forward like the other e2e).
	withPortForward(t, cfg, client, addonNS, pgSelector, 5432, "attach addon", func(localPort int) error {
		prov.WithAdminEndpoint(fmt.Sprintf("127.0.0.1:%d", localPort))
		_, aerr := engine.AttachAddon(ctx, cp.AddonPostgres, app, "", "")
		return aerr
	})

	// Seed a known row from inside the cluster using the app's DATABASE_URL.
	runSQLJob(t, ctx, client, appNS, app, "seed",
		`psql "$DATABASE_URL" -c "CREATE TABLE IF NOT EXISTS t (id int);"
psql "$DATABASE_URL" -c "INSERT INTO t VALUES (7);"`)

	// Back up: burrowd creates an in-cluster pg_dump Job — NO port-forward needed.
	res, err := engine.BackupAddon(ctx, cp.AddonPostgres, app, "", "")
	if err != nil {
		t.Fatalf("BackupAddon: %v", err)
	}
	if res.Backup.Status != cp.BackupCompleted {
		t.Fatalf("backup status = %q, want completed", res.Backup.Status)
	}

	// Drop the row in-cluster, proving restore actually puts it back.
	runSQLJob(t, ctx, client, appNS, app, "drop",
		`psql "$DATABASE_URL" -c "DELETE FROM t WHERE id = 7;"
test "$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM t WHERE id = 7;")" = "0"`)

	// Restore: burrowd creates an in-cluster pg_restore Job — again NO port-forward.
	if err := engine.RestoreAddon(ctx, cp.AddonPostgres, app, res.Backup.ID, "", true); err != nil {
		t.Fatalf("RestoreAddon: %v", err)
	}

	// Assert the row is back, in-cluster.
	runSQLJob(t, ctx, client, appNS, app, "assert",
		`psql "$DATABASE_URL" -tAc "SELECT id FROM t WHERE id = 7;" | grep -q 7`)
}

// secretValue reads one key out of a Secret in a named namespace — test-only introspection, so the
// test can assert that two environments were handed DIFFERENT connection strings (ADR-0067 §1). The
// value is compared and never printed: an assertion message that dumped it would put a live
// credential in the CI log, which is the thing ADR-0031 keeps out of every other surface.
func secretValue(t *testing.T, ctx context.Context, client kubernetes.Interface, ns, name, key string) string {
	t.Helper()
	sec, err := client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read secret %s/%s: %v", ns, name, err)
	}
	v, ok := sec.Data[key]
	if !ok {
		t.Fatalf("secret %s/%s has no %q key", ns, name, key)
	}
	return string(v)
}

// runSQLJob runs a one-shot psql Job in the app namespace that reads DATABASE_URL from the app's
// per-app Secret (via envFrom) and executes script. name disambiguates the Job. The Job uses the
// official postgres image's psql client and must complete successfully.
func runSQLJob(t *testing.T, ctx context.Context, client kubernetes.Interface, appNS, app, name, script string) {
	t.Helper()
	var backoff int32
	jobName := "pg-" + name
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: appNS},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "psql",
						Image:   "postgres:17-alpine",
						Command: []string{"sh", "-c", "set -e\n" + script},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cp.AppSecretName(app)},
							},
						}},
					}},
				},
			},
		},
	}
	if _, err := client.BatchV1().Jobs(appNS).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create %s job: %v", jobName, err)
	}
	waitForCond(t, 180*time.Second, jobName+" job succeeded", func() (bool, error) {
		j, err := client.BatchV1().Jobs(appNS).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if j.Status.Failed > 0 {
			return false, fmt.Errorf("%s job failed", jobName)
		}
		return j.Status.Succeeded > 0, nil
	})
}

// runRoundTripJob runs a one-shot psql Job in the app namespace that reads DATABASE_URL from the
// app's per-app Secret (via envFrom), creates a table, inserts a row, and reads it back. The Job
// uses the official postgres image's psql client and must complete successfully.
func runRoundTripJob(t *testing.T, ctx context.Context, client kubernetes.Interface, appNS, app string) {
	t.Helper()
	script := `set -e
psql "$DATABASE_URL" -c "CREATE TABLE IF NOT EXISTS t (id int);"
psql "$DATABASE_URL" -c "INSERT INTO t VALUES (42);"
psql "$DATABASE_URL" -tAc "SELECT id FROM t WHERE id = 42;" | grep -q 42`
	var backoff int32
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-roundtrip", Namespace: appNS},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "psql",
						Image:   "postgres:17-alpine",
						Command: []string{"sh", "-c", script},
						// DATABASE_URL comes from the app's per-app Secret, exactly as the app reads it.
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cp.AppSecretName(app)},
							},
						}},
					}},
				},
			},
		},
	}
	if _, err := client.BatchV1().Jobs(appNS).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create round-trip job: %v", err)
	}
	waitForCond(t, 180*time.Second, "round-trip job succeeded", func() (bool, error) {
		j, err := client.BatchV1().Jobs(appNS).Get(ctx, "pg-roundtrip", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if j.Status.Failed > 0 {
			return false, fmt.Errorf("round-trip job failed")
		}
		return j.Status.Succeeded > 0, nil
	})
}

// withPortForward retries fn under a freshly-established port-forward each attempt, until fn
// returns nil or the timeout elapses. Re-establishing the forward per attempt keeps the test
// robust if a single forward drops; the wrapped admin operation is idempotent, so re-running it is
// safe. desc names the operation in the failure message.
func withPortForward(t *testing.T, cfg *rest.Config, client kubernetes.Interface, ns, labelSelector string, containerPort int, desc string, fn func(localPort int) error) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for {
		last = func() error {
			localPort, stop, err := openPortForward(cfg, client, ns, labelSelector, containerPort)
			if err != nil {
				return err
			}
			defer stop()
			return fn(localPort)
		}()
		if last == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 90s on %s: %v", desc, last)
		}
		time.Sleep(3 * time.Second)
	}
}

// openPortForward forwards a local ephemeral port to containerPort on the first pod matching
// labelSelector in ns, returning the chosen local port and a stop function. This lets the
// out-of-cluster test reach an in-cluster Service that only resolves inside the cluster — the same
// trick `kubectl port-forward` uses. It returns an error (rather than failing the test) so the
// caller can retry.
func openPortForward(cfg *rest.Config, client kubernetes.Interface, ns, labelSelector string, containerPort int) (int, func(), error) {
	pods, err := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return 0, nil, fmt.Errorf("list pods %q in %s: %w", labelSelector, ns, err)
	}
	if len(pods.Items) == 0 {
		return 0, nil, fmt.Errorf("no pod matching %q in %s to port-forward", labelSelector, ns)
	}
	pod := pods.Items[0].Name

	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return 0, nil, fmt.Errorf("spdy round tripper: %w", err)
	}
	reqURL := client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", containerPort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, fmt.Errorf("new port forward: %w", err)
	}
	go func() { _ = fw.ForwardPorts() }()
	select {
	case <-readyCh:
	case <-time.After(15 * time.Second):
		close(stopCh)
		return 0, nil, fmt.Errorf("port-forward to %s/%s not ready within 15s", ns, pod)
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopCh)
		return 0, nil, fmt.Errorf("get forwarded ports: %w", err)
	}
	return int(ports[0].Local), func() { close(stopCh) }, nil
}

// waitForCond polls cond until it is true, erroring on a hard error or timeout.
func waitForCond(t *testing.T, timeout time.Duration, desc string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("waiting for %s: %v", desc, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, desc)
		}
		time.Sleep(2 * time.Second)
	}
}
