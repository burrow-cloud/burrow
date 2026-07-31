// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// PodPlacement is the placement policy Burrow carries to pods it CAUSES TO EXIST but does not
// author — [ADR-0077](../../docs/adr/0077-placement-policy-for-pods-burrow-does-not-author.md) §2's
// third seam, wired by WithControllerPodPlacement.
//
// # Why a third seam rather than a wider second one
//
// ADR-0073's two hooks are func(*corev1.PodSpec) because Burrow builds the pod spec and hands it
// over before writing it. A CloudNativePG `Cluster` is authored by the CONTROLLER: Burrow creates a
// custom resource and CNPG composes the pod, and what CNPG accepts is `spec.affinity` (nodeSelector,
// tolerations, node and pod affinity) and `spec.topologySpreadConstraints` — not a pod template.
// There is no pod spec to hand a hook, so there is no way to reach the most consequential pod Burrow
// places without a seam shaped like what the controller actually consumes.
//
// Synthesising a `PodSpec`, letting a hook mutate it, and scraping the fields back out was rejected
// in ADR-0077 §2: it invents a pod that never exists, and a field set on the fake spec that has no
// destination vanishes with nothing to notice it — the exact silent drop §3 is written to prevent.
//
// # It is the vocabulary, not CNPG's schema
//
// The fields below are Kubernetes' own placement vocabulary, spelled with Kubernetes' own types.
// Nothing here is named after CNPG, so a second controller maps onto the same seam and a CNPG
// version bump is not a breaking change for anyone who wired it. Where a target's field is named
// differently — CNPG carries pod affinity as `additionalPodAffinity`, because its own generated
// anti-affinity is still there underneath — the translation absorbs the difference and the doc
// comment on the field says so.
//
// # It is a value, not a function
//
// The two ADR-0073 hooks are functions because they run over a fully-constructed pod spec and may
// key their decision off what the engine composed (ADR-0073 §6). Here there is nothing to key off:
// Burrow does not compose the pod, so a function would be handed an argument invented for it. A
// value also makes ADR-0077 §3 exact — policy with no destination is refused when it is WIRED,
// because the whole policy is known then, rather than at some later write. And it removes ADR-0073
// §6's idempotency obligation from the wiring author entirely: a value applied twice is the value.
//
// # The obligation on whoever wires it (ADR-0077 §4)
//
// **Placement decides whether the database's volume can attach.** Under CNPG the controller manages
// the PersistentVolumeClaims, and a `Cluster` whose pods cannot reach their volumes is not a
// scheduling inconvenience — it is a database that will not start. Steering is not restricted here,
// because Burrow cannot know a cluster's topology, but it is the reason the managed product's own
// policy is one toleration and deliberately nothing else: k3s local-path volumes bind to one node,
// so any nodeSelector or affinity strands them (ADR-0077 §5).
//
// The zero value carries no policy and leaves every object Burrow writes exactly as it would be
// otherwise, which is ADR-0073 §4's guarantee at this seam.
type PodPlacement struct {
	// NodeSelector restricts the pod to nodes carrying these labels. Merged into the target's own
	// node selector field; it does not replace a selector the controller sets for its own reasons.
	NodeSelector map[string]string
	// Tolerations let the pod schedule onto tainted nodes. This is the field the tainted-pool
	// cluster of ADR-0061 needs and the only one the managed product sets (ADR-0077 §5).
	Tolerations []corev1.Toleration
	// NodeAffinity steers the pod toward or away from nodes by label expression.
	NodeAffinity *corev1.NodeAffinity
	// PodAffinity co-locates the pod with other pods. Under CNPG this lands on
	// `additionalPodAffinity`: ADDITIONAL to whatever the controller generates, not a replacement
	// for it.
	PodAffinity *corev1.PodAffinity
	// PodAntiAffinity separates the pod from other pods. Under CNPG this lands on
	// `additionalPodAntiAffinity`, and CNPG's own instance anti-affinity (`enablePodAntiAffinity`,
	// `podAntiAffinityType`) still applies underneath — the target's replica-spreading policy is
	// the controller's business, not placement policy Burrow relays.
	PodAntiAffinity *corev1.PodAntiAffinity
	// TopologySpreadConstraints spread the pods across failure domains.
	TopologySpreadConstraints []corev1.TopologySpreadConstraint
}

// WithControllerPodPlacement wires the operator's placement policy for pods Burrow causes a
// third-party controller to create — today the CloudNativePG `Cluster` behind the Postgres add-on
// (ADR-0066 §1). It is the third seam of ADR-0077 §2, alongside WithPodMutator (the app's own image)
// and WithPlatformPodMutator (Burrow's own images), and it does not overlap either: those two reach
// pods this package builds, and this one reaches pods it does not.
//
// "Controller" here is the third-party controller that authors the pod. ADR-0077 calls these
// workloads "operator-authored"; this method says "controller" because "operator" already means two
// other things in this repository — Burrow's own Kubernetes operator under operator/, and the person
// operating this binary, whose policy is what this carries.
//
// # It can refuse, and that is the point
//
// A returned error means some part of p has NO DESTINATION on a target Burrow writes, naming the
// exact JSON path. Refusing here rather than dropping it later is ADR-0077 §3: a CRD's structural
// schema PRUNES fields it does not know, silently and without an API error, so a `Cluster` written
// with an unknown placement field comes back without it and nothing anywhere says so. The operator
// who wired the hook believes their policy is in force. A database that refuses to start is
// recoverable; a database silently running unplaced is discovered during an incident.
//
// Policy that has no field on ANY target — a runtimeClassName, a security context, a volume — cannot
// be expressed at all: PodPlacement has no such field, so it is a compile error rather than a
// runtime refusal. The refusal below is for the subtler case, where the vocabulary has a field and a
// specific target's schema turns out not to carry it.
//
// The check runs against the placement schema recorded from the pinned CNPG release
// (cnpg_placement_schema.json), not against the CRD installed in the cluster. That is the honest
// limit: an install running a CNPG older than the pin can prune a field this check accepted. Reading
// the live CRD at start-up would close it and needs a cluster to do so.
//
// Returns the adapter for chaining, or a nil adapter and an error so a caller cannot proceed on a
// policy that would not be carried.
func (a *Adapter) WithControllerPodPlacement(p PodPlacement) (*Adapter, error) {
	return a.withControllerPodPlacement(p, placementTargets)
}

// withControllerPodPlacement is the wiring path with its targets passed in rather than read from
// the package. Tests wire against a target whose schema is not today's CNPG, which is the only way
// to exercise the refusal: the pinned release carries the whole vocabulary, so the case §3 exists
// for does not exist yet — and the day it does is a controller upgrade, on a database holding
// tenant data, which is a poor time to find out the refusal was never run.
func (a *Adapter) withControllerPodPlacement(p PodPlacement, targets []placementTarget) (*Adapter, error) {
	if err := validatePodPlacement(p, targets); err != nil {
		return nil, err
	}
	a.controllerPlacement = p
	return a, nil
}

// placementTarget is one controller-authored workload family Burrow causes to exist: how the
// vocabulary translates into the resource that controller reconciles, and the recorded schema of
// that resource. A second controller is a second entry — the seam above does not change.
type placementTarget struct {
	// name identifies the target in a refusal, so the message says which resource had no field.
	name string
	// translate renders the vocabulary as the fragment of the custom resource's spec that carries
	// placement. It returns exactly the keys the policy sets and no others, so a placement that
	// touches nothing leaves the resource byte-for-byte as it would be.
	translate func(PodPlacement) (map[string]any, error)
	// schema loads the recorded placement schema of the pinned release of that controller's CRD.
	schema func() (*recordedPlacementSchema, error)
}

// placementTargets is every controller-authored resource this package writes placement into. It is
// read, never assigned: the wiring path takes its targets as an argument so tests substitute rather
// than mutate.
var placementTargets = []placementTarget{cnpgClusterTarget}

// validatePodPlacement translates p for every target and reports the first target that cannot carry
// all of it, listing every path with no destination rather than only the first — an operator fixing
// a policy should see the whole gap in one pass, not one restart per field.
func validatePodPlacement(p PodPlacement, targets []placementTarget) error {
	for _, t := range targets {
		fragment, err := t.translate(p)
		if err != nil {
			return fmt.Errorf("kube: rendering placement policy for %s: %w", t.name, err)
		}
		schema, err := t.schema()
		if err != nil {
			return fmt.Errorf("kube: loading the recorded placement schema for %s: %w", t.name, err)
		}
		if gaps := placementGaps("spec", fragment, schema.Spec); len(gaps) > 0 {
			return fmt.Errorf("kube: placement policy cannot be carried by %s (%s %s):\n%s\n\n%s",
				t.name, schema.Controller, schema.Version, strings.Join(gaps, "\n"), placementRefusalGuidance)
		}
	}
	return nil
}

// placementRefusalGuidance is appended to every refusal so the message teaches the decision rather
// than merely reporting a mismatch — the same posture as authoredPodPathRationale on the ADR-0073
// paths.
const placementRefusalGuidance = `Why this is refused rather than dropped (ADR-0077 §3):

  Burrow does not author this pod. It creates a custom resource and a controller composes the pod
  from it, so placement policy reaches the pod only through the fields that resource has. A field
  the resource does not have is not rejected by the API server — a CRD's structural schema PRUNES
  unknown fields, silently and with no error — so the object comes back without the policy and
  nothing says so. The pod then sits Pending, which reports as zero ready instances and reads as a
  slow start.

What to do:

  - Express the policy with a field the target carries, if one exists.
  - Or drop it here and enforce it where it can be enforced: admission policy, a default runtime
    class, a namespace-level control. Wiring nothing sandboxes nothing (ADR-0073 §5), and this seam
    makes policy REACH the pod rather than making it mandatory.
  - If the field genuinely should be carried and the target gained it in a later release, the pinned
    release and the recorded schema (cnpg_placement_schema.json) are what to move.`

// recordedPlacementSchema is the placement subtree of a controller CRD's OpenAPI schema, recorded
// from a pinned release rather than derived from that controller's Go types.
//
// Recording the SCHEMA rather than importing the types is deliberate. The schema is what the API
// server validates and prunes against, so it is the contract that actually decides whether policy
// survives the write; and it keeps a placement seam from dragging the CNPG API module — and with it
// barman-cloud, the prometheus-operator APIs, ginkgo and gomega — into a dependency graph the
// project keeps small on purpose. ADR-0066 §3 declines the barman path on licensing grounds; a
// build dependency on it for a placement mapping would be an odd way to re-acquire it.
type recordedPlacementSchema struct {
	// Controller is the project the schema was recorded from ("CloudNativePG").
	Controller string `json:"controller"`
	// Version is the release it was recorded from, and must match the release Burrow pins.
	Version string `json:"version"`
	// Source is the release artifact the recording came from, so it can be re-derived.
	Source string `json:"source"`
	// CRD and APIVersion identify the resource inside that artifact.
	CRD        string `json:"crd"`
	APIVersion string `json:"apiVersion"`
	// Spec is the pruned schema of the custom resource's spec, holding only the placement fields.
	// Descriptions, defaults and validation keywords are stripped: what matters here is which paths
	// exist and what kind of thing each one is.
	Spec *schemaNode `json:"spec"`
}

// schemaNode is the pruned OpenAPI shape of one field: enough to answer "does this path exist, and
// is it still the same kind of thing", and nothing else.
type schemaNode struct {
	Type       string                 `json:"type,omitempty"`
	Properties map[string]*schemaNode `json:"properties,omitempty"`
	Items      *schemaNode            `json:"items,omitempty"`
	// AdditionalProperties is set on free-form maps — nodeSelector, a label selector's matchLabels —
	// where every key is carried and none can be pruned.
	AdditionalProperties *schemaNode `json:"additionalProperties,omitempty"`
}

// placementGaps walks a translated fragment against a target's recorded schema and returns one line
// per JSON path that has no destination, or whose destination is no longer the same kind of thing.
//
// It walks the VALUE rather than the type, so only what the policy actually sets is checked: a
// vocabulary field the target lacks costs nothing until someone sets it, and then it costs a
// refusal rather than a silent drop.
func placementGaps(path string, value any, node *schemaNode) []string {
	if value == nil {
		return nil
	}
	if node == nil {
		return []string{fmt.Sprintf("  %s — the target's schema has no field at this path, so the API "+
			"server would prune it on write", path)}
	}
	var gaps []string
	switch v := value.(type) {
	case map[string]any:
		if node.Type != "" && node.Type != "object" {
			return []string{typeGap(path, node.Type, "an object")}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child, ok := node.Properties[k]
			if !ok {
				// A free-form map (additionalProperties, no properties) carries every key.
				child = node.AdditionalProperties
			}
			if child == nil && node.Properties == nil && node.AdditionalProperties == nil {
				// The schema states an object with no shape at all; it cannot prune what it does
				// not describe, so there is nothing to report.
				continue
			}
			gaps = append(gaps, placementGaps(path+"."+k, v[k], child)...)
		}
	case []any:
		if node.Type != "" && node.Type != "array" {
			return []string{typeGap(path, node.Type, "an array")}
		}
		for i, item := range v {
			gaps = append(gaps, placementGaps(fmt.Sprintf("%s[%d]", path, i), item, node.Items)...)
		}
	case string:
		if node.Type != "" && node.Type != "string" {
			gaps = append(gaps, typeGap(path, node.Type, "a string"))
		}
	case bool:
		if node.Type != "" && node.Type != "boolean" {
			gaps = append(gaps, typeGap(path, node.Type, "a boolean"))
		}
	case float64, json.Number:
		if node.Type != "" && node.Type != "integer" && node.Type != "number" {
			gaps = append(gaps, typeGap(path, node.Type, "a number"))
		}
	}
	return gaps
}

// typeGap reports a path that still exists on the target but has changed shape — a rename's quieter
// cousin, where the write is accepted and means something else.
func typeGap(path, want, got string) string {
	return fmt.Sprintf("  %s — the target's schema has %q here, and the policy supplies %s, so the "+
		"value would be rejected or reinterpreted", path, want, got)
}

// asJSON round-trips a Kubernetes value through JSON, which is exactly what writing it into a custom
// resource does. Going through the types' own marshalling rather than naming fields by hand is what
// keeps the translation shaped by Kubernetes' vocabulary: only the handful of TOP-LEVEL keys a
// target renames are written out in this package, and every leaf below them is spelled by k8s.io/api.
func asJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshalling %T: %w", v, err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("unmarshalling %T: %w", v, err)
	}
	return out, nil
}
