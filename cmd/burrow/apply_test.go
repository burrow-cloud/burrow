// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// testApplier builds an applier backed by a fake dynamic client and a static RESTMapper that knows
// a cluster-scoped kind (Namespace) and a namespaced kind (ConfigMap). A prepended "patch" reactor
// records every apply so a test can assert one apply per object and inspect the resource, namespace,
// and patch options without a real cluster (the fake tracker would reject an apply-create otherwise).
func testApplier() (*applier, *[]clienttesting.PatchActionImpl) {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)

	gvrToListKind := map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "namespaces"}: "NamespaceList",
		{Version: "v1", Resource: "configmaps"}: "ConfigMapList",
	}
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind)

	var patches []clienttesting.PatchActionImpl
	dc.PrependReactor("patch", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pa := action.(clienttesting.PatchActionImpl)
		patches = append(patches, pa)
		obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
		obj.SetName(pa.GetName())
		return true, obj, nil
	})

	return &applier{dyn: dc, mapper: mapper}, &patches
}

// twoDocManifest is a namespace (cluster-scoped) and a configmap (namespaced) plus blank/comment
// documents that parsing must skip.
const twoDocManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: burrow
---
# a comment-only document, skipped
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: burrow
data:
  k: v
---
`

func TestParseManifestsSkipsEmpties(t *testing.T) {
	objs, err := parseManifests(twoDocManifest)
	if err != nil {
		t.Fatalf("parseManifests: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("parsed %d objects, want 2 (blank and comment-only documents must be skipped)", len(objs))
	}
	if objs[0].GetKind() != "Namespace" || objs[1].GetKind() != "ConfigMap" {
		t.Errorf("kinds = %q, %q; want Namespace, ConfigMap", objs[0].GetKind(), objs[1].GetKind())
	}
}

func TestApplyIssuesOneApplyPerObject(t *testing.T) {
	ap, patches := testApplier()
	var out bytes.Buffer
	if err := ap.apply(context.Background(), twoDocManifest, false, false, &out, &out); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(*patches) != 2 {
		t.Fatalf("issued %d applies, want one per object (2)", len(*patches))
	}

	// Namespaces are applied before the resources that live in them, so the cluster-scoped
	// Namespace is first: no namespace on the request and resource "namespaces".
	ns := (*patches)[0]
	if ns.GetResource() != (schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}) {
		t.Errorf("first apply resource = %v, want v1/namespaces", ns.GetResource())
	}
	if ns.GetNamespace() != "" {
		t.Errorf("cluster-scoped Namespace applied with namespace %q, want none", ns.GetNamespace())
	}

	// The namespaced ConfigMap resolves to resource "configmaps" in its declared namespace.
	cm := (*patches)[1]
	if cm.GetResource() != (schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}) {
		t.Errorf("second apply resource = %v, want v1/configmaps", cm.GetResource())
	}
	if cm.GetNamespace() != "burrow" {
		t.Errorf("ConfigMap applied in namespace %q, want burrow", cm.GetNamespace())
	}

	// Every apply is a server-side apply owned by Burrow's field manager, with force so Burrow
	// takes ownership of the fields it sets, and is not a dry run.
	for i, p := range *patches {
		if p.GetPatchType() != types.ApplyPatchType {
			t.Errorf("apply %d patch type = %v, want ApplyPatchType (server-side apply)", i, p.GetPatchType())
		}
		if p.PatchOptions.FieldManager != fieldManager {
			t.Errorf("apply %d field manager = %q, want %q", i, p.PatchOptions.FieldManager, fieldManager)
		}
		if p.PatchOptions.Force == nil || !*p.PatchOptions.Force {
			t.Errorf("apply %d should force-apply", i)
		}
		if len(p.PatchOptions.DryRun) != 0 {
			t.Errorf("apply %d should not be a dry run, got DryRun=%v", i, p.PatchOptions.DryRun)
		}
	}

	// Both objects are new to the fake (Get returns NotFound), so they summarize as created.
	if got, want := out.String(), "Applied 2 resource(s): 2 created.\n"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestApplyDryRunSetsDryRunAll(t *testing.T) {
	ap, patches := testApplier()
	var out bytes.Buffer
	if err := ap.apply(context.Background(), twoDocManifest, true, false, &out, &out); err != nil {
		t.Fatalf("apply dry-run: %v", err)
	}
	if len(*patches) != 2 {
		t.Fatalf("issued %d applies, want 2", len(*patches))
	}
	for i, p := range *patches {
		if len(p.PatchOptions.DryRun) != 1 || p.PatchOptions.DryRun[0] != "All" {
			t.Errorf("apply %d DryRun = %v, want [All]", i, p.PatchOptions.DryRun)
		}
	}
}

func TestApplyVerboseListsEachResource(t *testing.T) {
	ap, _ := testApplier()
	var out bytes.Buffer
	if err := ap.apply(context.Background(), twoDocManifest, false, true, &out, &out); err != nil {
		t.Fatalf("apply verbose: %v", err)
	}
	s := out.String()
	for _, want := range []string{"namespace/burrow created", "configmap/cfg created"} {
		if !strings.Contains(s, want) {
			t.Errorf("verbose output missing %q:\n%s", want, s)
		}
	}
}
