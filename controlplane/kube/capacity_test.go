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

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

func capNode(name, cpu, mem string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
	}
}

// pod builds a scheduled pod with the given container requests. cpu/mem are quantity strings ("" = none).
func pod(namespace, name, nodeName, phase string, cpu, mem string) *corev1.Pod {
	reqs := corev1.ResourceList{}
	if cpu != "" {
		reqs[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		reqs[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: reqs}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPhase(phase)},
	}
}

// TestReadResourceState asserts the adapter reads node allocatable, sums pod container requests,
// carries the node assignment, sorts nodes by name, and drops finished (Succeeded/Failed) pods.
func TestReadResourceState(t *testing.T) {
	client := fake.NewSimpleClientset(
		capNode("node-b", "2", "4Gi"),
		capNode("node-a", "1000m", "2Gi"),
		pod("kube-system", "cilium", "node-a", "Running", "600m", "1Gi"),
		pod("default", "website", "node-a", "Running", "", ""),
		pod("burrow", "burrowd", "node-b", "Running", "100m", "128Mi"),
		pod("default", "done", "node-a", "Succeeded", "500m", "512Mi"), // finished → excluded
	)

	state, err := kube.ReadResourceState(context.Background(), client)
	if err != nil {
		t.Fatalf("ReadResourceState: %v", err)
	}

	// Nodes are sorted by name, with allocatable in canonical units (milli-CPU, bytes).
	if len(state.Nodes) != 2 || state.Nodes[0].Name != "node-a" || state.Nodes[1].Name != "node-b" {
		t.Fatalf("nodes = %+v, want sorted [node-a node-b]", state.Nodes)
	}
	if state.Nodes[0].CPUMillis != 1000 || state.Nodes[0].MemBytes != 2*(1<<30) {
		t.Errorf("node-a allocatable = %+v, want 1000m / 2Gi", state.Nodes[0])
	}

	// The finished pod is dropped; three live pods remain.
	if len(state.Pods) != 3 {
		t.Fatalf("pods = %d, want 3 (finished pod excluded)", len(state.Pods))
	}
	byName := map[string]controlplane.PodRequest{}
	for _, p := range state.Pods {
		byName[p.Name] = p
	}
	if c := byName["cilium"]; c.CPUMillis != 600 || c.MemBytes != 1<<30 || c.Node != "node-a" {
		t.Errorf("cilium = %+v, want 600m / 1Gi on node-a", c)
	}
	if w := byName["website"]; w.CPUMillis != 0 || w.MemBytes != 0 {
		t.Errorf("website requests = %+v, want zero (no requests set)", w)
	}
}

// TestReadResourceStateInitAndSidecar asserts the effective-request math: a plain init container
// floors the pod request (peak), while a native sidecar (restartPolicy=Always init) adds to it.
func TestReadResourceStateInitAndSidecar(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "n",
			InitContainers: []corev1.Container{
				// A big one-shot init container: floors the pod at 800m even though the app asks 300m.
				{Name: "migrate", Resources: reqCPU("800m")},
				// A sidecar: its 100m adds on top of the app containers.
				{Name: "proxy", RestartPolicy: &always, Resources: reqCPU("100m")},
			},
			Containers: []corev1.Container{{Name: "app", Resources: reqCPU("300m")}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(capNode("n", "2", "4Gi"), p)
	state, err := kube.ReadResourceState(context.Background(), client)
	if err != nil {
		t.Fatalf("ReadResourceState: %v", err)
	}
	// app 300m + sidecar 100m = 400m, floored up to the 800m one-shot init peak → 800m.
	if len(state.Pods) != 1 || state.Pods[0].CPUMillis != 800 {
		t.Errorf("effective CPU = %v, want 800m", state.Pods)
	}
}

func reqCPU(cpu string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}
}
