// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// serverNodeToleration is the managed product's whole placement policy, and
// [ADR-0077](../../docs/adr/0077-placement-policy-for-pods-burrow-does-not-author.md) §5 makes it
// the case this seam must be able to express: one toleration for the server node and deliberately
// nothing else, because k3s local-path volumes bind to one node and any steering strands them.
func serverNodeToleration() PodPlacement {
	return PodPlacement{
		Tolerations: []corev1.Toleration{{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
	}
}

// TestPodPlacementExpressesOneTolerationAndNothingElse is ADR-0077 §5's obligation, asserted as an
// exact equality rather than as "the toleration is present".
//
// The distinction is the whole test. A translation that always emitted an `affinity` object would
// hand CNPG an explicit empty nodeSelector and an explicit empty affinity alongside the toleration,
// and on the cluster this policy exists for that is steering — the database's local-path volume is
// bound to one node, so a placement field nobody asked for is how it gets stranded. "Touch nothing
// else" has to mean nothing else is written.
func TestPodPlacementExpressesOneTolerationAndNothingElse(t *testing.T) {
	fragment, err := cnpgPlacement(serverNodeToleration())
	if err != nil {
		t.Fatalf("cnpgPlacement: %v", err)
	}
	want := map[string]any{
		"affinity": map[string]any{
			"tolerations": []any{map[string]any{
				"key":      "node-role.kubernetes.io/control-plane",
				"operator": "Exists",
				"effect":   "NoSchedule",
			}},
		},
	}
	if !reflect.DeepEqual(fragment, want) {
		t.Errorf("the server-node toleration rendered as %s,\nwant %s.\n"+
			"ADR-0077 §5: a design that cannot express \"tolerate this taint, touch nothing else\" has "+
			"failed. Any extra key here is placement policy the operator did not ask for, on a database "+
			"whose volume is bound to one node.", asIndentedJSON(t, fragment), asIndentedJSON(t, want))
	}
}

// TestZeroPodPlacementRendersNothing is ADR-0073 §4's byte-for-byte obligation at ADR-0077's target:
// an install that wires nothing sees a `Cluster` exactly as it would have been. A translation that
// rendered an empty `affinity: {}` would be a diff on every write and a field the controller now
// considers set.
func TestZeroPodPlacementRendersNothing(t *testing.T) {
	fragment, err := cnpgPlacement(PodPlacement{})
	if err != nil {
		t.Fatalf("cnpgPlacement: %v", err)
	}
	if len(fragment) != 0 {
		t.Errorf("the zero placement rendered %s, want no keys at all", asIndentedJSON(t, fragment))
	}
	// And through the accessor a caller composing a Cluster would use, on an adapter nobody wired.
	if got := New(fake.NewSimpleClientset(), "apps").cnpgClusterPlacement(); len(got) != 0 {
		t.Errorf("an unwired adapter contributed %s to a Cluster spec, want nothing", asIndentedJSON(t, got))
	}
}

// TestEmptyCollectionsRenderNothing covers the shape a caller most easily arrives at by accident: a
// policy built up conditionally, where the toleration list ended up allocated but empty. Rendering
// `tolerations: []` would replace whatever the controller had with an empty list, which is a
// placement change disguised as no policy.
func TestEmptyCollectionsRenderNothing(t *testing.T) {
	fragment, err := cnpgPlacement(PodPlacement{
		NodeSelector:              map[string]string{},
		Tolerations:               []corev1.Toleration{},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{},
	})
	if err != nil {
		t.Fatalf("cnpgPlacement: %v", err)
	}
	if len(fragment) != 0 {
		t.Errorf("empty collections rendered %s, want no keys at all", asIndentedJSON(t, fragment))
	}
}

// TestControllerPlacementUsesTheVocabularysOwnNames pins the two places the translation renames a
// field, because both are load-bearing and neither is obvious from the vocabulary.
//
// CNPG's own instance anti-affinity is generated from `enablePodAntiAffinity` and
// `podAntiAffinityType`, and it stays. What Burrow relays lands on the `additional*` fields, which
// ADD to it. Writing the vocabulary's names straight through would either be pruned (there is no
// `podAffinity` on a Cluster) or, worse, be read as Burrow taking over the controller's own
// replica-spreading policy from a seam that is about the operator's cluster, not about Postgres.
func TestControllerPlacementUsesTheVocabularysOwnNames(t *testing.T) {
	fragment, err := cnpgPlacement(PodPlacement{
		PodAffinity:     &corev1.PodAffinity{},
		PodAntiAffinity: &corev1.PodAntiAffinity{},
		NodeAffinity:    &corev1.NodeAffinity{},
		NodeSelector:    map[string]string{"pool": "platform"},
	})
	if err != nil {
		t.Fatalf("cnpgPlacement: %v", err)
	}
	affinity, ok := fragment["affinity"].(map[string]any)
	if !ok {
		t.Fatalf("no affinity in %s", asIndentedJSON(t, fragment))
	}
	for _, want := range []string{"nodeSelector", "nodeAffinity", "additionalPodAffinity", "additionalPodAntiAffinity"} {
		if _, ok := affinity[want]; !ok {
			t.Errorf("spec.affinity has no %q; got %s", want, asIndentedJSON(t, affinity))
		}
	}
	for _, unwanted := range []string{"podAffinity", "podAntiAffinity", "enablePodAntiAffinity", "podAntiAffinityType"} {
		if _, ok := affinity[unwanted]; ok {
			t.Errorf("spec.affinity carries %q. The first two do not exist on a Cluster and would be "+
				"pruned; the second two are the controller's own replica-spreading policy, which this "+
				"seam does not set.", unwanted)
		}
	}
}

// TestWiringAcceptsPolicyTheTargetCarries is the other half of the refusal: the concrete case of
// ADR-0077 §5 must go through, and the adapter must come back wired.
func TestWiringAcceptsPolicyTheTargetCarries(t *testing.T) {
	a, err := New(fake.NewSimpleClientset(), "apps").WithControllerPodPlacement(serverNodeToleration())
	if err != nil {
		t.Fatalf("wiring the server-node toleration was refused: %v", err)
	}
	if a == nil {
		t.Fatal("WithControllerPodPlacement returned a nil adapter with no error")
	}
	if got := a.cnpgClusterPlacement(); len(got) != 1 {
		t.Errorf("the wired policy contributed %s to a Cluster spec, want exactly the affinity key",
			asIndentedJSON(t, got))
	}
}

// TestWiringRefusesPolicyWithNoDestination is ADR-0077 §3, exercised against a target whose schema
// does not carry tolerations — the shape a CNPG release that renamed or dropped the field would
// have.
//
// It has to be built rather than found: CNPG 1.30 carries the whole vocabulary, which is what
// TestCNPGCarriesEveryFieldOfThePlacementVocabulary asserts. So the case this seam exists to refuse
// does not exist today, and the machinery that refuses it would sit untested until the day it
// mattered — which is during a controller upgrade, on a database holding tenant data.
func TestWiringRefusesPolicyWithNoDestination(t *testing.T) {
	narrowed := placementTarget{
		name:      "a Cluster whose schema dropped tolerations",
		translate: cnpgPlacement,
		schema: func() (*recordedPlacementSchema, error) {
			s, err := cnpgPlacementSchema()
			if err != nil {
				return nil, err
			}
			delete(s.Spec.Properties["affinity"].Properties, "tolerations")
			return s, nil
		},
	}
	a, err := New(fake.NewSimpleClientset(), "apps").
		withControllerPodPlacement(serverNodeToleration(), []placementTarget{narrowed})
	if err == nil {
		t.Fatal("wiring a toleration the target cannot carry was accepted. It would then be pruned by " +
			"the API server without an error, and the operator who wired it would believe their policy " +
			"was in force (ADR-0077 §3).")
	}
	if a != nil {
		t.Error("WithControllerPodPlacement returned a usable adapter alongside a refusal; a caller who " +
			"checks the adapter rather than the error would proceed on policy that is not carried")
	}
	// The refusal must name the field that had no destination — an error that says only "placement
	// is not supported" leaves the operator to find which of their fields was the problem.
	if !strings.Contains(err.Error(), "spec.affinity.tolerations") {
		t.Errorf("the refusal does not name the path with no destination:\n%v", err)
	}
}

// TestRefusalNamesEveryPathWithNoDestination checks the refusal reports the whole gap rather than
// the first field of it. An operator narrowing a policy field by field, one restart per field, is
// the version of this that gets abandoned halfway.
func TestRefusalNamesEveryPathWithNoDestination(t *testing.T) {
	narrowed := placementTarget{
		name:      "a Cluster whose schema carries no affinity at all",
		translate: cnpgPlacement,
		schema: func() (*recordedPlacementSchema, error) {
			s, err := cnpgPlacementSchema()
			if err != nil {
				return nil, err
			}
			delete(s.Spec.Properties["affinity"].Properties, "tolerations")
			delete(s.Spec.Properties["affinity"].Properties, "nodeSelector")
			return s, nil
		},
	}
	_, err := New(fake.NewSimpleClientset(), "apps").withControllerPodPlacement(PodPlacement{
		NodeSelector: map[string]string{"pool": "platform"},
		Tolerations:  serverNodeToleration().Tolerations,
	}, []placementTarget{narrowed})
	if err == nil {
		t.Fatal("two fields with no destination were accepted")
	}
	for _, want := range []string{"spec.affinity.nodeSelector", "spec.affinity.tolerations"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s:\n%v", want, err)
		}
	}
}

// TestRefusalNamesAFieldThatChangedShape covers the quieter half of drift. A field that keeps its
// name but becomes a different kind of thing is not pruned — it is rejected by the API server, or
// accepted and read as something else — and neither failure names the seam that supplied it.
func TestRefusalNamesAFieldThatChangedShape(t *testing.T) {
	reshaped := placementTarget{
		name:      "a Cluster whose tolerations became a string",
		translate: cnpgPlacement,
		schema: func() (*recordedPlacementSchema, error) {
			s, err := cnpgPlacementSchema()
			if err != nil {
				return nil, err
			}
			s.Spec.Properties["affinity"].Properties["tolerations"] = &schemaNode{Type: "string"}
			return s, nil
		},
	}
	_, err := New(fake.NewSimpleClientset(), "apps").
		withControllerPodPlacement(serverNodeToleration(), []placementTarget{reshaped})
	if err == nil {
		t.Fatal("a toleration list aimed at a string field was accepted")
	}
	if !strings.Contains(err.Error(), "spec.affinity.tolerations") {
		t.Errorf("the refusal does not name the reshaped path:\n%v", err)
	}
}

// TestControllerPlacementSurvivesWithNamespace is the environments obligation the two ADR-0073 hooks
// already carry (see WithNamespace): policy wired once at construction must reach every
// environment-scoped view of the adapter. Policy that survived only on the receiver would work in a
// single-namespace install and stop the moment an operation was routed to a named environment —
// which is exactly where a per-environment Postgres instance lives (ADR-0067 §1).
func TestControllerPlacementSurvivesWithNamespace(t *testing.T) {
	a, err := New(fake.NewSimpleClientset(), "apps").WithControllerPodPlacement(serverNodeToleration())
	if err != nil {
		t.Fatalf("WithControllerPodPlacement: %v", err)
	}
	scoped, ok := a.WithNamespace("apps-staging").(*Adapter)
	if !ok {
		t.Fatal("WithNamespace did not return a *Adapter")
	}
	if !reflect.DeepEqual(scoped.cnpgClusterPlacement(), a.cnpgClusterPlacement()) {
		t.Errorf("the environment-scoped view contributed %s, want the same policy as the receiver: %s",
			asIndentedJSON(t, scoped.cnpgClusterPlacement()), asIndentedJSON(t, a.cnpgClusterPlacement()))
	}
}

// TestControllerPlacementIsSeparateFromThePlatformHook is ADR-0077 §2's structural claim, and the
// one a future change is most likely to undo quietly: the third seam must not have grown into a
// synthesised pod spec.
//
// Rejected in §2 because a fake pod lets a field with no destination vanish with nothing to notice
// it — the exact silent drop §3 exists to prevent, reintroduced by the mechanism meant to avoid a
// third seam. It is checked by source scan rather than by behaviour because the failure is a pod
// spec that is CONSTRUCTED and then discarded, which no assertion on output can see.
//
// The scan is the one authored_pod_paths_test.go already uses, so a pod spec built on this path
// would in fact fail there too — as an uncatalogued authored pod path. This states the reason
// directly, where someone editing the placement files will meet it.
func TestControllerPlacementIsSeparateFromThePlatformHook(t *testing.T) {
	sites := authoredPodSpecSites(t)
	for _, key := range sortedKeysOf(sites) {
		site := sites[key]
		if !strings.HasPrefix(site.file, "placement") {
			continue
		}
		t.Errorf("%s:%d — %s constructs a corev1.PodSpec on the controller-placement path.\n"+
			"ADR-0077 §2 rejects synthesising a pod spec for a pod Burrow does not author: it invents a "+
			"pod that never exists, and a field set on it that has no destination vanishes with nothing "+
			"to notice it. Placement for these workloads is translated into the fields the controller "+
			"offers, and what has no field is refused (§3).", site.file, site.line, site.fn)
	}
}

// The two ADR-0073 hooks keep their exact signatures. ADR-0077 §2 leaves WithPlatformPodMutator's
// type and reach alone, so an install that wired it needs no edit; these method expressions stop
// compiling if either is widened — which is how a "small" convenience change to serve both kinds of
// pod from one hook would arrive.
var (
	_ func(*Adapter, func(*corev1.PodSpec)) *Adapter = (*Adapter).WithPodMutator
	_ func(*Adapter, func(*corev1.PodSpec)) *Adapter = (*Adapter).WithPlatformPodMutator
)

// TestPlatformPodMutatorStillReachesItsPodsAlongsideControllerPlacement is the behavioural half: the
// third seam is added, not substituted. An adapter carrying both applies the platform hook to the
// pods it authors exactly as before, and the controller placement to the resource it does not.
func TestPlatformPodMutatorStillReachesItsPodsAlongsideControllerPlacement(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a, err := New(client, "apps").
		WithAddonNamespace(addonNS).
		WithPlatformPodMutator(markPod(hookPlatform)).
		WithControllerPodPlacement(serverNodeToleration())
	if err != nil {
		t.Fatalf("WithControllerPodPlacement: %v", err)
	}

	spec := controlplane.AddonSpec{Type: controlplane.AddonCache, Backend: "valkey", Image: "valkey:test", Port: 6379}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	dep, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-cache", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get add-on Deployment: %v", err)
	}
	if got := hooksThatReached(dep.Spec.Template.Spec); !reflect.DeepEqual(got, []placementHook{hookPlatform}) {
		t.Errorf("the add-on pod was reached by %v, want exactly [%s]; adding the third seam must not "+
			"change what the ADR-0073 hooks reach", got, hookPlatform)
	}
	// And the controller placement did not leak onto a pod this adapter authors: it goes into the
	// custom resource, and nowhere else.
	if len(dep.Spec.Template.Spec.Tolerations) != 0 {
		t.Errorf("the add-on pod carries %v; controller placement is for pods Burrow does NOT author "+
			"and must not be applied to one it does", dep.Spec.Template.Spec.Tolerations)
	}
}

func asIndentedJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("encoding %T: %v", v, err)
	}
	return string(b)
}
