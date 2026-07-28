// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestAddonVolumesFindsClaimsAfterRemoval walks the whole path ADR-0064 §6 exists for: install a
// stateful add-on, remove it without --delete-data, and confirm the claim it left behind is still
// findable — attributed to the add-on it served and carrying its size.
func TestAddonVolumesFindsClaimsAfterRemoval(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	const addonNS = "burrow-addons"
	a := kube.New(client, ns).WithAddonNamespace(addonNS)

	spec := cp.AddonSpec{Type: cp.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428, StorageGi: 5}
	if _, err := a.DeployAddon(ctx, spec); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	if _, err := a.DeleteAddon(ctx, "burrow-logs", false); err != nil {
		t.Fatalf("DeleteAddon: %v", err)
	}

	vols, err := a.AddonVolumes(ctx)
	if err != nil {
		t.Fatalf("AddonVolumes: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("volumes = %+v, want the one claim the removal kept", vols)
	}
	v := vols[0]
	if v.Name != "burrow-logs" || v.Namespace != addonNS {
		t.Errorf("claim = %q in %q, want burrow-logs in %q", v.Name, v.Namespace, addonNS)
	}
	// Attribution comes off the add-on label the claim was created with, not off its name.
	if v.Addon != cp.AddonLogs {
		t.Errorf("addon = %q, want logs", v.Addon)
	}
	if v.Role != cp.AddonVolumeData || !v.ReinstallAdopts {
		t.Errorf("role = %q adopts = %v, want a data claim a reinstall adopts", v.Role, v.ReinstallAdopts)
	}
	// The requested size stands in until the cluster reports a bound capacity.
	if v.Size != "5Gi" {
		t.Errorf("size = %q, want the claim's 5Gi request", v.Size)
	}
	if v.CreatedAt.IsZero() {
		// The fake clientset stamps no creation timestamp, so this is informational only — the field
		// is populated from the object, not from ambient time.
		t.Log("fake clientset left CreationTimestamp zero")
	}
}

// TestAddonVolumesIdentifiesByLabelNotName asserts the identification rule: a claim is Burrow's
// because of its labels. A user's own claim sitting in the add-on namespace is not reported, and a
// claim whose NAME looks like an add-on's but carries no Burrow labels is not either — a name-prefix
// match would report both and invite someone to delete a claim Burrow never created.
func TestAddonVolumesIdentifiesByLabelNotName(t *testing.T) {
	ctx := context.Background()
	const addonNS = "burrow-addons"
	claim := func(name string, labels map[string]string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS, Labels: labels},
			Spec: corev1.PersistentVolumeClaimSpec{
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
	}
	client := fake.NewSimpleClientset(
		// Burrow's: managed-by plus the add-on type.
		claim("burrow-postgres", map[string]string{
			"app.kubernetes.io/managed-by": "burrow",
			"burrow.cloud/addon":           "postgres",
		}),
		// A user's own claim in the same namespace, unlabelled.
		claim("my-own-data", nil),
		// Named like an add-on's but not Burrow's — the case a prefix match gets wrong.
		claim("burrow-logs", map[string]string{"app.kubernetes.io/name": "burrow-logs"}),
		// Burrow-managed but not an add-on claim: no add-on label, no known add-on name.
		claim("burrow-something-else", map[string]string{"app.kubernetes.io/managed-by": "burrow"}),
	)
	a := kube.New(client, ns).WithAddonNamespace(addonNS)

	vols, err := a.AddonVolumes(ctx)
	if err != nil {
		t.Fatalf("AddonVolumes: %v", err)
	}
	if len(vols) != 1 || vols[0].Name != "burrow-postgres" || vols[0].Addon != cp.AddonPostgres {
		t.Fatalf("volumes = %+v, want only the labelled burrow-postgres claim", vols)
	}
}

// TestAddonVolumesReportsBackupClaim covers the Postgres dump volume, which survives every removal
// (ADR-0064 §4) and is therefore the claim most likely to be quietly accumulating. It is reported as
// a backup rather than as data: only one of the two comes back on reinstall.
func TestAddonVolumesReportsBackupClaim(t *testing.T) {
	ctx := context.Background()
	const addonNS = "burrow-addons"
	client := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cp.PostgresBackupVolume,
			Namespace: addonNS,
			// Deliberately WITHOUT the add-on label: this is a claim created before the label was
			// written, and it still has to be attributable.
			Labels: map[string]string{"app.kubernetes.io/managed-by": "burrow"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
		// A bound claim reports what was actually provisioned, which is what is being paid for.
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
		},
	})
	a := kube.New(client, ns).WithAddonNamespace(addonNS)

	vols, err := a.AddonVolumes(ctx)
	if err != nil {
		t.Fatalf("AddonVolumes: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("volumes = %+v, want the backup claim", vols)
	}
	v := vols[0]
	if v.Addon != cp.AddonPostgres || v.Role != cp.AddonVolumeBackup {
		t.Errorf("addon = %q role = %q, want postgres/backup", v.Addon, v.Role)
	}
	if v.ReinstallAdopts {
		t.Error("a reinstall does not adopt the backup claim")
	}
	// Provisioned capacity wins over the request: 20Gi is what the cluster gave and what is billed.
	if v.Size != "20Gi" {
		t.Errorf("size = %q, want the bound 20Gi capacity", v.Size)
	}
}

// TestAddonVolumesEmptyCluster asserts an add-on namespace with no claims returns no volumes and no
// error — a cluster that has removed nothing has nothing to report.
func TestAddonVolumesEmptyCluster(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), ns).WithAddonNamespace("burrow-addons")
	vols, err := a.AddonVolumes(context.Background())
	if err != nil {
		t.Fatalf("AddonVolumes: %v", err)
	}
	if len(vols) != 0 {
		t.Errorf("volumes = %+v, want none", vols)
	}
}
