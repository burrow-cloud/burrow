// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/burrow-cloud/burrow/controlplane"
)

// This file is ADR-0064's removal contract, expressed over a CloudNativePG `Cluster` (ADR-0066 §1).
// The contract does not change because the mechanism did: removal KEEPS the data and names the claim
// it kept, `--delete-data` is the separate ask that destroys it, and the backup claim survives both.
//
// WHAT IS DIFFERENT IS WHO OWNS THE DISK, and it is the whole of the work here. Under ADR-0031 the
// claim is Burrow's: it is created by Burrow, named after the instance, and simply not deleted. Under
// CloudNativePG the claims are the OPERATOR's — it composes them from the `Cluster`, names them
// `<cluster>-1`, and (verified in CNPG's own PVC builder, which calls the cluster's
// SetInheritedDataAndOwnership on every claim it makes) stamps the `Cluster` on each one as an owner
// reference. Deleting the `Cluster` therefore hands the claims to Kubernetes' garbage collector.
//
// That default is the exact inverse of ADR-0064 §1. A mechanism swap that silently converted "remove
// the add-on" from "keep every attached app's database" into "destroy every attached app's database"
// would be the record's central failure reintroduced by the back door — with no flag typed, no name
// typed back, and no final backup taken. So Burrow does not adopt CNPG's default: it DETACHES the
// claims from the `Cluster` before deleting it, which is the same edit `kubectl cnpg destroy
// --keep-pvc` makes and the reason that flag exists at all.

// CloudNativePG's own label and annotation namespace. It is NOT the API group: the group is
// `postgresql.cnpg.io` (CNPGAPIGroup, what the custom resource lives under) while the metadata the
// operator stamps on the resources it composes is under the shorter `cnpg.io`. Spelling them apart
// is deliberate — writing the group here would select nothing and a removal that selects nothing
// looks exactly like a removal with nothing to do.
const (
	cnpgMetadataNamespace = "cnpg.io"
	// cnpgClusterLabel is on every resource CloudNativePG composes for a `Cluster`, naming the
	// cluster it belongs to. It is how the claims are FOUND rather than named: constructing
	// `<instance>-1` would work for the single-instance `Cluster` Burrow authors today and would
	// quietly retain half a volume group the day it does not.
	cnpgClusterLabel = cnpgMetadataNamespace + "/cluster"
)

// deletePostgresCluster tears down a CloudNativePG-backed Postgres instance (ADR-0066 §1) under
// ADR-0064's contract: the workload goes, the data claims stay unless deleteData was asked for, and
// this environment's backup claim survives either way.
//
// THE ORDER OF THE TWO BRANCHES IS THE SAFETY, and they are deliberately mirror images:
//
//   - Keeping the data detaches the claims FIRST and deletes the `Cluster` second, so the garbage
//     collector never sees a claim it is entitled to collect.
//   - Destroying the data deletes the `Cluster` FIRST and the claims second, so the operator is not
//     still reconciling an instance while its volume is being removed from under it — and so a claim
//     an earlier data-keeping removal already detached, which no owner reference would take, is
//     destroyed by name rather than left behind by a garbage collector with nothing to act on.
//
// An instance that cannot be READ is not an instance that may be assumed gone. A `Cluster` this
// cannot get an answer about — the API refused, the grant is missing, the connection failed — aborts
// with nothing touched and the registry row intact, rather than degrading to "there is no such
// add-on" and deleting the record of a database that is still running.
func (a *Adapter) deletePostgresCluster(ctx context.Context, name string, deleteData bool) (controlplane.AddonRemoval, error) {
	removal := controlplane.AddonRemoval{Namespace: a.addonNamespace}

	cluster, clusterFound, err := a.readCNPGClusterForRemoval(ctx, name)
	if err != nil {
		return removal, err
	}
	claims, err := a.cnpgClusterClaims(ctx, name)
	if err != nil {
		return removal, err
	}
	// The instance exists if EITHER half of it does. A `Cluster` with no claim yet is an instance
	// mid-creation; claims with no `Cluster` are an instance whose custom resource was deleted by
	// hand, or whose operator (and with it the CRD) has been uninstalled — in both of those the disk
	// holding every attached app's database is still allocated, and a removal that reported
	// ErrNotFound would leave it there with no registry row left to find it by.
	if !clusterFound && len(claims) == 0 {
		return removal, fmt.Errorf("kube: addon %q: %w", name, controlplane.ErrNotFound)
	}

	// Which environment this instance serves, taken from the object's own labels rather than parsed
	// back out of its name — the same reading the Deployment-backed path takes, and the reason the
	// backup claim it reports is this environment's rather than the compiled-in default's
	// (ADR-0067 §1). The claims carry it too, inherited from the `Cluster` (inheritedMetadata), which
	// is what keeps the answer available when the `Cluster` itself is already gone.
	env := controlplane.DefaultEnvironment
	switch {
	case clusterFound:
		env = addonEnvironment(cluster.GetLabels())
	case len(claims) > 0:
		env = addonEnvironment(claims[0].Labels)
	}

	if !deleteData {
		for i := range claims {
			if err := a.retainCNPGClaim(ctx, &claims[i], name, env); err != nil {
				return removal, err
			}
		}
	}
	if clusterFound {
		if err := a.deleteCNPGCluster(ctx, name); err != nil {
			return removal, err
		}
	}
	if deleteData {
		// Explicitly, by name, rather than on the strength of the owner reference. A claim retained by
		// an earlier data-keeping removal has no owner at all — and if it were left to the collector,
		// `--delete-data` would report a destroyed volume while the volume, and every database in it,
		// were still sitting there.
		for i := range claims {
			if err := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).
				Delete(ctx, claims[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return removal, fmt.Errorf("kube: deleting addon volume %q: %w", claims[i].Name, err)
			}
		}
		removal.DataDeleted = len(claims) > 0
		// The superuser Secret shares the volume's fate, never split from it (ADR-0064 §Context). It
		// is Burrow's own Secret, named after the instance — CloudNativePG's generated ones carry the
		// `Cluster` as their owner and go with it.
		_ = a.client.CoreV1().Secrets(a.addonNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	} else if len(claims) > 0 {
		// The `Cluster` Burrow authors declares ONE instance and no separate WAL or tablespace
		// storage, so CloudNativePG composes exactly one claim for it and that is the one named here.
		// Every claim found is retained and labelled regardless of how many there are, so ADR-0064
		// §6's listing — which is plural, and is the surface a user actually reclaims from — stays
		// complete even if a future spec grows the volume group past what this field can say.
		removal.RetainedDataVolume = claims[0].Name
	}

	// The backup claim ALWAYS survives, `--delete-data` included (ADR-0064 §4): backups outliving the
	// database they came from is the whole point of taking them, and it is what makes the destructive
	// path survivable. It is a claim Burrow creates and owns on the ADR-0032 dump path, unrelated to
	// the `Cluster`, so nothing above could have reached it — it is reported so the operator knows
	// the storage is still allocated and can reclaim it deliberately.
	if claim, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, env); err == nil {
		if _, err := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).
			Get(ctx, claim, metav1.GetOptions{}); err == nil {
			removal.RetainedBackupVolume = claim
		}
	}
	return removal, nil
}

// readCNPGClusterForRemoval reads the `Cluster` a removal is about to act on, and — unlike
// getCNPGCluster, the read every OTHER path uses — refuses to fold a failure into an absence.
//
// getCNPGCluster answers "is there a Cluster here" for a readiness probe and a status sweep, where a
// forbidden read on an un-upgraded burrowd is honestly reported as "there is none": that cluster
// cannot have one, because Burrow could not have created it. A REMOVAL cannot reason that way. It
// runs on a cluster where Burrow did create one, the registry says so, and every way of failing to
// read it — the grant is missing, the API server refused, the connection dropped — has the same
// consequence if it is read as absence: the removal proceeds past the object holding every attached
// app's database without touching it, and the registry row that named it is deleted. The instance
// becomes unreachable, un-listable and still running.
//
// So only a genuine 404 is an absence. That one is ambiguous in its own right — a dynamic client does
// no discovery, so "no such object" and "the CRD is not served because the operator was uninstalled"
// arrive identically — and it is handled by the caller looking at the claims, which are core
// resources and answer either way.
func (a *Adapter) readCNPGClusterForRemoval(ctx context.Context, name string) (*unstructured.Unstructured, bool, error) {
	if a.dynamic == nil {
		return nil, false, fmt.Errorf("kube: addon %q is backed by a CloudNativePG %s and no dynamic client is "+
			"wired, so it cannot be read or removed: %w", name, cnpgClusterKind, controlplane.ErrInvalid)
	}
	u, err := a.dynamic.Resource(cnpgClusterGVR).Namespace(a.addonNamespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case apierrors.IsForbidden(err):
		return nil, false, fmt.Errorf("kube: burrowd is not permitted to read the CloudNativePG %s %q, so this "+
			"removal cannot tell whether the instance is still running and refuses rather than assume it is gone. "+
			"Re-run `burrow install` to bring the control plane's grants up to date: %w",
			cnpgClusterKind, name, err)
	case err != nil:
		return nil, false, fmt.Errorf("kube: reading the CloudNativePG %s %q: %w", cnpgClusterKind, name, err)
	}
	return u, true, nil
}

// deleteCNPGCluster deletes the custom resource. Everything CloudNativePG composed from it and still
// owns — the pods, the jobs, the services (including the additional managed service carrying the
// instance's name), and the operator's own generated Secrets — goes with it by ordinary garbage
// collection. Only what this file deliberately disowned first survives.
//
// AlreadyGone is success. A removal retried after a partial failure must reach the end.
func (a *Adapter) deleteCNPGCluster(ctx context.Context, name string) error {
	err := a.dynamic.Resource(cnpgClusterGVR).Namespace(a.addonNamespace).
		Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting the CloudNativePG %s %q: %w", cnpgClusterKind, name, err)
	}
	return nil
}

// cnpgClusterClaims lists the PersistentVolumeClaims CloudNativePG composed for the named cluster, by
// the label the operator stamps on them. Sorted by name, so what a removal reports and the order it
// acts in are both deterministic.
//
// Selecting by LABEL rather than by constructed name matters for the same reason it does elsewhere in
// this package: `<instance>-1` is the right answer for the single-instance `Cluster` Burrow authors
// today, and the day a spec adds a replica or separate WAL storage a name-shaped guess would retain
// one claim out of a group and silently strand the rest.
//
// A listing that fails is an error rather than an empty result. Everything downstream reads "no
// claims" as "this instance has no disk", which on the keep path means nothing is retained and on the
// destroy path means nothing was destroyed — both of them false, and the second of them reported to
// someone who just typed the name of a database back.
func (a *Adapter) cnpgClusterClaims(ctx context.Context, name string) ([]corev1.PersistentVolumeClaim, error) {
	list, err := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: cnpgClusterLabel + "=" + name,
	})
	if err != nil {
		return nil, fmt.Errorf("kube: listing the CloudNativePG volumes of addon %q: %w", name, err)
	}
	out := list.Items
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// retainCNPGClaim turns one of CloudNativePG's claims into a claim ADR-0064 §1 has kept: it strips
// the `Cluster` from the claim's owner references so the garbage collector will not take it when the
// `Cluster` goes, and labels it as a retained Burrow add-on data volume so ADR-0064 §6's listing can
// see it.
//
// THE LABEL IS ADDED HERE AND NOT AT INSTALL, and the timing is the argument. A live CNPG claim is
// the operator's: labelling it `managed-by=burrow` while the `Cluster` still owns it would put the
// running instance's disk in a listing whose whole subject is volumes nothing owns any more. The
// moment the owner reference comes off is the moment the claim stops being the operator's and becomes
// a retained volume — the thing the listing is FOR — so that is when it is labelled. Without this the
// §6 listing would be blank for every CloudNativePG-backed instance, and "kept your data" would be a
// promise with no way to find what was kept.
//
// IT DELIBERATELY DOES NOT MARK THE CLAIM `detached`, which is the other half of what `kubectl cnpg
// destroy --keep-pvc` does, and this is the one place Burrow's behaviour departs from that command on
// purpose. CloudNativePG's PVC classifier treats any status it does not recognize — and it recognizes
// only `ready`, `initializing` and empty — as a claim to IGNORE. A `detached` claim is therefore never
// classified as dangling, and a dangling claim is precisely what a `Cluster` created again under the
// same name reattaches an instance to instead of initializing a new one. Marking it would leave the
// data present and unreachable, and ADR-0064 §1's promise is not that the bytes survive, it is that
// reinstalling the add-on picks them back up. The claim keeps CloudNativePG's own metadata untouched
// for exactly that reason.
//
// The patch is a JSON merge patch rather than a read-modify-write. Owner references are what is being
// removed, and a conflict-retry loop around the object holding a database is a worse way to express
// "take this one field off" than a patch that names the fields it sets and nothing else.
func (a *Adapter) retainCNPGClaim(ctx context.Context, pvc *corev1.PersistentVolumeClaim, instance, env string) error {
	owners := make([]metav1.OwnerReference, 0, len(pvc.OwnerReferences))
	for _, ref := range pvc.OwnerReferences {
		if ref.Kind == cnpgClusterKind && ref.Name == instance && ref.APIVersion == cnpgClusterAPIVersion {
			continue
		}
		owners = append(owners, ref)
	}
	patch := map[string]any{"metadata": map[string]any{
		"ownerReferences": owners,
		// The labels AddonVolumes selects and attributes by. The add-on and environment labels are
		// already there — the `Cluster` passes them down through inheritedMetadata — and are written
		// again so a claim that somehow lacks them is still attributed to the right instance rather
		// than to the default environment by fallback.
		"labels": map[string]string{
			managedByLabel:  managedByValue,
			nameLabel:       instance,
			addonLabel:      string(controlplane.AddonPostgres),
			addonEnvLabel:   env,
			addonVolumeRole: controlplane.AddonVolumeData,
		},
	}}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("kube: composing the retain patch for addon volume %q: %w", pvc.Name, err)
	}
	if _, err := a.client.CoreV1().PersistentVolumeClaims(a.addonNamespace).
		Patch(ctx, pvc.Name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		// Refused rather than pressed on with. The next step deletes the `Cluster`, and a claim still
		// carrying it as an owner would be collected — the removal would report that it kept the data
		// and would have destroyed it.
		return fmt.Errorf("kube: refusing to remove addon %q: its data volume %q could not be detached from the "+
			"CloudNativePG %s, and deleting the %s while it is still owned would destroy the volume this removal "+
			"promises to keep. Nothing was removed: %w", instance, pvc.Name, cnpgClusterKind, cnpgClusterKind, err)
	}
	return nil
}
