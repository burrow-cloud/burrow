// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// CNPGVersion is the CloudNativePG release this package's placement translation targets, and the
// release Burrow installs when the Postgres add-on needs the operator (ADR-0066 §1).
//
// It is a pin rather than "whatever is latest" because the translation below is a claim about that
// release's schema. Moving this constant without re-recording cnpg_placement_schema.json fails
// TestCNPGPlacementSchemaIsTheRecordedRelease, which is the point: a controller upgrade is exactly
// when a placement field can be renamed or removed, and ADR-0077's Consequences call a mapping that
// silently stops covering a field the reintroduction of §3's failure.
const CNPGVersion = "1.30.0"

// cnpgPlacementSchemaJSON is the placement subtree of the `clusters.postgresql.cnpg.io` CRD, taken
// from the pinned release's install manifest. It is embedded rather than kept as testdata because
// the WIRING-TIME refusal of ADR-0077 §3 validates against it: it is production data, and the tests
// check it rather than owning it.
//
// Re-record it with: BURROW_CNPG_SCHEMA=record go test ./controlplane/kube -run CNPGPlacementSchema
//
//go:embed cnpg_placement_schema.json
var cnpgPlacementSchemaJSON []byte

// cnpgClusterTarget is the CloudNativePG `Cluster` (ADR-0066 §1): the first — and today the only —
// pod Burrow causes to exist without authoring it.
var cnpgClusterTarget = placementTarget{
	name:      "the CloudNativePG Cluster",
	translate: cnpgPlacement,
	schema:    cnpgPlacementSchema,
}

// cnpgPlacementSchema parses the embedded recording. Parsing on demand rather than at package init
// keeps a malformed recording a returned error at the one call site that needs it, instead of a
// panic that takes down a binary which may never wire placement at all.
func cnpgPlacementSchema() (*recordedPlacementSchema, error) {
	var s recordedPlacementSchema
	if err := json.Unmarshal(cnpgPlacementSchemaJSON, &s); err != nil {
		return nil, fmt.Errorf("parsing the embedded CNPG placement schema: %w", err)
	}
	if s.Spec == nil {
		return nil, fmt.Errorf("the embedded CNPG placement schema records no spec")
	}
	return &s, nil
}

// cnpgPlacement renders a PodPlacement as the fragment of a CNPG `Cluster` spec that carries
// placement: `spec.affinity` and `spec.topologySpreadConstraints`.
//
// Every key is omitted unless the policy sets it. That is what lets the seam express the case
// ADR-0077 §5 says it must — "tolerate this taint, touch nothing else" — as a fragment holding one
// tolerations list and no other key at all. A translation that always emitted an `affinity` object
// with empty members would hand CNPG an explicit empty nodeSelector and an explicit empty affinity,
// which is policy nobody asked for on a database whose local-path volume is bound to one node.
//
// Only the five top-level names below are written by hand; everything under them is spelled by
// k8s.io/api's own JSON tags, so a leaf field is never misnamed here. Two of the five are not the
// vocabulary's name:
//
//   - PodAffinity lands on `additionalPodAffinity` and PodAntiAffinity on
//     `additionalPodAntiAffinity`. CNPG generates its own instance anti-affinity from
//     `enablePodAntiAffinity` and `podAntiAffinityType`; these fields add to it rather than
//     replacing it, and Burrow does not touch the controller's own replica-spreading policy from a
//     placement seam.
//
// The remaining three — `nodeSelector`, `tolerations`, `nodeAffinity` — sit under `spec.affinity`
// rather than at the top of the spec, which is CNPG's grouping and not Kubernetes'.
func cnpgPlacement(p PodPlacement) (map[string]any, error) {
	affinity := map[string]any{}
	for _, f := range []struct {
		key   string
		set   bool
		value any
	}{
		{"nodeSelector", len(p.NodeSelector) > 0, p.NodeSelector},
		{"tolerations", len(p.Tolerations) > 0, p.Tolerations},
		{"nodeAffinity", p.NodeAffinity != nil, p.NodeAffinity},
		{"additionalPodAffinity", p.PodAffinity != nil, p.PodAffinity},
		{"additionalPodAntiAffinity", p.PodAntiAffinity != nil, p.PodAntiAffinity},
	} {
		if !f.set {
			continue
		}
		v, err := asJSON(f.value)
		if err != nil {
			return nil, fmt.Errorf("spec.affinity.%s: %w", f.key, err)
		}
		affinity[f.key] = v
	}

	spec := map[string]any{}
	if len(affinity) > 0 {
		spec["affinity"] = affinity
	}
	if len(p.TopologySpreadConstraints) > 0 {
		v, err := asJSON(p.TopologySpreadConstraints)
		if err != nil {
			return nil, fmt.Errorf("spec.topologySpreadConstraints: %w", err)
		}
		spec["topologySpreadConstraints"] = v
	}
	return spec, nil
}

// cnpgClusterPlacement is what a caller composing a `Cluster` merges into its spec. It returns an
// empty map when nothing is wired, so the composed object is byte-for-byte what it would be without
// this seam (ADR-0073 §4, at ADR-0077's target).
//
// It cannot fail on a wired placement: WithControllerPodPlacement already translated and validated
// the same value, and refused rather than storing one that would not render.
func (a *Adapter) cnpgClusterPlacement() map[string]any {
	spec, err := cnpgPlacement(a.controllerPlacement)
	if err != nil {
		return map[string]any{}
	}
	return spec
}
