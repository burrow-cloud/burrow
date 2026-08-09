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
	k8stesting "k8s.io/client-go/testing"

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

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
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

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), archive); err != nil {
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

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
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

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 0)); err != nil {
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
		if _, err := a.DeployAddon(ctx, postgresSpec(t), env, testInstanceOf(postgresSpec(t), env), testArchive(env, 30)); err != nil {
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

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), nil); err != nil {
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

	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), nil); err != nil {
		t.Fatalf("first DeployAddon: %v", err)
	}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
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

	_, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), archive)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("DeployAddon with a plaintext endpoint = %v, want ErrInvalid", err)
	}
}

// TestArchiveSkippedWithoutThePluginIsStatedNotRefused asserts the posture the plugin gets, which is
// deliberately not the operator's. CloudNativePG is a REFUSAL because without it there is no database
// at all. The backup plugin is different: the database installs and serves every app on it, and what
// is missing is the archive — so refusing would take the database away to protect a backup, on a
// cluster where the plugin may not even be installable yet (its manifest needs cert-manager). The
// instance is created without archiving and the omission is STATED, so nothing claims a backup that
// is not happening.
func TestArchiveSkippedWithoutThePluginIsStatedNotRefused(t *testing.T) {
	ctx := context.Background()
	client, dyn := cnpgReadyCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	info, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30))
	if err != nil {
		t.Fatalf("DeployAddon without the plugin must succeed: %v", err)
	}
	if info.Warning == "" {
		t.Fatal("an instance created without archiving must say so; a silent omission is a backup nobody knows they do not have")
	}
	if !strings.Contains(info.Warning, "burrow cluster postgres install") {
		t.Errorf("the warning must name the operator step that fixes it: %q", info.Warning)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	// And the Cluster names no plugin: a spec referencing a plugin that is not installed would not
	// reconcile at all, which would take the database away by another route.
	if _, found, _ := unstructured.NestedSlice(getCluster(t, dyn, instance).Object, "spec", "plugins"); found {
		t.Error("the Cluster names a plugin the cluster does not have")
	}
	if _, err := dyn.Resource(stanzaGVR).Namespace(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a Stanza was written on a cluster with no plugin controller to reconcile it: %v", err)
	}
}

// TestArchiveRefusesARepointedDestination asserts an instance keeps the repository it was created
// against. Re-pointing a stanza at a different bucket would leave every backup already taken
// unreachable from the stanza that wrote them, while the instance carried on looking healthy.
func TestArchiveRefusesARepointedDestination(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	spec := postgresSpec(t)

	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
		t.Fatalf("first DeployAddon: %v", err)
	}
	other := testArchive(controlplane.DefaultEnvironment, 30)
	other.Config.Bucket = "a-different-bucket"
	_, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), other)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("re-installing against a different bucket = %v, want ErrInvalid", err)
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

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
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

// setStanzaBackupCount writes the count of full backups the plugin's own sidecar keeps on a
// `Stanza`'s status. It is the repository's account of itself and the only place Burrow can read
// whether a base backup exists without asking pgBackRest directly.
func setStanzaBackupCount(t *testing.T, dyn dynamic.Interface, name string, full int64) {
	t.Helper()
	stanza := getStanza(t, dyn, name)
	if err := unstructured.SetNestedField(stanza.Object, full, "status", "backupsCount", "Full"); err != nil {
		t.Fatalf("setting the Stanza backup count: %v", err)
	}
	if _, err := dyn.Resource(stanzaGVR).Namespace(cnpgTestNamespace).
		Update(context.Background(), stanza, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating the Stanza: %v", err)
	}
}

// TestNewInstanceAsksForItsBaseBackupImmediately is issue #467. An instance created with
// `immediate: false` archives write-ahead log from the moment it starts and has nothing to replay it
// onto until the schedule first fires — up to a day in which every artifact of a working backup
// exists and a restore is impossible. The window is at its widest right after creation, which is also
// when somebody is most likely to load data in.
func TestNewInstanceAsksForItsBaseBackupImmediately(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	info, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30))
	if err != nil {
		t.Fatalf("DeployAddon with an archive: %v", err)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)

	sb, err := dyn.Resource(scheduledBackupGVR).Namespace(cnpgTestNamespace).Get(ctx, instance+"-schedule", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the ScheduledBackup: %v", err)
	}
	immediate, found, err := unstructured.NestedBool(sb.Object, "spec", "immediate")
	if err != nil || !found || !immediate {
		t.Fatalf("ScheduledBackup immediate = %v (found %v): a fresh instance must not archive for a day with nothing to restore onto", immediate, found)
	}
	// And the install SAYS it asked, without claiming the backup exists: the request is a fact, the
	// backup is not one until the repository says so.
	if info.Backups.BaseBackup != controlplane.AddonBaseBackupRequested {
		t.Errorf("base backup state = %q, want %q", info.Backups.BaseBackup, controlplane.AddonBaseBackupRequested)
	}
}

// TestInstallReportsTheArchiveItActuallyWired is issue #466. The reported repository comes from the
// instance's own objects, and the state is a value a script can switch on rather than a sentence it
// has to parse.
func TestInstallReportsTheArchiveItActuallyWired(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	archive := testArchive(controlplane.DefaultEnvironment, 45)
	info, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), archive)
	if err != nil {
		t.Fatalf("DeployAddon with an archive: %v", err)
	}
	b := info.Backups
	if b == nil || b.State != controlplane.AddonBackupsArchiving {
		t.Fatalf("backups = %+v, want state %q", b, controlplane.AddonBackupsArchiving)
	}
	if b.Bucket != archive.Config.Bucket || b.RepoPath != archive.RepoPath {
		t.Errorf("reported repository = %s/%s, want %s/%s", b.Bucket, b.RepoPath, archive.Config.Bucket, archive.RepoPath)
	}
	if b.RetentionDays != 45 {
		t.Errorf("retention = %d days, want 45 — the window an operator would otherwise read out of a custom resource", b.RetentionDays)
	}
	if b.Provider != archive.Provider {
		t.Errorf("provider = %q, want %q", b.Provider, archive.Provider)
	}
	if b.Schedule == "" {
		t.Error("the base-backup schedule is not reported, so how much a restore could lose is unreadable")
	}
}

// TestInstallWithNoArchiveWarnsAndNamesTheFix asserts the second form issue #466 asks for: an
// instance that archives nowhere says so, and names the two commands that change it. Silence here is
// the failure that shows up months later.
func TestInstallWithNoArchiveWarnsAndNamesTheFix(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	info, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), nil)
	if err != nil {
		t.Fatalf("DeployAddon with no destination: %v", err)
	}
	b := info.Backups
	if b == nil || b.State != controlplane.AddonBackupsNone {
		t.Fatalf("backups = %+v, want state %q", b, controlplane.AddonBackupsNone)
	}
	for _, want := range []string{"burrow config provider add", "burrow addon install postgres"} {
		if !strings.Contains(b.Detail, want) {
			t.Errorf("the detail must name %q, at the moment the reader learns they need it: %q", want, b.Detail)
		}
	}
}

// TestInstallWithoutThePluginReportsNoArchiving is the case where the destination was registered and
// the instance still archives nowhere, because the cluster has no plugin. It is exactly the gap
// between intent and wiring the report exists to close: a provider was registered, and this instance
// does not archive.
func TestInstallWithoutThePluginReportsNoArchiving(t *testing.T) {
	ctx := context.Background()
	client, dyn := cnpgReadyCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	info, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30))
	if err != nil {
		t.Fatalf("DeployAddon without the plugin must succeed: %v", err)
	}
	if info.Backups == nil || info.Backups.State != controlplane.AddonBackupsNone {
		t.Fatalf("backups = %+v, want state %q even though a destination was resolved", info.Backups, controlplane.AddonBackupsNone)
	}
	if info.Backups.Detail != info.Warning {
		t.Errorf("the detail must be the install's own note rather than a second wording of it: %q vs %q", info.Backups.Detail, info.Warning)
	}
}

// TestReinstallDoesNotClaimABaseBackupItCouldNotAskFor covers the instance that already exists.
// CloudNativePG honours `immediate` only while a schedule has never been checked, so a re-run cannot
// ask for a first backup that way — and reporting one as requested would be the overstatement this
// surface exists to remove. It reports none, and names the command that takes one.
func TestReinstallDoesNotClaimABaseBackupItCouldNotAskFor(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	spec, archive := postgresSpec(t), testArchive(controlplane.DefaultEnvironment, 30)

	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), archive); err != nil {
		t.Fatalf("first DeployAddon: %v", err)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	setStanzaBackupCount(t, dyn, instance, 0)

	info, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), archive)
	if err != nil {
		t.Fatalf("re-running DeployAddon: %v", err)
	}
	if info.Backups.BaseBackup != controlplane.AddonBaseBackupNone {
		t.Fatalf("base backup state = %q, want %q", info.Backups.BaseBackup, controlplane.AddonBaseBackupNone)
	}
	if !strings.Contains(info.Backups.Detail, "burrow addon backup-instance postgres") {
		t.Errorf("an instance with archived write-ahead log and no base backup must be told how to take one: %q", info.Backups.Detail)
	}
}

// TestBaseBackupIsReportedPresentFromTheRepository asserts the positive case is read from the
// repository's own count rather than from Burrow having asked for a backup at some point.
func TestBaseBackupIsReportedPresentFromTheRepository(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	spec, archive := postgresSpec(t), testArchive(controlplane.DefaultEnvironment, 30)

	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), archive); err != nil {
		t.Fatalf("first DeployAddon: %v", err)
	}
	instance, _ := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	setStanzaBackupCount(t, dyn, instance, 3)

	info, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), archive)
	if err != nil {
		t.Fatalf("re-running DeployAddon: %v", err)
	}
	if info.Backups.BaseBackup != controlplane.AddonBaseBackupPresent {
		t.Errorf("base backup state = %q, want %q", info.Backups.BaseBackup, controlplane.AddonBaseBackupPresent)
	}
}

// TestBackupsAreNotClaimedWhenTheWiringCannotBeRead is the honesty case. A build with no dynamic
// client cannot read a `Cluster` back at all, and the answer is "not confirmed" rather than either of
// the two confident ones — a report that guesses is worse than the silence it replaced.
func TestBackupsAreNotClaimedWhenTheWiringCannotBeRead(t *testing.T) {
	ctx := context.Background()
	client, dyn := archivingCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	// burrowd installed before the Postgres add-on existed holds no grant on postgresql.cnpg.io, so
	// this read is refused on a cluster that has not been upgraded. It is the one case where Burrow
	// genuinely cannot tell whether the instance archives.
	dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "clusters",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: kube.CNPGAPIGroup, Resource: "clusters"}, "", errors.New("no grant"))
		})

	info, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), testArchive(controlplane.DefaultEnvironment, 30))
	if err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	if info.Backups == nil || info.Backups.State != controlplane.AddonBackupsUnknown {
		t.Fatalf("backups = %+v, want state %q rather than a confident answer Burrow cannot stand behind", info.Backups, controlplane.AddonBackupsUnknown)
	}
	if info.Backups.Detail == "" {
		t.Error("an unconfirmed report must say what could not be read")
	}
}

// TestAddonsWithNoBackupPathSaySo is the rest of issue #466: the same silence applied to every other
// add-on type. A metrics store holding samples on a volume nothing copies is a fact worth stating at
// install time rather than after a node goes.
func TestAddonsWithNoBackupPathSaySo(t *testing.T) {
	for _, tt := range []struct {
		addon controlplane.AddonType
		want  string
	}{
		{controlplane.AddonCache, "rebuildable"},
		{controlplane.AddonLogs, "data volume"},
		{controlplane.AddonMetrics, "data volume"},
	} {
		b := controlplane.TypeBackups(tt.addon)
		if b == nil || b.State != controlplane.AddonBackupsNone {
			t.Fatalf("%s backups = %+v, want state %q", tt.addon, b, controlplane.AddonBackupsNone)
		}
		if !strings.Contains(b.Detail, tt.want) {
			t.Errorf("%s detail = %q, want it to mention %q", tt.addon, b.Detail, tt.want)
		}
	}
	if controlplane.TypeBackups(controlplane.AddonPostgres) != nil {
		t.Error("postgres must report from the instance, not from the catalog")
	}
}
