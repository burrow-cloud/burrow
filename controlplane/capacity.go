// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"sort"
)

// A typical in-cluster build's resource request (ADR-0053): a quarter of a CPU and 512 MB. The
// capacity verdict answers whether this would currently schedule, since a Pending build on a
// fully-committed node is the concrete pain the capacity surface exists to pre-empt (issue #274).
const (
	buildRequestCPUMillis int64 = 250
	buildRequestMemBytes  int64 = 512 * 1024 * 1024
)

// topConsumersLimit caps how many top CPU / memory consumers the report lists — enough to see what
// dominates a node without dumping every pod.
const topConsumersLimit = 5

// NodeAllocatable is one node's schedulable capacity, read straight from its .status.allocatable
// (the Kubernetes API's own scheduling budget). CPUMillis is in milli-CPU (1000 = one core);
// MemBytes is bytes. It carries no live-usage figure — allocatable is what the scheduler divides
// among pod requests, and needs no metrics-server.
type NodeAllocatable struct {
	Name      string
	CPUMillis int64
	MemBytes  int64
}

// PodRequest is one pod's summed resource requests and the node it is scheduled on. It is what the
// scheduler actually reserves against a node's allocatable — not live usage. Node is empty for a
// pod that has not been scheduled (e.g. Pending for lack of room); such a pod reserves nothing yet
// but still shows as a consumer of demand. CPUMillis is milli-CPU; MemBytes is bytes.
type PodRequest struct {
	Namespace string
	Name      string
	Node      string
	CPUMillis int64
	MemBytes  int64
}

// ClusterResourceState is the raw scheduling-capacity facts read from the Kubernetes API alone
// (node allocatable and the per-pod sum of resource requests) — no metrics-server involved. The
// engine turns it into a CapacityReport; keeping the seam's output raw keeps the headroom, top
// consumers, and verdict math pure and unit-testable against a fake.
type ClusterResourceState struct {
	Nodes []NodeAllocatable
	Pods  []PodRequest
}

// CapacityProber reads the cluster's scheduling-capacity facts read-only over the Kubernetes API:
// each node's allocatable and every pod's summed requests, with no metrics-server required
// (issue #275). It is the seam over those reads so the engine's headroom/verdict logic stays pure
// and unit-testable against a fake. The production adapter (controlplane/kube) wraps a client-go
// clientset. It is an OPTIONAL seam — nil is allowed, and a capacity read errors cleanly
// (ErrNotImplemented) when it is not wired. It never writes.
type CapacityProber interface {
	// ReadResourceState reads node allocatable and pod requests read-only. It never writes.
	ReadResourceState(ctx context.Context) (ClusterResourceState, error)
}

// NodeCapacity is the allocatable / committed / free-headroom breakdown for one node, or — when
// Name is empty in CapacityReport.Cluster — the cluster-wide total. Committed is the sum of the
// resource requests of the pods scheduled here; Free is Allocatable minus Committed, the room the
// scheduler still has to place a new pod. All CPU figures are milli-CPU, memory figures bytes.
type NodeCapacity struct {
	Name           string `json:"name,omitempty"`
	Pods           int    `json:"pods"`
	AllocCPUMillis int64  `json:"alloc_cpu_millis"`
	UsedCPUMillis  int64  `json:"committed_cpu_millis"`
	FreeCPUMillis  int64  `json:"free_cpu_millis"`
	AllocMemBytes  int64  `json:"alloc_mem_bytes"`
	UsedMemBytes   int64  `json:"committed_mem_bytes"`
	FreeMemBytes   int64  `json:"free_mem_bytes"`
}

// Consumer is one pod's contribution to the committed total, for the top-consumers lists. CPUMillis
// is milli-CPU; MemBytes is bytes. It is a request figure, not live usage.
type Consumer struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"`
	CPUMillis int64  `json:"cpu_millis"`
	MemBytes  int64  `json:"mem_bytes"`
}

// CapacityReport answers "is my cluster at capacity, do I need to scale, and what is using the most
// CPU/memory?" from the Kubernetes API alone (issue #275). It reports, per node and cluster-total,
// allocatable / committed (sum of requests) / free headroom; the top consumers by CPU and by memory
// request; and a short plain-language verdict — whether a typical in-cluster build would currently
// schedule and whether another node is needed. It is SCHEDULING headroom (requests vs allocatable),
// which is what determines whether a pod schedules and needs no metrics-server; live utilization is
// a separate layer (see UtilizationNote, issue #276).
type CapacityReport struct {
	// Nodes is the per-node breakdown, one entry per schedulable node.
	Nodes []NodeCapacity `json:"nodes"`
	// Cluster is the cluster-wide total (its Name is empty).
	Cluster NodeCapacity `json:"cluster"`
	// TopCPU lists the pods requesting the most CPU, most first; TopMemory the most memory.
	TopCPU    []Consumer `json:"top_cpu"`
	TopMemory []Consumer `json:"top_memory"`
	// BuildCPUMillis / BuildMemBytes are the resource request the verdict pre-flights (a typical
	// in-cluster build: a quarter of a CPU, 512 MB).
	BuildCPUMillis int64 `json:"build_cpu_millis"`
	BuildMemBytes  int64 `json:"build_mem_bytes"`
	// BuildFits is true when at least one node has enough free CPU AND memory at once to schedule
	// that build. A build pod lands on a single node, so headroom split across nodes does not count.
	BuildFits bool `json:"build_fits"`
	// BuildFitsNode names the node the build would schedule on when BuildFits is true.
	BuildFitsNode string `json:"build_fits_node,omitempty"`
	// Verdict is the short, plain-language recommendation (plain CPU units, memory in MB/GB).
	Verdict string `json:"verdict"`
	// UtilizationNote records that this is scheduling headroom only and what metrics-server would add.
	UtilizationNote string `json:"utilization_note"`
}

// utilizationNote is the fixed note (issue #275/#276) that the report is scheduling headroom from
// the Kubernetes API alone — the answer to "will this schedule" needs nothing more — and that live
// CPU/memory usage is a separate layer that metrics-server would add.
const utilizationNote = "This is scheduling headroom (pod requests vs node allocatable), read from the Kubernetes API alone — it is what decides whether a pod schedules and needs no metrics-server. Live CPU/memory usage is a separate layer; installing metrics-server would add it (it is not required for this answer)."

// ClusterCapacity reports the cluster's scheduling capacity and headroom (issue #275): per node and
// cluster-total allocatable / committed / free, the top CPU and memory consumers, and a verdict on
// whether a typical in-cluster build fits and whether another node is needed — all from the
// Kubernetes API alone, no metrics-server. It runs the read through the CapacityProber seam and
// computes the report purely, so it changes nothing in the cluster.
func (e *Engine) ClusterCapacity(ctx context.Context) (CapacityReport, error) {
	if e.capacity == nil {
		return CapacityReport{}, fmt.Errorf("cluster capacity: reading resource state is not configured: %w", ErrNotImplemented)
	}
	state, err := e.capacity.ReadResourceState(ctx)
	if err != nil {
		return CapacityReport{}, fmt.Errorf("cluster capacity: %w", err)
	}
	return computeCapacity(state), nil
}

// computeCapacity is the pure headroom math: it aggregates pod requests onto their nodes, totals
// the cluster, ranks the top CPU and memory consumers, and renders the build-fit verdict. It reads
// no clock, cluster, or I/O — everything comes from state — so it is deterministic and unit-tested
// against a fake CapacityProber.
func computeCapacity(state ClusterResourceState) CapacityReport {
	// Index nodes so pod requests aggregate onto the node they are scheduled on.
	byNode := make(map[string]*NodeCapacity, len(state.Nodes))
	nodes := make([]NodeCapacity, len(state.Nodes))
	for i, n := range state.Nodes {
		nodes[i] = NodeCapacity{Name: n.Name, AllocCPUMillis: n.CPUMillis, AllocMemBytes: n.MemBytes}
		byNode[n.Name] = &nodes[i]
	}

	var cluster NodeCapacity
	for _, p := range state.Pods {
		// Only scheduled pods reserve capacity against a node's allocatable; an unscheduled
		// (Pending) pod has no node yet and reserves nothing, though it still ranks as a consumer.
		if p.Node == "" {
			continue
		}
		nc, ok := byNode[p.Node]
		if !ok {
			// A pod on a node we did not list (e.g. a control-plane node excluded from the node
			// read) still counts toward the cluster total but has no per-node row.
			cluster.Pods++
			cluster.UsedCPUMillis += p.CPUMillis
			cluster.UsedMemBytes += p.MemBytes
			continue
		}
		nc.Pods++
		nc.UsedCPUMillis += p.CPUMillis
		nc.UsedMemBytes += p.MemBytes
	}

	for i := range nodes {
		nodes[i].FreeCPUMillis = nodes[i].AllocCPUMillis - nodes[i].UsedCPUMillis
		nodes[i].FreeMemBytes = nodes[i].AllocMemBytes - nodes[i].UsedMemBytes
		cluster.Pods += nodes[i].Pods
		cluster.AllocCPUMillis += nodes[i].AllocCPUMillis
		cluster.AllocMemBytes += nodes[i].AllocMemBytes
		cluster.UsedCPUMillis += nodes[i].UsedCPUMillis
		cluster.UsedMemBytes += nodes[i].UsedMemBytes
	}
	cluster.FreeCPUMillis = cluster.AllocCPUMillis - cluster.UsedCPUMillis
	cluster.FreeMemBytes = cluster.AllocMemBytes - cluster.UsedMemBytes

	report := CapacityReport{
		Nodes:           nodes,
		Cluster:         cluster,
		TopCPU:          topConsumers(state.Pods, byCPU),
		TopMemory:       topConsumers(state.Pods, byMem),
		BuildCPUMillis:  buildRequestCPUMillis,
		BuildMemBytes:   buildRequestMemBytes,
		UtilizationNote: utilizationNote,
	}
	report.BuildFits, report.BuildFitsNode = buildFit(nodes)
	report.Verdict = verdict(report)
	return report
}

// byCPU and byMem select which resource a consumer ranking sorts on.
type resourceAxis int

const (
	byCPU resourceAxis = iota
	byMem
)

// topConsumers ranks the pods by their request on the given axis, most first, and returns up to
// topConsumersLimit of them. Pods requesting nothing on that axis are dropped (0m/0B is not a
// "top consumer"). Ties break by namespace then name so the ranking is deterministic.
func topConsumers(pods []PodRequest, axis resourceAxis) []Consumer {
	ranked := make([]Consumer, 0, len(pods))
	for _, p := range pods {
		amount := p.CPUMillis
		if axis == byMem {
			amount = p.MemBytes
		}
		if amount <= 0 {
			continue
		}
		ranked = append(ranked, Consumer{
			Namespace: p.Namespace, Name: p.Name, Node: p.Node,
			CPUMillis: p.CPUMillis, MemBytes: p.MemBytes,
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i].CPUMillis, ranked[j].CPUMillis
		if axis == byMem {
			a, b = ranked[i].MemBytes, ranked[j].MemBytes
		}
		if a != b {
			return a > b
		}
		if ranked[i].Namespace != ranked[j].Namespace {
			return ranked[i].Namespace < ranked[j].Namespace
		}
		return ranked[i].Name < ranked[j].Name
	})
	if len(ranked) > topConsumersLimit {
		ranked = ranked[:topConsumersLimit]
	}
	return ranked
}

// buildFit reports whether a typical in-cluster build would schedule — whether any single node has
// both a quarter of a CPU and 512 MB free at once (a pod lands on one node, so headroom summed
// across nodes does not help) — and names the first such node. Nodes are considered in the order
// given, which the adapter sorts by name for a stable answer.
func buildFit(nodes []NodeCapacity) (bool, string) {
	for _, n := range nodes {
		if n.FreeCPUMillis >= buildRequestCPUMillis && n.FreeMemBytes >= buildRequestMemBytes {
			return true, n.Name
		}
	}
	return false, ""
}

// verdict renders the short, plain-language recommendation in plain CPU units and MB/GB memory
// (never "250m"): whether a typical in-cluster build fits and, when it does not, that the cluster
// needs another node or a larger one.
func verdict(r CapacityReport) string {
	build := fmt.Sprintf("A typical in-cluster build (%s, %s)", HumanCPU(r.BuildCPUMillis), HumanMemory(r.BuildMemBytes))
	if len(r.Nodes) == 0 {
		return build + " cannot be placed: no schedulable nodes were found."
	}
	if r.BuildFits {
		return fmt.Sprintf("%s would schedule now on node %s (free there: %s, %s). The cluster has headroom; no new node needed.",
			build, r.BuildFitsNode, HumanCPU(nodeFreeCPU(r, r.BuildFitsNode)), HumanMemory(nodeFreeMem(r, r.BuildFitsNode)))
	}
	return build + " would NOT schedule right now — no single node has that much CPU and memory free at once. Add a node (or resize an existing one) before running a build or scaling up."
}

// nodeFreeCPU and nodeFreeMem look up a named node's free headroom for the verdict text.
func nodeFreeCPU(r CapacityReport, name string) int64 {
	for _, n := range r.Nodes {
		if n.Name == name {
			return n.FreeCPUMillis
		}
	}
	return 0
}

func nodeFreeMem(r CapacityReport, name string) int64 {
	for _, n := range r.Nodes {
		if n.Name == name {
			return n.FreeMemBytes
		}
	}
	return 0
}
