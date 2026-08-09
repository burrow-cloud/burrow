// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/burrow-cloud/burrow/controlplane"
)

// This file is ADR-0082's write path onto a CloudNativePG `Cluster` that already exists: reading the
// shape an instance is in, and changing one property of it in place.
//
// IT PATCHES, IT DOES NOT RE-COMPOSE. Both changes go through a JSON merge patch naming exactly one
// path, for the reason attachArchiveToExistingCluster gives about the one other in-place edit Burrow
// makes: a `Cluster` Burrow wrote is not otherwise edited, and re-applying a freshly composed spec
// over a live database would carry every drift between this release and the one that created it —
// the image, the parameters, the archive wiring — into an operation the operator asked to change a
// single number.
//
// THE PATHS ARE THE ONES THE COMPOSITION WRITES, which is what makes ADR-0082's "the result matches
// what installing at that shape would have produced" checkable rather than asserted:
// postgresClusterSpec takes the standby count as an argument and writes it to `spec.instances`, and
// this writes to `spec.instances`. There is one spelling of each field and both sides use it.

// cnpgStandbyToInstances converts Burrow's standby count into CloudNativePG's `spec.instances`, which
// counts the primary as well (ADR-0081's Context: two things are called an instance and only one of
// them is the add-on's).
func cnpgStandbyToInstances(standbys int) int64 { return int64(standbys) + 1 }

// cnpgInstancesToStandbys is its inverse, and it floors at zero rather than reporting a negative
// count: a `Cluster` with `instances: 0` is not a shape Burrow writes, and answering "-1 standbys"
// would put an impossible number in front of an operator instead of the true one.
func cnpgInstancesToStandbys(instances int64) int {
	if instances < 1 {
		return 0
	}
	return int(instances - 1)
}

// AddonInstanceShape reports the configurable shape of environment env's add-on instance: how many
// standbys it runs and how big its data volume is (ADR-0082 §1).
//
// It reads the `Cluster`, which is the object those two facts live on. Reading them off the registry
// would report what an install once asked for; reading them off the claim would report what the
// storage provider granted, which for an expansion in progress is the OLD size — and the value an
// operator is comparing a new one against is the one that has been asked for.
func (a *Adapter) AddonInstanceShape(ctx context.Context, t controlplane.AddonType, env, instance string) (controlplane.AddonShape, error) {
	if t != controlplane.AddonPostgres {
		return controlplane.AddonShape{}, fmt.Errorf("kube: the %s add-on has no configurable shape: %w", t, controlplane.ErrInvalid)
	}
	if instance == "" {
		return controlplane.AddonShape{}, fmt.Errorf("kube: reading the shape of a %s instance in environment %q: no instance named: %w", t, env, controlplane.ErrInvalid)
	}
	name := instance
	u, found, err := a.getCNPGCluster(ctx, name)
	if err != nil {
		return controlplane.AddonShape{}, err
	}
	if !found {
		return controlplane.AddonShape{}, fmt.Errorf("kube: environment %q has no postgres instance %q to read the shape of — install one with `burrow addon install postgres --env %s`: %w",
			env, name, env, controlplane.ErrNotFound)
	}
	// A `Cluster` with no `instances` field is one the operator is defaulting for, which is a single
	// pod and therefore no standby. An unreadable field is the same answer for the same reason: the
	// shape reported has to be the shape a change is measured against, and guessing high would make a
	// scale-up look like a scale-down.
	instances, found, err := unstructured.NestedInt64(u.Object, "spec", "instances")
	if err != nil {
		return controlplane.AddonShape{}, fmt.Errorf("kube: reading the instance count of the CloudNativePG Cluster %q: %w", name, err)
	}
	if !found {
		instances = 1
	}
	size, _, err := unstructured.NestedString(u.Object, "spec", "storage", "size")
	if err != nil {
		return controlplane.AddonShape{}, fmt.Errorf("kube: reading the volume size of the CloudNativePG Cluster %q: %w", name, err)
	}
	return controlplane.AddonShape{Standbys: cnpgInstancesToStandbys(instances), Storage: size}, nil
}

// ConfigureAddonInstance changes one property of an existing instance in place (ADR-0082).
//
// EVERY REFUSAL HAS ALREADY BEEN MADE. The engine read the shape through AddonInstanceShape, decided
// whether this grows or shrinks, refused a volume shrink and held an unconfirmed scale-down — so
// what arrives here is a change that has been decided on and audited, and re-deciding it would be a
// second opinion that can disagree with the row already written.
//
// A patch names exactly one path, so an instance's image, parameters, roles, services and archive
// wiring are untouched by a change to its size — which is the difference between this and the
// remove-and-reinstall ADR-0082 exists to replace.
func (a *Adapter) ConfigureAddonInstance(ctx context.Context, req controlplane.ConfigureInstanceRequest) error {
	if req.Addon != controlplane.AddonPostgres {
		return fmt.Errorf("kube: the %s add-on has no configurable shape: %w", req.Addon, controlplane.ErrInvalid)
	}
	if req.Instance == "" {
		return fmt.Errorf("kube: configuring a %s instance in environment %q: no instance named: %w", req.Addon, req.Environment, controlplane.ErrInvalid)
	}
	name := req.Instance
	spec := map[string]any{}
	switch {
	case req.Standbys != nil:
		spec["instances"] = cnpgStandbyToInstances(*req.Standbys)
	case req.Storage != "":
		// `spec.storage.size` and nothing else under `storage`: a merge patch replaces the objects it
		// names, so patching the whole `storage` block would drop a storageClass or a pvcTemplate the
		// instance was created with.
		spec["storage"] = map[string]any{"size": req.Storage}
	default:
		return fmt.Errorf("kube: configuring the postgres instance %q was asked to change nothing: %w", name, controlplane.ErrInvalid)
	}
	clusters, err := a.cnpgClusters()
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return fmt.Errorf("kube: composing the configuration patch for the CloudNativePG Cluster %q: %w", name, err)
	}
	if _, err := clusters.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("kube: environment %q has no postgres instance %q to configure: %w", req.Environment, name, controlplane.ErrNotFound)
		}
		return fmt.Errorf("kube: configuring the CloudNativePG Cluster %q: %w", name, err)
	}
	return nil
}
