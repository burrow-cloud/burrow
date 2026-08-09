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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// The CloudNativePG metadata the removal path reads, spelled here rather than exported from the
// package. A test that names the operator's labels independently is what notices the day the adapter
// starts selecting on something else — and selecting on nothing looks exactly like an instance with
// no disk, which is the failure these tests exist to catch.
const (
	cnpgClusterLabelName    = "cnpg.io/cluster"
	cnpgPVCStatusAnnotation = "cnpg.io/pvcStatus"
	cnpgPVCStatusReady      = "ready"
)

// cnpgInstance builds the objects CloudNativePG composes for one Burrow-authored `Cluster`: the
// custom resource, and the single data claim it owns. The claim carries what the operator really
// puts on it — its own cluster label, its `ready` status, the serial annotation, the labels the
// `Cluster` passes down through inheritedMetadata — and an owner reference back to the `Cluster`,
// which is the fact the whole removal path exists to deal with.
func cnpgInstance(name, env string) (*unstructured.Unstructured, *corev1.PersistentVolumeClaim) {
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.CNPGAPIGroup + "/v1",
		"kind":       "Cluster",
		"metadata": map[string]any{
			"name":      name,
			"namespace": cnpgTestNamespace,
			"uid":       "uid-" + name,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "burrow",
				"app.kubernetes.io/name":       name,
				"burrow.cloud/addon":           string(controlplane.AddonPostgres),
				"burrow.cloud/environment":     env,
			},
		},
		"spec": map[string]any{"instances": int64(1)},
	}}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-1",
			Namespace: cnpgTestNamespace,
			Labels: map[string]string{
				cnpgClusterLabelName:       name,
				"cnpg.io/pvcRole":          "PG_DATA",
				"cnpg.io/instanceName":     name + "-1",
				"burrow.cloud/addon":       string(controlplane.AddonPostgres),
				"burrow.cloud/environment": env,
			},
			Annotations: map[string]string{
				cnpgPVCStatusAnnotation: cnpgPVCStatusReady,
				"cnpg.io/nodeSerial":    "1",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         kube.CNPGAPIGroup + "/v1",
				Kind:               "Cluster",
				Name:               name,
				UID:                types.UID("uid-" + name),
				Controller:         ptrTo(true),
				BlockOwnerDeletion: ptrTo(true),
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
	}
	return cluster, claim
}

func ptrTo[T any](v T) *T { return &v }

// cnpgRemovalAdapter wires an adapter over a fake cluster holding the given typed objects and the
// given `Cluster` custom resources.
func cnpgRemovalAdapter(typed []runtime.Object, clusters ...*unstructured.Unstructured) (*kube.Adapter, *fake.Clientset, *dynamicfake.FakeDynamicClient) {
	client := fake.NewSimpleClientset(typed...)
	objs := make([]runtime.Object, 0, len(clusters))
	for _, c := range clusters {
		objs = append(objs, c)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpgClusterGVR: "ClusterList"}, objs...)
	return kube.New(client, "burrow").WithDynamicClient(dyn), client, dyn
}

// getClaim reads a claim back, failing the test when it is gone.
func getClaim(t *testing.T, client *fake.Clientset, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc, err := client.CoreV1().PersistentVolumeClaims(cnpgTestNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the claim %q back: %v", name, err)
	}
	return pvc
}

// claimGone asserts a claim is not there.
func claimGone(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	_, err := client.CoreV1().PersistentVolumeClaims(cnpgTestNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the claim %q survived: err = %v", name, err)
	}
}

// clusterGone asserts the custom resource is not there.
func clusterGone(t *testing.T, dyn dynamic.Interface, name string) {
	t.Helper()
	_, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the Cluster %q survived: err = %v", name, err)
	}
}

// TestDeleteAddonKeepsTheCloudNativePGDataClaim is ADR-0064 §1 under ADR-0066 §1's mechanism, and it
// is the central assertion of the slice. CloudNativePG stamps the `Cluster` on every claim it makes,
// so the operator's own default is that deleting the instance deletes every attached app's database.
// The record says the opposite, so the claim is DISOWNED before the `Cluster` goes.
func TestDeleteAddonKeepsTheCloudNativePGDataClaim(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres"
	cluster, claim := cnpgInstance(name, controlplane.DefaultEnvironment)
	a, client, dyn := cnpgRemovalAdapter([]runtime.Object{claim}, cluster)

	removal, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, false)
	if err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	clusterGone(t, dyn, name)
	if removal.DataDeleted {
		t.Error("a removal that was not asked to destroy the data reported that it did")
	}
	if removal.RetainedDataVolume != name+"-1" {
		t.Errorf("retained data volume = %q, want the operator's claim %q", removal.RetainedDataVolume, name+"-1")
	}

	kept := getClaim(t, client, name+"-1")
	for _, ref := range kept.OwnerReferences {
		if ref.Kind == "Cluster" && ref.Name == name {
			t.Fatal("the retained claim still names the Cluster as its owner — the garbage collector takes it as soon as the Cluster is gone, so 'the data was kept' is false")
		}
	}
}

// TestRetainedCloudNativePGClaimIsLabelledForTheListing is ADR-0064 §6. A claim nobody can find is a
// silent bill rather than a decision, and the operator's claims deliberately do not carry Burrow's
// selectable label while the `Cluster` owns them — a live instance's disk is not a retained volume.
// The label goes on at the moment the claim stops being the operator's, which is the moment it
// becomes the thing the listing is for.
func TestRetainedCloudNativePGClaimIsLabelledForTheListing(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres-staging"
	cluster, claim := cnpgInstance(name, "staging")
	a, _, _ := cnpgRemovalAdapter([]runtime.Object{claim}, cluster)

	if _, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, false); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}

	vols, err := a.AddonVolumes(ctx)
	if err != nil {
		t.Fatalf("AddonVolumes: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("AddonVolumes = %+v, want the one retained claim", vols)
	}
	got := vols[0]
	if got.Name != name+"-1" {
		t.Errorf("claim = %q, want %q", got.Name, name+"-1")
	}
	if got.Addon != controlplane.AddonPostgres || got.Role != controlplane.AddonVolumeData {
		t.Errorf("attributed as %s/%s, want postgres/data", got.Addon, got.Role)
	}
	if got.Environment != "staging" {
		t.Errorf("environment = %q, want staging — the listing has to say which environment's data this is", got.Environment)
	}
	if !got.ReinstallAdopts {
		t.Error("the retained data claim is reported as one a reinstall does not pick back up")
	}
}

// TestRetainedCloudNativePGClaimIsNotMarkedDetached pins the one place Burrow deliberately does NOT
// do what `kubectl cnpg destroy --keep-pvc` does. That command both disowns the claim and annotates
// it `detached`; CloudNativePG's own classifier treats any status it does not recognize — and it
// recognizes only `ready`, `initializing` and empty — as a claim to IGNORE, so a `detached` claim is
// never classified as dangling and a `Cluster` created again under the same name never reattaches to
// it. The data would be present and unreachable, which is not what ADR-0064 §1 promises: the promise
// is that reinstalling the add-on picks the data back up, not that the bytes still exist somewhere.
func TestRetainedCloudNativePGClaimIsNotMarkedDetached(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres"
	cluster, claim := cnpgInstance(name, controlplane.DefaultEnvironment)
	a, client, _ := cnpgRemovalAdapter([]runtime.Object{claim}, cluster)

	if _, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, false); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}

	kept := getClaim(t, client, name+"-1")
	if got := kept.Annotations[cnpgPVCStatusAnnotation]; got != cnpgPVCStatusReady {
		t.Errorf("%s = %q, want %q — an unrecognized status makes CloudNativePG ignore the claim, so a reinstall comes up beside the data instead of on it",
			cnpgPVCStatusAnnotation, got, cnpgPVCStatusReady)
	}
	if kept.Labels[cnpgClusterLabelName] != name {
		t.Errorf("the claim lost CloudNativePG's own cluster label (%q), which is how a re-created Cluster finds it", cnpgClusterLabelName)
	}
}

// TestDeleteAddonWithDeleteDataDestroysTheClusterAndItsClaims is the destructive branch: the caller
// typed the flag, typed the name back, and (where an object store is registered) already has a final
// backup. The volume goes, and the superuser Secret goes with it — ADR-0064 §Context's shared fate,
// which under this mechanism matters just as much: a retained credential beside a destroyed volume is
// only litter, but a destroyed credential beside a retained volume is data that cannot be opened.
func TestDeleteAddonWithDeleteDataDestroysTheClusterAndItsClaims(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres"
	cluster, claim := cnpgInstance(name, controlplane.DefaultEnvironment)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cnpgTestNamespace}}
	a, client, dyn := cnpgRemovalAdapter([]runtime.Object{claim, secret}, cluster)

	removal, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, true)
	if err != nil {
		t.Fatalf("DeleteAddon --delete-data: %v", err)
	}
	if !removal.DataDeleted {
		t.Error("--delete-data reported that no data volume was destroyed")
	}
	if removal.RetainedDataVolume != "" {
		t.Errorf("--delete-data reported a retained data volume %q", removal.RetainedDataVolume)
	}
	clusterGone(t, dyn, name)
	claimGone(t, client, name+"-1")
	if _, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the superuser Secret survived a destroyed volume: err = %v", err)
	}
}

// TestDeleteDataDestroysAnAlreadyDetachedCloudNativePGClaim is the case that decides whether the
// claims are deleted by NAME or left to the garbage collector. A claim retained by an earlier
// data-keeping removal has no owner reference at all, so deleting the `Cluster` would not take it:
// relying on ownership would report a destroyed volume to someone who is still being billed for it,
// and whose databases are still on it.
func TestDeleteDataDestroysAnAlreadyDetachedCloudNativePGClaim(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres"
	cluster, claim := cnpgInstance(name, controlplane.DefaultEnvironment)
	claim.OwnerReferences = nil // as a previous `addon remove` (no --delete-data) left it
	a, client, _ := cnpgRemovalAdapter([]runtime.Object{claim}, cluster)

	removal, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, true)
	if err != nil {
		t.Fatalf("DeleteAddon --delete-data: %v", err)
	}
	if !removal.DataDeleted {
		t.Error("--delete-data reported that no data volume was destroyed")
	}
	claimGone(t, client, name+"-1")
}

// TestDeleteAddonKeepsTheBackupClaimUnderEitherBranch is ADR-0064 §4: backups outlive the database
// they came from, and that is what makes §2's destructive path survivable at all. The mechanism has
// nothing to do with it — the dump claim is Burrow's, created on the ADR-0032 backup path, and no
// `Cluster` ever owned it — so the only thing that could go wrong is failing to REPORT it, which
// would leave allocated storage nobody knows about.
func TestDeleteAddonKeepsTheBackupClaimUnderEitherBranch(t *testing.T) {
	for _, deleteData := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep-data", true: "delete-data"}[deleteData], func(t *testing.T) {
			ctx := context.Background()
			name := "burrow-postgres"
			cluster, claim := cnpgInstance(name, controlplane.DefaultEnvironment)
			backup := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name:      controlplane.PostgresBackupVolume,
				Namespace: cnpgTestNamespace,
			}}
			a, client, _ := cnpgRemovalAdapter([]runtime.Object{claim, backup}, cluster)

			removal, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, deleteData)
			if err != nil {
				t.Fatalf("DeleteAddon: %v", err)
			}
			if removal.RetainedBackupVolume != controlplane.PostgresBackupVolume {
				t.Errorf("retained backup volume = %q, want %q", removal.RetainedBackupVolume, controlplane.PostgresBackupVolume)
			}
			getClaim(t, client, controlplane.PostgresBackupVolume)
		})
	}
}

// TestDeleteAddonReportsThisEnvironmentsBackupClaim asserts the claim reported is the one this
// instance's dumps are on (ADR-0067 §1). The environment is read off the `Cluster`'s own label rather
// than parsed back out of its name, and getting it wrong would tell an operator removing staging that
// production's dumps were what survived.
func TestDeleteAddonReportsThisEnvironmentsBackupClaim(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres-staging"
	cluster, claim := cnpgInstance(name, "staging")
	want, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, name)
	if err != nil {
		t.Fatal(err)
	}
	backups := []runtime.Object{
		claim,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: want, Namespace: cnpgTestNamespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: controlplane.PostgresBackupVolume, Namespace: cnpgTestNamespace}},
	}
	a, _, _ := cnpgRemovalAdapter(backups, cluster)

	removal, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, false)
	if err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}
	if removal.RetainedBackupVolume != want {
		t.Errorf("retained backup volume = %q, want this environment's claim %q", removal.RetainedBackupVolume, want)
	}
}

// TestDeleteAddonLeavesAnotherEnvironmentsInstanceAlone is ADR-0067 §1's isolation, asserted on the
// most destructive verb in the product. Two environments have no name they both resolve to, and a
// removal must act on exactly one of them — including its backup claim, which `--delete-data` is
// entitled to leave alone but never entitled to take from somebody else.
func TestDeleteAddonLeavesAnotherEnvironmentsInstanceAlone(t *testing.T) {
	ctx := context.Background()
	prod, prodClaim := cnpgInstance("burrow-postgres", controlplane.DefaultEnvironment)
	staging, stagingClaim := cnpgInstance("burrow-postgres-staging", "staging")
	stagingBackups, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, "staging")
	if err != nil {
		t.Fatal(err)
	}
	typed := []runtime.Object{
		prodClaim, stagingClaim,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: controlplane.PostgresBackupVolume, Namespace: cnpgTestNamespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: stagingBackups, Namespace: cnpgTestNamespace}},
	}
	a, client, dyn := cnpgRemovalAdapter(typed, prod, staging)

	if _, err := a.DeleteAddon(ctx, "burrow-postgres-staging", controlplane.AddonPostgres, true); err != nil {
		t.Fatalf("DeleteAddon staging --delete-data: %v", err)
	}

	claimGone(t, client, "burrow-postgres-staging-1")
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(ctx, "burrow-postgres", metav1.GetOptions{}); err != nil {
		t.Errorf("removing staging took production's Cluster: %v", err)
	}
	getClaim(t, client, "burrow-postgres-1")
	getClaim(t, client, controlplane.PostgresBackupVolume)
	getClaim(t, client, stagingBackups)
}

// TestDeleteAddonRefusesWhenTheClusterCannotBeRead is the slice's stated safety property: an instance
// you cannot READ is not an instance you may assume is gone. A refused or failed read has exactly the
// same shape as an absent object, and reading it as absence on this path means walking past a running
// database and deleting the registry row that named it.
func TestDeleteAddonRefusesWhenTheClusterCannotBeRead(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres"
	cluster, claim := cnpgInstance(name, controlplane.DefaultEnvironment)
	a, client, dyn := cnpgRemovalAdapter([]runtime.Object{claim}, cluster)
	dyn.PrependReactor("get", "clusters", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: kube.CNPGAPIGroup, Resource: "clusters"}, name,
			errors.New("clusters.postgresql.cnpg.io is forbidden"))
	})

	_, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, true)
	if err == nil {
		t.Fatal("a removal proceeded over a Cluster it could not read")
	}
	if errors.Is(err, controlplane.ErrNotFound) {
		t.Errorf("an unreadable Cluster was reported as an absent add-on: %v", err)
	}
	// Nothing was touched, which is what makes the refusal safe to retry.
	getClaim(t, client, name+"-1")
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Log("the Cluster is still there, as it must be")
	}
}

// TestDeleteAddonRefusesWhenTheClaimCannotBeDetached asserts the keep branch fails CLOSED. The claim
// is disowned first and the `Cluster` deleted second precisely so that a failure in between changes
// nothing: pressing on would delete the `Cluster` while the claim still names it as an owner, and the
// removal would report that it kept the data at the moment the garbage collector took it.
func TestDeleteAddonRefusesWhenTheClaimCannotBeDetached(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres"
	cluster, claim := cnpgInstance(name, controlplane.DefaultEnvironment)
	a, client, dyn := cnpgRemovalAdapter([]runtime.Object{claim}, cluster)
	client.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server said no")
	})

	if _, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, false); err == nil {
		t.Fatal("a removal deleted the Cluster after failing to detach the volume it promised to keep")
	}
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("the Cluster was deleted even though the claim was still owned by it: %v", err)
	}
}

// TestDeleteAddonRemovesAnInstanceWhoseClusterIsAlreadyGone covers the operator being uninstalled, the
// CRD no longer being served, or someone deleting the custom resource by hand. A dynamic client does
// no discovery, so all three arrive as a 404 — and in every one of them the disk holding every
// attached app's database is still allocated. Reporting ErrNotFound would leave it there with no
// registry row left to find it by.
func TestDeleteAddonRemovesAnInstanceWhoseClusterIsAlreadyGone(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres"
	_, claim := cnpgInstance(name, controlplane.DefaultEnvironment)
	a, client, _ := cnpgRemovalAdapter([]runtime.Object{claim})

	removal, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, false)
	if err != nil {
		t.Fatalf("DeleteAddon over an absent Cluster: %v", err)
	}
	if removal.RetainedDataVolume != name+"-1" {
		t.Errorf("retained data volume = %q, want %q", removal.RetainedDataVolume, name+"-1")
	}
	getClaim(t, client, name+"-1")
}

// TestDeleteAddonOnAnAbsentCloudNativePGInstanceIsNotFound: neither half of the instance is there, so
// there is genuinely nothing to remove. This is the ONLY shape that reports an absence — every other
// way of not finding the `Cluster` is either an error or an instance whose claims are still there.
func TestDeleteAddonOnAnAbsentCloudNativePGInstanceIsNotFound(t *testing.T) {
	a, _, _ := cnpgRemovalAdapter(nil)

	_, err := a.DeleteAddon(context.Background(), "burrow-postgres", controlplane.AddonPostgres, false)
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("DeleteAddon error = %v, want ErrNotFound", err)
	}
}

// TestDeleteAddonOnACloudNativePGInstanceMidCreation asserts a `Cluster` that has not produced a
// claim yet is removable, and reports honestly. An add-on is often removed precisely BECAUSE it never
// came up, and a removal that needed the instance to be healthy first would make exactly that case
// unremovable.
func TestDeleteAddonOnACloudNativePGInstanceMidCreation(t *testing.T) {
	ctx := context.Background()
	name := "burrow-postgres"
	cluster, _ := cnpgInstance(name, controlplane.DefaultEnvironment)
	a, _, dyn := cnpgRemovalAdapter(nil, cluster)

	removal, err := a.DeleteAddon(ctx, name, controlplane.AddonPostgres, true)
	if err != nil {
		t.Fatalf("DeleteAddon on a Cluster with no volume yet: %v", err)
	}
	clusterGone(t, dyn, name)
	if removal.DataDeleted {
		t.Error("a removal that found no volume reported destroying one")
	}
	if removal.RetainedDataVolume != "" {
		t.Errorf("retained data volume = %q, want none", removal.RetainedDataVolume)
	}
}

// TestDeleteAddonOnCloudNativePGWithNoDynamicClientRefuses: an adapter that cannot address custom
// resources cannot remove an instance made of one. It says so rather than reporting an absence, for
// the reason every other unreadable case does.
func TestDeleteAddonOnCloudNativePGWithNoDynamicClientRefuses(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), "burrow")

	_, err := a.DeleteAddon(context.Background(), "burrow-postgres", controlplane.AddonPostgres, false)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("DeleteAddon error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "burrow-postgres") {
		t.Errorf("the refusal does not name the instance: %v", err)
	}
}

// TestDeleteAddonOfAnotherTypeNeverTouchesTheCustomResource pins the routing from the other side:
// the TYPE is told, so removing a Deployment-backed add-on tears down a Deployment and a `Cluster`
// sharing the namespace is not read, not deleted, and not consulted.
func TestDeleteAddonOfAnotherTypeNeverTouchesTheCustomResource(t *testing.T) {
	ctx := context.Background()
	cluster, _ := cnpgInstance("burrow-postgres", controlplane.DefaultEnvironment)
	a, _, dyn := cnpgRemovalAdapter(nil, cluster)

	_, err := a.DeleteAddon(ctx, "burrow-logs", controlplane.AddonLogs, false)
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("DeleteAddon error = %v, want ErrNotFound — a Deployment-backed add-on must not find a Cluster", err)
	}
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(ctx, "burrow-postgres", metav1.GetOptions{}); err != nil {
		t.Errorf("removing another add-on deleted the Postgres Cluster: %v", err)
	}
}
