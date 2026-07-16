// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.CapacityProber = (*Prober)(nil)

// ReadResourceState reads the cluster's scheduling-capacity facts read-only (issue #275).
func (p *Prober) ReadResourceState(ctx context.Context) (controlplane.ClusterResourceState, error) {
	return ReadResourceState(ctx, p.client)
}

// ReadResourceState reads node allocatable and pod requests read-only over the given clientset
// (issue #275): each schedulable node's .status.allocatable CPU/memory, and every non-terminal
// pod's summed resource requests with the node it is scheduled on. It performs only get/list reads —
// it never writes — and needs read on nodes (already granted for capability detection) plus a
// cluster-wide read on pods. It is a free function so the same read runs whether driven by the
// kubeconfig client or burrowd's in-cluster client. All of this comes from the Kubernetes API
// alone; no metrics-server is involved (that would add the separate live-usage layer, issue #276).
func ReadResourceState(ctx context.Context, client kubernetes.Interface) (controlplane.ClusterResourceState, error) {
	nodeList, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return controlplane.ClusterResourceState{}, fmt.Errorf("kube: listing nodes: %w", err)
	}
	nodes := make([]controlplane.NodeAllocatable, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		alloc := n.Status.Allocatable
		nodes = append(nodes, controlplane.NodeAllocatable{
			Name:      n.Name,
			CPUMillis: alloc.Cpu().MilliValue(),
			MemBytes:  alloc.Memory().Value(),
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	podList, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return controlplane.ClusterResourceState{}, fmt.Errorf("kube: listing pods: %w", err)
	}
	pods := make([]controlplane.PodRequest, 0, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		// A pod that has finished (Succeeded/Failed) reserves nothing — the scheduler has released
		// its request — so it does not count toward committed capacity.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, mem := podRequests(pod)
		pods = append(pods, controlplane.PodRequest{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Node:      pod.Spec.NodeName,
			CPUMillis: cpu,
			MemBytes:  mem,
		})
	}
	return controlplane.ClusterResourceState{Nodes: nodes, Pods: pods}, nil
}

// podRequests computes a pod's effective CPU (milli) and memory (bytes) requests the way the
// scheduler reserves them: the sum of the normal containers' requests, plus any native sidecar
// (an init container with restartPolicy=Always, which runs for the pod's whole life), taken to at
// least the peak of the ordinary init containers' requests (init containers run one at a time
// before the app containers, so the largest single init request is a floor on what the pod needs).
func podRequests(pod *corev1.Pod) (cpuMillis, memBytes int64) {
	for i := range pod.Spec.Containers {
		cpu, mem := containerRequests(&pod.Spec.Containers[i])
		cpuMillis += cpu
		memBytes += mem
	}
	var initPeakCPU, initPeakMem int64
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		cpu, mem := containerRequests(c)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			// A sidecar (restartPolicy=Always init container) stays running alongside the app
			// containers, so its request is added, not just peaked.
			cpuMillis += cpu
			memBytes += mem
			continue
		}
		if cpu > initPeakCPU {
			initPeakCPU = cpu
		}
		if mem > initPeakMem {
			initPeakMem = mem
		}
	}
	if initPeakCPU > cpuMillis {
		cpuMillis = initPeakCPU
	}
	if initPeakMem > memBytes {
		memBytes = initPeakMem
	}
	return cpuMillis, memBytes
}

// containerRequests returns a container's CPU (milli) and memory (bytes) requests, zero when unset.
func containerRequests(c *corev1.Container) (cpuMillis, memBytes int64) {
	req := c.Resources.Requests
	return req.Cpu().MilliValue(), req.Memory().Value()
}
