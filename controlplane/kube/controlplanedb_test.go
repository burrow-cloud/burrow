// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestDetectControlPlaneDatabase covers what `burrow cluster` reports about the control plane's own
// database (ADR-0086 §2). The two shapes are not distinguishable from the outside — both work, and
// only one of them can be backed up — so the report has to be a live read of the object that is
// actually there.
func TestDetectControlPlaneDatabase(t *testing.T) {
	const namespace = "burrow"

	t.Run("a CloudNativePG cluster with no destination is unbacked-up", func(t *testing.T) {
		p := prober(namespace, nil, controlPlaneCluster(namespace, 1, false))
		got := detect(t, p).ControlPlaneDatabase
		want := controlplane.ControlPlaneDatabaseCapability{
			Kind: controlplane.ControlPlaneDatabaseCloudNativePG, Ready: true, BackedUp: false,
		}
		if got != want {
			t.Errorf("database = %+v, want %+v", got, want)
		}
	})

	t.Run("a CloudNativePG cluster with the backup plugin archives", func(t *testing.T) {
		p := prober(namespace, nil, controlPlaneCluster(namespace, 1, true))
		if got := detect(t, p).ControlPlaneDatabase; !got.BackedUp {
			t.Errorf("a Cluster carrying the pgBackRest plugin should report as backed up, got %+v", got)
		}
	})

	t.Run("a cluster with no instance serving is not ready", func(t *testing.T) {
		p := prober(namespace, nil, controlPlaneCluster(namespace, 0, false))
		if got := detect(t, p).ControlPlaneDatabase; got.Ready {
			t.Errorf("a Cluster with no ready instance should not report ready, got %+v", got)
		}
	})

	t.Run("a plain Deployment is reported as plain", func(t *testing.T) {
		p := prober(namespace, []runtime.Object{plainDatabase(namespace)})
		got := detect(t, p).ControlPlaneDatabase
		want := controlplane.ControlPlaneDatabaseCapability{
			Kind: controlplane.ControlPlaneDatabasePlain, Ready: true, BackedUp: false,
		}
		if got != want {
			t.Errorf("database = %+v, want %+v", got, want)
		}
	})

	t.Run("neither is reported as nothing rather than guessed", func(t *testing.T) {
		p := prober(namespace, nil)
		if got := detect(t, p).ControlPlaneDatabase; got.Kind != "" {
			t.Errorf("with no database found the report should be empty, got %+v", got)
		}
	})

	t.Run("a prober that was told no namespace reports nothing", func(t *testing.T) {
		// The kubeconfig-side probe `burrow cluster install` runs is this case: it is standing
		// outside the control plane whose database this would describe.
		p := kube.NewProber(fake.NewSimpleClientset(plainDatabase(namespace)))
		if got := detect(t, p).ControlPlaneDatabase; got.Kind != "" {
			t.Errorf("a prober with no control-plane namespace should report nothing, got %+v", got)
		}
	})
}

func detect(t *testing.T, p *kube.Prober) controlplane.ClusterCapabilities {
	t.Helper()
	caps, err := p.DetectCapabilities(context.Background())
	if err != nil {
		t.Fatalf("DetectCapabilities: %v", err)
	}
	return caps
}

// prober builds a Prober over a fake cluster holding the given typed objects and `Cluster` custom
// resources, told which namespace the control plane lives in.
func prober(namespace string, typed []runtime.Object, clusters ...*unstructured.Unstructured) *kube.Prober {
	objs := make([]runtime.Object, 0, len(clusters))
	for _, c := range clusters {
		objs = append(objs, c)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: kube.CNPGAPIGroup, Version: "v1", Resource: "clusters"}: "ClusterList",
		}, objs...)
	return kube.NewProber(fake.NewSimpleClientset(typed...)).
		WithDynamicClient(dyn).
		WithControlPlaneNamespace(namespace)
}

// controlPlaneCluster builds the control-plane database's `Cluster` with the given ready count, and
// with or without the pgBackRest plugin that ships its write-ahead log off-cluster.
func controlPlaneCluster(namespace string, ready int64, archiving bool) *unstructured.Unstructured {
	spec := map[string]any{"instances": int64(1)}
	if archiving {
		spec["plugins"] = []any{map[string]any{
			"name": kube.PgBackRestPluginName, "enabled": true, "isWALArchiver": true,
		}}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kube.CNPGAPIGroup + "/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": kube.ControlPlaneDatabaseName, "namespace": namespace},
		"spec":       spec,
		"status":     map[string]any{"readyInstances": ready},
	}}
}

// plainDatabase builds the Deployment a `--database plain` install runs its database as.
func plainDatabase(namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: kube.ControlPlaneDatabaseName, Namespace: namespace},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
}
