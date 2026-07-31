// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane"
)

// CloudNativePG's identity in a cluster, taken from the pinned release's install manifest
// (CNPGVersion). Burrow does not ship the operator — it applies the upstream release artifact and
// the cluster pulls the image — so these names are a claim about somebody else's manifest, and they
// move when the pin moves.
const (
	// CNPGAPIGroup is CloudNativePG's API group. Its presence in API-group discovery means the
	// operator's CRDs are installed, which is the same no-RBAC signal cert-manager is detected by.
	CNPGAPIGroup = "postgresql.cnpg.io"
	// CNPGNamespace and CNPGControllerDeployment are where the release manifest puts the operator.
	// They name the wait target for an install; detection uses the label selector below instead, so
	// an operator installed elsewhere (a Helm release into another namespace) is still seen.
	CNPGNamespace            = "cnpg-system"
	CNPGControllerDeployment = "cnpg-controller-manager"
	// cnpgControllerSelector matches the operator Deployment by the label the release manifest and
	// the Helm chart both carry, in any namespace — the same posture as the ingress-nginx and MetalLB
	// probes, which find a controller wherever it was installed rather than only where Burrow would
	// have put it.
	cnpgControllerSelector = "app.kubernetes.io/name=cloudnative-pg"
	// cnpgManagerContainer is the operator container inside that Deployment, and the one whose image
	// carries the release. Naming it keeps a sidecar somebody added from being read as the version.
	cnpgManagerContainer = "manager"
)

// CNPGManifestURL is the release artifact a CloudNativePG version publishes: the CRDs, the RBAC, and
// the operator Deployment in one document. It is what an install applies AND what the placement
// schema is recorded from, so the schema Burrow validates against is the schema the cluster holds.
//
// Applying the upstream artifact rather than a vendored copy is deliberate: Burrow ships no
// third-party bytes, it points a cluster at the images and manifests their publishers already serve.
func CNPGManifestURL(version string) string {
	return fmt.Sprintf("https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v%s/cnpg-%s.yaml", version, version)
}

// DetectCloudNativePG reports the CloudNativePG operator's situation: whether its CRDs are served,
// whether a controller is actually running, and which release that controller is.
//
// Present and Ready are separate for the reason detectIngress keeps them separate: a CRD is
// cluster-scoped and OUTLIVES the operator that installed it. Delete the cnpg-system namespace and
// every `postgresql.cnpg.io` CRD is still there, still served by discovery, and nothing reconciles a
// `Cluster` written against it — the object is accepted and then sits there. Reporting that as
// "CloudNativePG is installed" would be the orphan-IngressClass mistake on a component that holds
// tenant data.
//
// It needs no new RBAC: API-group discovery needs none at all, and the cluster-wide
// apps/deployments get/list the capability ClusterRole already holds for ingress-controller and
// MetalLB detection covers the rest.
//
// It is exported, unlike the survey's other detectors, so `burrow cluster postgres install` decides
// whether to install from the SAME read `burrow cluster` reports. The ingress path grew a second,
// separate presence check in cmd/burrow and the two disagreed about an orphaned install; one
// detector is how that does not happen twice.
func DetectCloudNativePG(ctx context.Context, client kubernetes.Interface) (controlplane.CloudNativePGCapability, error) {
	out := controlplane.CloudNativePGCapability{Pinned: CNPGVersion}

	groups, err := client.Discovery().ServerGroups()
	if err != nil {
		return controlplane.CloudNativePGCapability{}, fmt.Errorf("kube: discovering API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name == CNPGAPIGroup {
			out.Present = true
			break
		}
	}

	deps, err := client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: cnpgControllerSelector,
	})
	if err != nil {
		return controlplane.CloudNativePGCapability{}, fmt.Errorf("kube: listing cloudnative-pg controller deployments: %w", err)
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		if d.Status.ReadyReplicas > 0 {
			out.Ready = true
		}
		if out.Version == "" {
			out.Version = cnpgOperatorVersion(d)
		}
	}
	return out, nil
}

// cnpgOperatorVersion reads the release from the operator container's image tag, or "" when the
// Deployment has no such container or the image is pinned by digest alone. An empty version is
// reported as unknown rather than guessed: "which CloudNativePG is this" has no safe default, and a
// wrong answer to it is worse than no answer on the component the databases run under.
func cnpgOperatorVersion(d *appsv1.Deployment) string {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == cnpgManagerContainer {
			return imageTag(c.Image)
		}
	}
	return ""
}

// imageTag extracts the tag from a pullable image reference ("1.30.0" from
// "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0"), or "" when the reference carries no tag. A digest
// is stripped first, and the tag is the last ':'-separated segment after the final '/', so a
// registry host's ":port" is never mistaken for one.
func imageTag(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon <= slash {
		return ""
	}
	return ref[colon+1:]
}
