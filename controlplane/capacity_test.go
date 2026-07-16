// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// capacityEngine builds an engine wired only with the fake CapacityProber (plus the required
// seams) for the scheduling-headroom tests.
func capacityEngine(t *testing.T, state cp.ClusterResourceState) *cp.Engine {
	t.Helper()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: fake.NewDatabase(),
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		CapacityProber: fake.NewCapacityProber(state),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestClusterCapacityHeadroomAndConsumers drives a two-node cluster with known allocatable and pod
// requests through the engine and asserts the computed per-node and cluster headroom, the top
// consumers ranked by CPU and by memory, and the build-fit verdict.
func TestClusterCapacityHeadroomAndConsumers(t *testing.T) {
	state := cp.ClusterResourceState{
		Nodes: []cp.NodeAllocatable{
			// node-a: 1 CPU / 2 GiB allocatable.
			{Name: "node-a", CPUMillis: 1000, MemBytes: 2 * gib},
			// node-b: 2 CPU / 4 GiB allocatable.
			{Name: "node-b", CPUMillis: 2000, MemBytes: 4 * gib},
		},
		Pods: []cp.PodRequest{
			// node-a is nearly full: 900m / 1.75 GiB committed → 100m / 256 MiB free.
			{Namespace: "kube-system", Name: "cilium", Node: "node-a", CPUMillis: 600, MemBytes: 1 * gib},
			{Namespace: "default", Name: "website", Node: "node-a", CPUMillis: 300, MemBytes: 768 * mib},
			// node-b is light: 250m / 512 MiB committed → 1750m / 3.5 GiB free.
			{Namespace: "burrow", Name: "burrowd", Node: "node-b", CPUMillis: 250, MemBytes: 512 * mib},
			// An unscheduled (Pending) pod reserves nothing but is still a consumer.
			{Namespace: "default", Name: "pending-build", Node: "", CPUMillis: 250, MemBytes: 512 * mib},
		},
	}
	report, err := capacityEngine(t, state).ClusterCapacity(context.Background())
	if err != nil {
		t.Fatalf("ClusterCapacity: %v", err)
	}

	if len(report.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(report.Nodes))
	}
	a := report.Nodes[0]
	if a.Name != "node-a" || a.UsedCPUMillis != 900 || a.FreeCPUMillis != 100 {
		t.Errorf("node-a CPU: used=%d free=%d, want used=900 free=100", a.UsedCPUMillis, a.FreeCPUMillis)
	}
	if a.UsedMemBytes != 1*gib+768*mib || a.FreeMemBytes != 2*gib-(1*gib+768*mib) {
		t.Errorf("node-a mem: used=%d free=%d", a.UsedMemBytes, a.FreeMemBytes)
	}
	if a.Pods != 2 {
		t.Errorf("node-a pods = %d, want 2", a.Pods)
	}

	// Cluster totals sum the two nodes; the Pending pod contributes to neither committed total.
	if report.Cluster.AllocCPUMillis != 3000 || report.Cluster.UsedCPUMillis != 1150 || report.Cluster.FreeCPUMillis != 1850 {
		t.Errorf("cluster CPU = %+v, want alloc=3000 used=1150 free=1850", report.Cluster)
	}
	if report.Cluster.Pods != 3 {
		t.Errorf("cluster pods = %d, want 3 (Pending excluded from committed)", report.Cluster.Pods)
	}

	// Top CPU: cilium (600) > website (300) > burrowd/pending (250, tie broken by namespace/name).
	if got := report.TopCPU[0].Name; got != "cilium" {
		t.Errorf("top CPU[0] = %q, want cilium", got)
	}
	if got := report.TopCPU[1].Name; got != "website" {
		t.Errorf("top CPU[1] = %q, want website", got)
	}
	// Top memory: cilium (1 GiB) > website (768 MiB) > burrowd/pending (512 MiB).
	if got := report.TopMemory[0].Name; got != "cilium" {
		t.Errorf("top memory[0] = %q, want cilium", got)
	}

	// A quarter-CPU / 512 MiB build does NOT fit on node-a (only 100m free) but DOES on node-b.
	if !report.BuildFits || report.BuildFitsNode != "node-b" {
		t.Errorf("build fit = %v on %q, want true on node-b", report.BuildFits, report.BuildFitsNode)
	}
	if !strings.Contains(report.Verdict, "node-b") {
		t.Errorf("verdict does not name the fitting node: %q", report.Verdict)
	}
	if report.UtilizationNote == "" || !strings.Contains(report.UtilizationNote, "metrics-server") {
		t.Errorf("utilization note should mention metrics-server: %q", report.UtilizationNote)
	}
}

// TestClusterCapacityBuildDoesNotFit asserts the verdict when a fully-committed single node leaves
// no room for a build — the issue #274 case: the build would sit Pending.
func TestClusterCapacityBuildDoesNotFit(t *testing.T) {
	state := cp.ClusterResourceState{
		Nodes: []cp.NodeAllocatable{{Name: "only", CPUMillis: 920, MemBytes: 1500 * mib}},
		Pods: []cp.PodRequest{
			{Namespace: "kube-system", Name: "overhead", Node: "only", CPUMillis: 820, MemBytes: 1100 * mib},
		},
	}
	report, err := capacityEngine(t, state).ClusterCapacity(context.Background())
	if err != nil {
		t.Fatalf("ClusterCapacity: %v", err)
	}
	if report.BuildFits {
		t.Errorf("build should not fit: node free CPU=%d mem=%d", report.Nodes[0].FreeCPUMillis, report.Nodes[0].FreeMemBytes)
	}
	if !strings.Contains(report.Verdict, "NOT schedule") || !strings.Contains(strings.ToLower(report.Verdict), "add a node") {
		t.Errorf("verdict should say the build will not schedule and to add a node: %q", report.Verdict)
	}
}

// TestClusterCapacityNotConfigured asserts the clean ErrNotImplemented when no CapacityProber is
// wired — the optional-seam contract.
func TestClusterCapacityNotConfigured(t *testing.T) {
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: fake.NewDatabase(),
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.ClusterCapacity(context.Background()); !errors.Is(err, cp.ErrNotImplemented) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
}

// TestHumanCPU asserts the plain-language CPU rendering — the README convention (never "500m").
func TestHumanCPU(t *testing.T) {
	cases := map[int64]string{
		0:    "no CPU",
		100:  "a tenth of a CPU",
		250:  "¼ of a CPU",
		333:  "⅓ of a CPU",
		500:  "½ a CPU",
		667:  "⅔ of a CPU",
		750:  "¾ of a CPU",
		1000: "1 CPU",
		2000: "2 CPUs",
		1500: "1½ CPUs",
		1250: "1¼ CPUs",
		40:   "under a tenth of a CPU",
		620:  "about ⅔ of a CPU", // a DOKS node's system overhead reads in plain language
	}
	for millis, want := range cases {
		if got := cp.HumanCPU(millis); got != want {
			t.Errorf("HumanCPU(%d) = %q, want %q", millis, got, want)
		}
	}
}

// TestHumanMemory asserts MB/GB rendering matching the manifests' convention (512Mi → "512 MB").
func TestHumanMemory(t *testing.T) {
	cases := map[int64]string{
		0:          "no memory",
		512 * mib:  "512 MB",
		2 * gib:    "2 GB",
		160 * mib:  "160 MB",
		1536 * mib: "1.5 GB",
		64 * mib:   "64 MB",
	}
	for bytes, want := range cases {
		if got := cp.HumanMemory(bytes); got != want {
			t.Errorf("HumanMemory(%d) = %q, want %q", bytes, got, want)
		}
	}
}
