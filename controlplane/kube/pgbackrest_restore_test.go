// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// These tests cover ADR-0066 §4's adapter half: the recovery `Cluster`, and the ordering around it.
//
// The two worth reading first are TestRestoreInstanceRecoversUnderTheInstancesOwnName — the decision
// every other consumer of a Postgres instance depends on, since AddonInstanceName names the Service,
// the Secret and the instance a backup is taken of — and TestRestoreInstanceRefusesAMismatchedStanza,
// which is the check that has to happen BEFORE anything is destroyed: recovering from a repository
// this instance never wrote to does not fail, it produces an empty database.

// recoveringCluster is archivingCluster with a reactor that gives every `Cluster` created in it a
// serving instance. A fake dynamic client reconciles nothing, so without this the adapter's wait for
// the recovered instance would be a wait for an operator that does not exist — and what the tests are
// about is what Burrow writes and in what order, not CloudNativePG's own work.
func recoveringCluster(objects ...runtime.Object) (*k8sfake.Clientset, dynamic.Interface) {
	client, dyn := archivingCluster(objects...)
	dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("create", "clusters", func(action k8stesting.Action) (bool, runtime.Object, error) {
		u, ok := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if ok {
			_ = unstructured.SetNestedField(u.Object, int64(1), "status", "readyInstances")
		}
		// Handled=false: the mutation stands and the tracker stores the object as usual.
		return false, nil, nil
	})
	return client, dyn
}

// instanceClaim is the data claim CloudNativePG composes for an instance, carrying the label the
// operator stamps on it — which is how a restore FINDS it, rather than by constructing the name.
func instanceClaim(instance string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controlplane.AddonDataVolumeName(controlplane.AddonPostgres, instance),
			Namespace: cnpgTestNamespace,
			Labels:    map[string]string{"cnpg.io/cluster": instance},
		},
	}
}

// restorableInstance stands up an archiving instance with its data claim present, which is the state
// a real cluster is in when someone asks for a physical restore.
func restorableInstance(t *testing.T) (*kube.Adapter, dynamic.Interface, *k8sfake.Clientset, string) {
	t.Helper()
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	client, dyn := recoveringCluster(instanceClaim(instance))
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	if _, err := a.DeployAddon(context.Background(), postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment),
		testArchive(controlplane.DefaultEnvironment, 30)); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	return a, dyn, client, instance
}

// TestRestoreInstanceRecoversUnderTheInstancesOwnName is the shape of the whole slice on the cluster
// side. CloudNativePG cannot recover in place, so a restore replaces the `Cluster` — and it does so
// under the name AddonInstanceName gives, because that name is what the additional Service, the
// superuser Secret, every DATABASE_URL, `addon remove` and `addon backup-instance` all resolve.
// A recovery that left a differently-named instance behind would leave every one of those pointing
// at something that is no longer live.
func TestRestoreInstanceRecoversUnderTheInstancesOwnName(t *testing.T) {
	ctx := context.Background()
	a, dyn, client, instance := restorableInstance(t)

	out, err := a.RestoreInstance(ctx, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		Instance:    testInstance(controlplane.DefaultEnvironment),
		BackupLabel: "20260801-020000F",
		Archive:     testArchive(controlplane.DefaultEnvironment, 30),
	})
	if err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}
	if out.Instance != instance {
		t.Errorf("recovered instance = %q, want the environment's own instance name %q", out.Instance, instance)
	}

	u := getCluster(t, dyn, instance)
	// The recovery bootstrap, and NOT initdb: they are alternatives, and a `Cluster` carrying both
	// would be rejected or would initialize empty.
	if _, found, _ := unstructured.NestedMap(u.Object, "spec", "bootstrap", "initdb"); found {
		t.Error("the recovered Cluster still carries bootstrap.initdb, which would initialize an empty database")
	}
	if got := nestedString(t, u, "spec", "bootstrap", "recovery", "recoveryTarget", "backupID"); got != "20260801-020000F" {
		t.Errorf("recoveryTarget.backupID = %q, want pgBackRest's label for the named backup", got)
	}
	source := nestedString(t, u, "spec", "bootstrap", "recovery", "source")
	externals, _, _ := unstructured.NestedSlice(u.Object, "spec", "externalClusters")
	if len(externals) != 1 {
		t.Fatalf("externalClusters = %d entries, want the one repository to recover from", len(externals))
	}
	ext := externals[0].(map[string]any)
	if ext["name"] != source {
		t.Errorf("bootstrap.recovery.source = %q does not name the externalClusters entry %q", source, ext["name"])
	}
	plugin := ext["plugin"].(map[string]any)
	if plugin["name"] != kube.PgBackRestPluginName {
		t.Errorf("the recovery source's plugin = %q, want the pgBackRest plugin", plugin["name"])
	}
	if params := plugin["parameters"].(map[string]any); params["stanzaRef"] != instance {
		t.Errorf("the recovery source's stanzaRef = %q, want this instance's stanza %q", params["stanzaRef"], instance)
	}
	// The recovered instance ARCHIVES. It is the environment's live database from the moment it comes
	// up, and one that does not archive is a database with no backups and nothing saying so.
	plugins, _, _ := unstructured.NestedSlice(u.Object, "spec", "plugins")
	archiving := false
	for _, p := range plugins {
		e := p.(map[string]any)
		if e["name"] == kube.PgBackRestPluginName && e["isWALArchiver"] == true {
			archiving = true
		}
	}
	if !archiving {
		t.Error("the recovered Cluster does not archive its write-ahead log")
	}
	// And the compatibility seam survives the restore: the additional managed Service still carries
	// the instance's name, so every DATABASE_URL and the provisioner's admin connection still resolve.
	services, _, _ := unstructured.NestedSlice(u.Object, "spec", "managed", "services", "additional")
	if len(services) != 1 {
		t.Fatalf("managed.services.additional = %d, want the instance's own Service", len(services))
	}
	svc := services[0].(map[string]any)["serviceTemplate"].(map[string]any)["metadata"].(map[string]any)
	if svc["name"] != instance {
		t.Errorf("the recovered instance's Service is %q, want %q", svc["name"], instance)
	}

	// The pre-restore claim is destroyed and reported. It cannot be retained: a claim left lying there
	// is what a `Cluster` of the same name is REATTACHED to instead of recovering, so keeping it would
	// silently cancel the restore.
	claim := controlplane.AddonDataVolumeName(controlplane.AddonPostgres, instance)
	if len(out.VolumesDestroyed) != 1 || out.VolumesDestroyed[0] != claim {
		t.Errorf("volumes destroyed = %v, want the pre-restore data claim %q", out.VolumesDestroyed, claim)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(cnpgTestNamespace).Get(ctx, claim, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the pre-restore data claim is still there (%v), so the recovery would have reattached to it", err)
	}
}

// TestRestoreInstanceRecoversToATime is the point-in-time half of ADR-0066 §4's table: the whole
// instance, to any point in the write-ahead-log window, which is the thing a logical dump cannot do.
func TestRestoreInstanceRecoversToATime(t *testing.T) {
	ctx := context.Background()
	a, dyn, _, instance := restorableInstance(t)

	if _, err := a.RestoreInstance(ctx, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		Instance:    testInstance(controlplane.DefaultEnvironment),
		TargetTime:  "2026-08-01T14:30:00Z",
		Archive:     testArchive(controlplane.DefaultEnvironment, 30),
	}); err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}
	u := getCluster(t, dyn, instance)
	if got := nestedString(t, u, "spec", "bootstrap", "recovery", "recoveryTarget", "targetTime"); got != "2026-08-01T14:30:00Z" {
		t.Errorf("recoveryTarget.targetTime = %q, want the requested instant", got)
	}
	if _, found, _ := unstructured.NestedString(u.Object, "spec", "bootstrap", "recovery", "recoveryTarget", "backupID"); found {
		t.Error("a point-in-time recovery also named a base backup, which is two targets")
	}
}

// TestRestoreInstanceWithNoTargetRecoversTheNewestState pins the deliberate ABSENCE of fields:
// CloudNativePG with no recoveryTarget restores the latest base backup and replays every segment
// after it, which is what "the newest state the repository holds" means.
func TestRestoreInstanceWithNoTargetRecoversTheNewestState(t *testing.T) {
	ctx := context.Background()
	a, dyn, _, instance := restorableInstance(t)

	if _, err := a.RestoreInstance(ctx, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		Instance:    testInstance(controlplane.DefaultEnvironment),
		Archive:     testArchive(controlplane.DefaultEnvironment, 30),
	}); err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}
	u := getCluster(t, dyn, instance)
	if _, found, _ := unstructured.NestedMap(u.Object, "spec", "bootstrap", "recovery", "recoveryTarget"); found {
		t.Error("a latest recovery carries a recoveryTarget, which pins it to a point instead")
	}
}

// TestRestoreInstanceRefusesAMismatchedStanza is the check that has to run BEFORE anything is
// destroyed. Recovering from a bucket this instance never archived to does not error — pgBackRest
// finds no backups for the stanza and the instance comes up EMPTY — so a destination that disagrees
// with the instance's own stanza is refused, with the live instance untouched.
func TestRestoreInstanceRefusesAMismatchedStanza(t *testing.T) {
	ctx := context.Background()
	a, dyn, client, instance := restorableInstance(t)

	elsewhere := testArchive(controlplane.DefaultEnvironment, 30)
	elsewhere.Config.Bucket = "somebody-elses-bucket"

	_, err := a.RestoreInstance(ctx, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		Instance:    testInstance(controlplane.DefaultEnvironment),
		Archive:     elsewhere,
	})
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("restore against a repository the instance never wrote to: got %v, want ErrInvalid", err)
	}
	// Nothing was destroyed: the instance and its disk are exactly where they were.
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{}); err != nil {
		t.Errorf("the live Cluster was removed by a refused restore: %v", err)
	}
	claim := controlplane.AddonDataVolumeName(controlplane.AddonPostgres, instance)
	if _, err := client.CoreV1().PersistentVolumeClaims(cnpgTestNamespace).Get(ctx, claim, metav1.GetOptions{}); err != nil {
		t.Errorf("the live data claim was removed by a refused restore: %v", err)
	}
}

// TestRestoreInstanceRefusesAnInstanceWithNoRepository: an instance installed with no object-storage
// provider registered has no `Stanza`, no plugin and no archive, so there is nothing in a repository
// to recover. The refusal says so by name and points at the per-app path, which needs no repository.
func TestRestoreInstanceRefusesAnInstanceWithNoRepository(t *testing.T) {
	ctx := context.Background()
	client, dyn := recoveringCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, testInstanceOf(postgresSpec(t), controlplane.DefaultEnvironment), nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	_, err := a.RestoreInstance(ctx, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		Instance:    testInstance(controlplane.DefaultEnvironment),
		Archive:     testArchive(controlplane.DefaultEnvironment, 30),
	})
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("restore of an instance with no repository: got %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "burrow addon restore postgres") {
		t.Errorf("the refusal does not point at the path that works without a repository: %v", err)
	}
}

// TestRestoreInstanceRecoversAnInstanceThatIsGone: "the instance is gone" is the case ADR-0066 §4's
// table gives physical recovery, so an environment whose `Cluster` was deleted by hand must still be
// recoverable. What it needs is the repository description, which outlives the instance.
func TestRestoreInstanceRecoversAnInstanceThatIsGone(t *testing.T) {
	ctx := context.Background()
	a, dyn, _, instance := restorableInstance(t)
	if err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Delete(ctx, instance, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("removing the Cluster by hand: %v", err)
	}

	out, err := a.RestoreInstance(ctx, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		Instance:    testInstance(controlplane.DefaultEnvironment),
		Archive:     testArchive(controlplane.DefaultEnvironment, 30),
	})
	if err != nil {
		t.Fatalf("RestoreInstance with no Cluster present: %v", err)
	}
	if out.Instance != instance {
		t.Errorf("recovered instance = %q, want %q", out.Instance, instance)
	}
	getCluster(t, dyn, instance)
}

// TestRestoreInstanceKeepsTheSuperuserSecret is the invariant a recovered instance is unusable
// without. CloudNativePG reconciles `burrow_admin` from this Secret (managed.roles), which is what
// makes the provisioner's admin connection work against a data directory whose own copy of that
// password is as old as the recovery target. Deleting or rotating it would lock Burrow out of the
// instance it just restored.
func TestRestoreInstanceKeepsTheSuperuserSecret(t *testing.T) {
	ctx := context.Background()
	a, _, client, instance := restorableInstance(t)
	before, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the superuser secret: %v", err)
	}

	if _, err := a.RestoreInstance(ctx, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		Instance:    testInstance(controlplane.DefaultEnvironment),
		Archive:     testArchive(controlplane.DefaultEnvironment, 30),
	}); err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}
	after, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the superuser secret is gone after a restore: %v", err)
	}
	if string(after.Data[kube.PostgresPasswordKey]) != string(before.Data[kube.PostgresPasswordKey]) {
		t.Error("the superuser password was rotated by a restore, which would lock Burrow out of the recovered instance")
	}
}

// TestRestoreInstanceKeepsTheRepositoryConfiguration: the `Stanza`, the schedule and the credential
// Secret describe the REPOSITORY, not the instance, and the repository is the thing the restore just
// read from. Removing them would leave a recovered instance archiving nowhere and taking no scheduled
// backups — and would delete the description of where every existing backup is.
func TestRestoreInstanceKeepsTheRepositoryConfiguration(t *testing.T) {
	ctx := context.Background()
	a, dyn, client, instance := restorableInstance(t)

	if _, err := a.RestoreInstance(ctx, controlplane.RestoreInstanceRequest{
		Environment: controlplane.DefaultEnvironment,
		Instance:    testInstance(controlplane.DefaultEnvironment),
		Archive:     testArchive(controlplane.DefaultEnvironment, 30),
	}); err != nil {
		t.Fatalf("RestoreInstance: %v", err)
	}
	getStanza(t, dyn, instance)
	if _, err := dyn.Resource(scheduledBackupGVR).Namespace(cnpgTestNamespace).Get(ctx, instance+"-schedule", metav1.GetOptions{}); err != nil {
		t.Errorf("the ScheduledBackup was removed by a restore: %v", err)
	}
	if _, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance+"-pgbackrest", metav1.GetOptions{}); err != nil {
		t.Errorf("the repository credential Secret was removed by a restore: %v", err)
	}
}
