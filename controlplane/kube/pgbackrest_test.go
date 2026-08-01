// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// These tests cover ADR-0066 §3: what Burrow writes so that an instance archives its write-ahead log
// and can take a base backup, and what it deliberately does not write.
//
// The assertions worth reading first are TestStanzaCarriesTheCredentialByReference — the credential
// pair must never be a field of a custom resource, only a reference to two keys of a Secret — and
// TestStanzaRetentionIsBurrowsWindow, which is where ADR-0063 §3's "two retention policies cannot
// disagree" is settled by there only being one.

// pgBackRest resource identities, spelled again here rather than exported from the package: a test
// that names them independently is what notices the day the adapter starts writing a different one.
var (
	stanzaGVR          = schema.GroupVersionResource{Group: kube.PgBackRestAPIGroup, Version: "v1", Resource: "stanzas"}
	scheduledBackupGVR = schema.GroupVersionResource{Group: kube.CNPGAPIGroup, Version: "v1", Resource: "scheduledbackups"}
	backupGVR          = schema.GroupVersionResource{Group: kube.CNPGAPIGroup, Version: "v1", Resource: "backups"}
)

// pgBackRestControllerDeployment is the plugin's controller as its release manifest names and labels
// it. The label is `app`, not the `app.kubernetes.io/name` the operator uses — that difference is a
// fact about somebody else's manifest, which is why it is stated here as well as in the detector.
func pgBackRestControllerDeployment(namespace string, readyReplicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kube.PgBackRestControllerDeployment,
			Namespace: namespace,
			Labels:    map[string]string{"app": kube.PgBackRestControllerDeployment},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas},
	}
}

// archivingCluster stands up a fake cluster with CloudNativePG AND the pgBackRest plugin installed
// and running, which is what wiring an archive requires before anything is written.
func archivingCluster(objects ...runtime.Object) (*fake.Clientset, dynamic.Interface) {
	client := fake.NewSimpleClientset(append(objects,
		cnpgOperatorDeployment("cnpg-system", "ghcr.io/cloudnative-pg/cloudnative-pg:"+kube.CNPGVersion, 1),
		pgBackRestControllerDeployment("cnpg-system", 1))...)
	client.Resources = []*metav1.APIResourceList{
		{GroupVersion: "postgresql.cnpg.io/v1"},
		{GroupVersion: kube.PgBackRestAPIGroup + "/v1"},
		{GroupVersion: "apps/v1"},
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			cnpgClusterGVR:     "ClusterList",
			stanzaGVR:          "StanzaList",
			scheduledBackupGVR: "ScheduledBackupList",
			backupGVR:          "BackupList",
		})
	return client, dyn
}

// testArchive is a resolved destination for one environment: the shape the engine hands the adapter
// after reading the provider row and the credential pair.
func testArchive(env string, retentionDays int) *controlplane.ArchiveDestination {
	return &controlplane.ArchiveDestination{
		Provider: "backups",
		Config: controlplane.ObjectStoreConfig{
			Endpoint:      "https://s3.us-west-002.example.invalid",
			Region:        "us-west-002",
			Bucket:        "burrow-backups-x7f2",
			RetentionDays: retentionDays,
		},
		Credential: controlplane.ObjectStoreCredential{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "s3cr3t-value"},
		RepoPath:   controlplane.PgBackRestRepoPath(env),
	}
}

// getStanza reads the `Stanza` the adapter wrote.
func getStanza(t *testing.T, dyn dynamic.Interface, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := dyn.Resource(stanzaGVR).Namespace(cnpgTestNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the Stanza %q: %v", name, err)
	}
	return u
}

// firstRepository returns the one S3 repository of a `Stanza`.
func firstRepository(t *testing.T, stanza *unstructured.Unstructured) map[string]any {
	t.Helper()
	repos, found, err := unstructured.NestedSlice(stanza.Object, "spec", "stanzaConfiguration", "s3Repositories")
	if err != nil || !found || len(repos) == 0 {
		t.Fatalf("the Stanza has no s3Repositories: %v", stanza.Object)
	}
	repo, ok := repos[0].(map[string]any)
	if !ok {
		t.Fatalf("the Stanza's repository is not an object: %v", repos[0])
	}
	return repo
}

// TestDeployAddonWiresTheArchive asserts the whole of what an archiving install writes: the
// credential Secret, the `Stanza`, the `ScheduledBackup`, and the `Cluster` entry that makes
// PostgreSQL hand its write-ahead log to the plugin.
func TestDeployAddonWiresTheArchive(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
		t.Fatalf("DeployAddon with an archive: %v", err)
	}
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatal(err)
	}

	// The Cluster hands its write-ahead log to the plugin. isWALArchiver is the load-bearing field:
	// without it a base backup would have no archive behind it and no point-in-time recovery.
	plugins, found, err := unstructured.NestedSlice(getCluster(t, dyn, instance).Object, "spec", "plugins")
	if err != nil || !found || len(plugins) != 1 {
		t.Fatalf("the Cluster has no single plugin entry: %v", plugins)
	}
	entry, _ := plugins[0].(map[string]any)
	if entry["name"] != kube.PgBackRestPluginName {
		t.Errorf("plugin name = %v, want %q (not barman — ADR-0066 §3 declines it on licence)", entry["name"], kube.PgBackRestPluginName)
	}
	if entry["isWALArchiver"] != true {
		t.Errorf("isWALArchiver = %v, want true: without it there is no archive behind a base backup", entry["isWALArchiver"])
	}

	// The schedule exists, is handled by the plugin, and is owned by the Cluster so it does not
	// outlive the instance it fires at.
	sb, err := dyn.Resource(scheduledBackupGVR).Namespace(cnpgTestNamespace).
		Get(ctx, instance+"-schedule", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the ScheduledBackup: %v", err)
	}
	if method, _, _ := unstructured.NestedString(sb.Object, "spec", "method"); method != "plugin" {
		t.Errorf("ScheduledBackup method = %q, want plugin", method)
	}
	if owner, _, _ := unstructured.NestedString(sb.Object, "spec", "backupOwnerReference"); owner != "cluster" {
		t.Errorf("backupOwnerReference = %q, want cluster", owner)
	}
	if schedule, _, _ := unstructured.NestedString(sb.Object, "spec", "schedule"); schedule == "" {
		t.Error("the ScheduledBackup has no schedule")
	}
}

// TestStanzaCarriesTheCredentialByReference is the secret-handling invariant. The pair goes into a
// Secret and the custom resource carries a REFERENCE to two keys of it — never the values. A `Stanza`
// is readable by anything that can read custom resources in the namespace, and this is the credential
// that writes to every backup Burrow holds.
func TestStanzaCarriesTheCredentialByReference(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	archive := testArchive(controlplane.DefaultEnvironment, 30)

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, archive); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatal(err)
	}

	stanza := getStanza(t, dyn, instance)
	rendered, err := stanza.MarshalJSON()
	if err != nil {
		t.Fatalf("rendering the Stanza: %v", err)
	}
	for _, secret := range []string{archive.Credential.AccessKeyID, archive.Credential.SecretAccessKey} {
		if strings.Contains(string(rendered), secret) {
			t.Fatalf("the Stanza carries a credential VALUE; it must carry only a reference:\n%s", rendered)
		}
	}
	ref, found, err := unstructured.NestedMap(firstRepository(t, stanza), "secretRef")
	if err != nil || !found {
		t.Fatalf("the repository has no secretRef: %v", stanza.Object)
	}
	id, _ := ref["accessKeyId"].(map[string]any)
	if id["name"] != instance+"-pgbackrest" {
		t.Errorf("accessKeyId secret = %v, want the instance's own pgbackrest Secret", id["name"])
	}

	// And the Secret it points at actually holds the pair.
	sec, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance+"-pgbackrest", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the archive credential Secret: %v", err)
	}
	if string(sec.Data["access-key-id"]) != archive.Credential.AccessKeyID {
		t.Error("the archive credential Secret does not hold the access key id")
	}
}

// TestStanzaRetentionIsBurrowsWindow asserts the answer to "which retention is authoritative"
// (ADR-0063 §3, ADR-0066 §4). Burrow's declared window is written INTO pgBackRest's retention as a
// number of days, so there is one policy rather than two that agree until they do not — and the
// ARCHIVE retention is deliberately left unset, because expiring write-ahead log ahead of the backups
// that need it destroys point-in-time recovery within a window whose backups still list fine.
func TestStanzaRetentionIsBurrowsWindow(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	repo := firstRepository(t, getStanza(t, dyn, instance))
	policy, ok := repo["retentionPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("the repository has no retentionPolicy: %v", repo)
	}
	if policy["fullType"] != "time" {
		t.Errorf("fullType = %v, want time: Burrow's window is a number of DAYS, not a count of backups", policy["fullType"])
	}
	if full, _ := policy["full"].(int64); full != 30 {
		t.Errorf("full = %v, want the provider's 30-day window", policy["full"])
	}
	if _, present := policy["archive"]; present {
		t.Error("archive retention must stay unset: expiring WAL ahead of the backups needing it destroys PITR silently")
	}
}

// TestStanzaWithNoRetentionDeclaresNone asserts a provider with no declared window leaves pgBackRest
// with no retention at all rather than a number Burrow invented. An unbounded repository is a cost
// problem the operator can see; a repository expiring against a window nobody declared is a recovery
// problem they cannot.
func TestStanzaWithNoRetentionDeclaresNone(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testArchive(controlplane.DefaultEnvironment, 0)); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if _, present := firstRepository(t, getStanza(t, dyn, instance))["retentionPolicy"]; present {
		t.Error("a provider with no declared window must not produce a retention policy Burrow made up")
	}
}

// TestArchiveIsIsolatedPerEnvironment asserts one environment's repository is not addressable from
// another's. Each environment has its own instance (ADR-0067 §1), so each gets its own stanza, its
// own credential Secret, and its own repository PATH — two stanzas sharing a path would have each
// one's create-stanza looking at the other's backups.
func TestArchiveIsIsolatedPerEnvironment(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	for _, env := range []string{controlplane.DefaultEnvironment, "staging"} {
		if _, err := a.DeployAddon(ctx, postgresSpec(t), env, testArchive(env, 30)); err != nil {
			t.Fatalf("DeployAddon in %s: %v", env, err)
		}
	}

	paths := map[string]string{}
	for _, env := range []string{controlplane.DefaultEnvironment, "staging"} {
		instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, env)
		if err != nil {
			t.Fatal(err)
		}
		repo := firstRepository(t, getStanza(t, dyn, instance))
		path, _ := repo["repoPath"].(string)
		if path == "" {
			t.Fatalf("%s's repository has no path", env)
		}
		paths[env] = path
	}
	if paths[controlplane.DefaultEnvironment] == paths["staging"] {
		t.Errorf("both environments archive to %q; one environment's backups must be unreachable from another's", paths["staging"])
	}
	if !strings.Contains(paths["staging"], "staging") {
		t.Errorf("staging's repository path %q does not name the environment", paths["staging"])
	}
}

// TestDeployAddonWithoutAnArchiveWritesNoPlugin asserts an install on a cluster with no
// object-storage provider is byte-for-byte the instance it was before this existed: no plugin entry,
// no `Stanza`, no schedule, no credential Secret. The small self-hoster's floor does not rise because
// backups became possible for somebody else.
func TestDeployAddonWithoutAnArchiveWritesNoPlugin(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if _, found, _ := unstructured.NestedSlice(getCluster(t, dyn, instance).Object, "spec", "plugins"); found {
		t.Error("an instance with no destination must carry no plugin entry")
	}
	if _, err := dyn.Resource(stanzaGVR).Namespace(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a Stanza was written for an instance with no destination: %v", err)
	}
	if _, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance+"-pgbackrest", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("an archive credential Secret was written for an instance with no destination: %v", err)
	}
}

// TestReinstallAttachesTheArchiveToAnExistingCluster asserts the sequence a user actually takes:
// install Postgres, then decide they want backups. Re-running the install wires the existing
// instance to the newly-registered destination rather than requiring the component holding every
// app's data to be destroyed and rebuilt.
func TestReinstallAttachesTheArchiveToAnExistingCluster(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	spec := postgresSpec(t)

	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("first DeployAddon: %v", err)
	}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
		t.Fatalf("second DeployAddon: %v", err)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	plugins, found, _ := unstructured.NestedSlice(getCluster(t, dyn, instance).Object, "spec", "plugins")
	if !found || len(plugins) != 1 {
		t.Fatalf("re-installing with a destination did not attach the plugin: %v", plugins)
	}
}

// TestArchiveRefusesAPlaintextEndpoint asserts an http:// destination is refused rather than silently
// upgraded. pgBackRest reaches S3 over TLS only, so accepting it would produce an instance that comes
// up, an archive that never arrives, and a first symptom an hour later.
func TestArchiveRefusesAPlaintextEndpoint(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	archive := testArchive(controlplane.DefaultEnvironment, 30)
	archive.Config.Endpoint = "http://minio.example.invalid:9000"

	_, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, archive)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("DeployAddon with a plaintext endpoint = %v, want ErrInvalid", err)
	}
}

// TestArchiveRefusedWithoutThePlugin asserts an install that WANTS to archive on a cluster with no
// plugin refuses by name rather than writing a `Stanza` nothing will reconcile.
func TestArchiveRefusedWithoutThePlugin(t *testing.T) {
	ctx := context.Background()
	client, dyn := cnpgReadyCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	_, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testArchive(controlplane.DefaultEnvironment, 30))
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("DeployAddon without the plugin = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "burrow cluster postgres install") {
		t.Errorf("the refusal must name the operator step that fixes it: %v", err)
	}
}

// TestRemovalTakesTheArchiveConfigurationAndNoBackup asserts what a removal does to the archive: the
// schedule, the stanza and the credential go, because a schedule firing at an instance that is not
// there is noise and a credential nothing reads is exposure. The repository is in the object store
// and is not touched — ADR-0064's contract read onto the one copy that is genuinely outside the
// cluster.
func TestRemovalTakesTheArchiveConfigurationAndNoBackup(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if _, err := a.DeleteAddon(ctx, instance, controlplane.AddonPostgres, false); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}

	if _, err := dyn.Resource(stanzaGVR).Namespace(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the Stanza survived the removal: %v", err)
	}
	if _, err := dyn.Resource(scheduledBackupGVR).Namespace(cnpgTestNamespace).Get(ctx, instance+"-schedule", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the ScheduledBackup survived the removal and will keep firing at nothing: %v", err)
	}
	if _, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance+"-pgbackrest", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the archive credential survived the removal: %v", err)
	}
}

// TestDetectPgBackRest asserts the three states the plugin can be in are told apart, including the
// one the Present/Ready split exists for: CRDs served with no controller behind them, where a
// `Stanza` is accepted and reconciled by nothing.
func TestDetectPgBackRest(t *testing.T) {
	for _, tc := range []struct {
		name          string
		served        bool
		readyReplicas int32
		present       bool
		ready         bool
	}{
		{name: "absent"},
		{name: "orphaned CRDs", served: true, present: true},
		{name: "running", served: true, readyReplicas: 1, present: true, ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objects []runtime.Object
			if tc.readyReplicas > 0 || tc.served {
				objects = append(objects, pgBackRestControllerDeployment("cnpg-system", tc.readyReplicas))
			}
			client := fake.NewSimpleClientset(objects...)
			client.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}
			if tc.served {
				client.Resources = append(client.Resources, &metav1.APIResourceList{GroupVersion: kube.PgBackRestAPIGroup + "/v1"})
			}
			got, err := kube.DetectPgBackRest(context.Background(), client)
			if err != nil {
				t.Fatalf("DetectPgBackRest: %v", err)
			}
			if got.Present != tc.present || got.Ready != tc.ready {
				t.Errorf("pgbackrest = %+v, want present=%v ready=%v", got, tc.present, tc.ready)
			}
			if got.Pinned != kube.PgBackRestVersion {
				t.Errorf("pinned = %q, want %q even on a cluster with no plugin", got.Pinned, kube.PgBackRestVersion)
			}
		})
	}
}

// TestPgBackRestManifestURLIsPinned asserts the artifact an install applies is the publisher's own
// TAG and not a moving branch. A pin nobody asserts is a constant nobody notices moving, and this one
// decides which backup engine a cluster runs.
func TestPgBackRestManifestURLIsPinned(t *testing.T) {
	url := kube.PgBackRestManifestURL(kube.PgBackRestVersion)
	if !strings.Contains(url, "/v"+kube.PgBackRestVersion+"/") {
		t.Errorf("manifest URL %q does not carry the pinned version", url)
	}
	if strings.Contains(url, "/main/") {
		t.Errorf("manifest URL %q points at a branch rather than a release tag", url)
	}
	if strings.Contains(strings.ToLower(url), "barman") {
		t.Errorf("manifest URL %q is a barman plugin; ADR-0066 §3 declines it on licence", url)
	}
}
