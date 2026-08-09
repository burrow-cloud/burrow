// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/burrow-cloud/burrow/controlplane"
)

// These tests cover ADR-0082's adapter half: reading the shape a `Cluster` is in, and changing one
// property of it in place.
//
// TestConfigureAddonInstanceLeavesEverythingElseAlone is the one that matters. The whole argument for
// `addon config` over remove-and-reinstall is that changing a number does not touch the database, so
// a patch that carried a re-composed spec — a different image, different parameters, an archive
// wiring from a later release — would be the operation the record exists to replace, wearing its name.

// configurableCluster is a `Cluster` in the shape Burrow writes one: an instance count, a volume
// size, and the neighbours a configuration change must not disturb.
func configurableCluster(name string, instances int64, size string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": name, "namespace": cnpgTestNamespace},
		"spec": map[string]any{
			"instances": instances,
			"imageName": "ghcr.io/cloudnative-pg/postgresql:17.2",
			"storage": map[string]any{
				"size": size,
				// A storage class the instance was created with. A patch that replaced the whole
				// `storage` object rather than its `size` would drop it, and the claim the operator
				// then asks for is one on whatever the cluster's default class is.
				"storageClass": "do-block-storage",
			},
			"postgresql": map[string]any{"parameters": map[string]any{"shared_buffers": "128MB"}},
		},
	}}
}

// readCluster reads a `Cluster` back out of the fake dynamic client.
func readCluster(t *testing.T, dyn dynamic.Interface, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the Cluster %q back: %v", name, err)
	}
	return u
}

// TestAddonInstanceShapeReadsTheCluster: standbys are `spec.instances` MINUS the primary, which is
// the translation ADR-0081's Context insists on — two things are called an instance and only one of
// them is the add-on's.
func TestAddonInstanceShapeReadsTheCluster(t *testing.T) {
	name, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	a, _, _ := cnpgRemovalAdapter(nil, configurableCluster(name, 3, "20Gi"))

	shape, err := a.AddonInstanceShape(context.Background(), controlplane.AddonPostgres, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
	if err != nil {
		t.Fatalf("AddonInstanceShape: %v", err)
	}
	if shape.Standbys != 2 {
		t.Errorf("standbys = %d, want 2: three CloudNativePG instances is a primary and two standbys", shape.Standbys)
	}
	if shape.Storage != "20Gi" {
		t.Errorf("storage = %q, want 20Gi", shape.Storage)
	}
}

// TestAddonInstanceShapeIsNotFoundWithoutAnInstance: an environment with no Postgres has no shape,
// and that is ErrNotFound rather than a zero-valued answer a change could be measured against.
func TestAddonInstanceShapeIsNotFoundWithoutAnInstance(t *testing.T) {
	a, _, _ := cnpgRemovalAdapter(nil)

	_, err := a.AddonInstanceShape(context.Background(), controlplane.AddonPostgres, controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestConfigureAddonInstanceSetsTheInstanceCount, and it asserts the value the COMPOSITION would have
// written at the same standby count — which is ADR-0082's "the result matches what installing at that
// shape would have produced", checked rather than asserted.
func TestConfigureAddonInstanceSetsTheInstanceCount(t *testing.T) {
	name, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	a, _, dyn := cnpgRemovalAdapter(nil, configurableCluster(name, 1, "20Gi"))
	standbys := 2

	if err := a.ConfigureAddonInstance(context.Background(), controlplane.ConfigureInstanceRequest{
		Addon: controlplane.AddonPostgres, Environment: controlplane.DefaultEnvironment, Instance: testInstance(controlplane.DefaultEnvironment), Standbys: &standbys,
	}); err != nil {
		t.Fatalf("ConfigureAddonInstance: %v", err)
	}
	got, found, err := unstructured.NestedInt64(readCluster(t, dyn, name).Object, "spec", "instances")
	if err != nil || !found {
		t.Fatalf("reading spec.instances: found=%v err=%v", found, err)
	}
	if got != 3 {
		t.Errorf("spec.instances = %d, want 3 (a primary and two standbys)", got)
	}
}

// TestConfigureAddonInstanceLeavesEverythingElseAlone: a change to one number must not carry a
// re-composed spec over a live database.
func TestConfigureAddonInstanceLeavesEverythingElseAlone(t *testing.T) {
	name, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	a, _, dyn := cnpgRemovalAdapter(nil, configurableCluster(name, 1, "20Gi"))

	if err := a.ConfigureAddonInstance(context.Background(), controlplane.ConfigureInstanceRequest{
		Addon: controlplane.AddonPostgres, Environment: controlplane.DefaultEnvironment, Instance: testInstance(controlplane.DefaultEnvironment), Storage: "50Gi",
	}); err != nil {
		t.Fatalf("ConfigureAddonInstance: %v", err)
	}
	spec := readCluster(t, dyn, name).Object["spec"].(map[string]any)
	storage := spec["storage"].(map[string]any)
	if storage["size"] != "50Gi" {
		t.Errorf("spec.storage.size = %v, want 50Gi", storage["size"])
	}
	// The three neighbours a careless patch drops, each one silently: the class the claim is made on,
	// the image the instance runs, and the tuning it was sized against.
	if storage["storageClass"] != "do-block-storage" {
		t.Errorf("spec.storage.storageClass = %v, want it untouched: a volume on a different class is a different volume", storage["storageClass"])
	}
	if spec["imageName"] != "ghcr.io/cloudnative-pg/postgresql:17.2" {
		t.Errorf("spec.imageName = %v, want it untouched", spec["imageName"])
	}
	if spec["postgresql"] == nil {
		t.Error("spec.postgresql was dropped by a change to the volume size")
	}
}

// TestConfigureAddonInstanceIsNotFoundWithoutAnInstance: nothing is written into existence as a side
// effect of a scale.
func TestConfigureAddonInstanceIsNotFoundWithoutAnInstance(t *testing.T) {
	a, _, _ := cnpgRemovalAdapter(nil)
	standbys := 1

	err := a.ConfigureAddonInstance(context.Background(), controlplane.ConfigureInstanceRequest{
		Addon: controlplane.AddonPostgres, Environment: controlplane.DefaultEnvironment, Instance: testInstance(controlplane.DefaultEnvironment), Standbys: &standbys,
	})
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}
