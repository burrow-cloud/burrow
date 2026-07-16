// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// toClientCaps converts the control-plane capability report (returned by the kubeconfig-side probe
// in `burrow install`) into the client DTO the summary/render helpers take. The two are
// structurally identical; this keeps a single set of render helpers for both the install summary
// and the `burrow cluster` view.
func toClientCaps(c controlplane.ClusterCapabilities) client.ClusterCapabilities {
	return client.ClusterCapabilities{
		Ingress:      client.IngressCapability{Present: c.Ingress.Present, Classes: c.Ingress.Classes},
		Storage:      client.StorageCapability{DefaultPresent: c.Storage.DefaultPresent, DefaultClass: c.Storage.DefaultClass, Classes: c.Storage.Classes},
		LoadBalancer: client.LoadBalancerCapability{Supported: c.LoadBalancer.Supported, Inferred: c.LoadBalancer.Inferred, Provider: c.LoadBalancer.Provider},
		CertManager:  client.CertManagerCapability{Present: c.CertManager.Present},
		Provider:     client.ProviderCapability{Cloud: c.Provider.Cloud, Name: c.Provider.Name},
		DNS:          client.DNSCapability{Configured: c.DNS.Configured, Providers: c.DNS.Providers},
	}
}

// newClusterCmd is the single home for the cluster (ADR-0037): bare `burrow cluster` is the
// read-only view of what the cluster can do (ADR-0034) — ingress, storage, LoadBalancer support,
// cert-manager, provider, DNS, read live — and `burrow cluster ingress install` provisions the
// shared ingress/TLS infrastructure (folded in from the retired `system` group). `cluster` is the
// concrete, unambiguous noun for the Kubernetes cluster, covering both inspect and provision.
func newClusterCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Inspect what your cluster can do, and set up its shared infrastructure (ingress, TLS)",
		Long: "cluster is the home for the Kubernetes cluster Burrow runs on. With no subcommand it\n" +
			"reports the cluster's capabilities, read live: whether an ingress controller is installed\n" +
			"and which IngressClass to use, whether there is a default StorageClass for persistent\n" +
			"volumes, whether Service type=LoadBalancer is likely supported or the cluster is\n" +
			"NodePort-only, whether cert-manager is installed for TLS, the cloud provider, and whether\n" +
			"a DNS provider is configured. That view is read-only and changes nothing.\n\n" +
			"`burrow cluster ingress install` provisions the shared ingress/TLS infrastructure\n" +
			"(ingress-nginx, cert-manager, a Let's Encrypt issuer); a one-time operator setup.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			caps, err := c.Cluster(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, caps, "")
			}
			writeClusterReport(out, caps)
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.AddCommand(newIngressCmd())
	cmd.AddCommand(newBootstrapCmd())
	cmd.AddCommand(newCapacityCmd())
	return cmd
}

// newCapacityCmd is `burrow cluster capacity` (issue #275): a read-only view of the cluster's
// scheduling headroom — per node and cluster-total allocatable CPU/memory, how much is committed
// (the sum of pod resource requests), and the free headroom left — plus the top CPU and memory
// consumers and a verdict on whether a typical in-cluster build would schedule now and whether the
// cluster needs another node. It is computed from the Kubernetes API alone (node allocatable and
// pod requests), which is what actually determines scheduling and needs no metrics-server; live
// CPU/memory usage is a separate layer that installing metrics-server would add. Read-only.
func newCapacityCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "capacity",
		Short: "Show cluster scheduling headroom: allocatable vs committed, top consumers, and whether a build fits",
		Long: "capacity answers \"is my cluster at capacity, do I need to scale, and what is using the\n" +
			"most CPU/memory?\" — read live and read-only. For each node and the cluster as a whole it\n" +
			"reports allocatable CPU/memory, how much is committed (the sum of pod resource requests),\n" +
			"and the free headroom left, then lists the top CPU and memory consumers and gives a short\n" +
			"verdict on whether a typical in-cluster build would schedule now and whether another node\n" +
			"is needed.\n\n" +
			"This is scheduling headroom — pod requests vs node allocatable — computed from the\n" +
			"Kubernetes API alone. It is exactly what determines whether a pod schedules, so it needs\n" +
			"no metrics-server. Live CPU/memory usage is a separate layer; installing metrics-server\n" +
			"would add it (it is not required for this answer).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx)
			if err != nil {
				return err
			}
			report, err := c.Capacity(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, report, "")
			}
			writeCapacityReport(out, report)
			return nil
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// writeCapacityReport prints the capacity report in plain language — CPU in plain units ("½ a CPU",
// "¼ of a CPU"), memory in MB/GB — never machine units like "500m" (issue #275/#277).
func writeCapacityReport(w io.Writer, r client.CapacityReport) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tPODS\tCPU (free / allocatable)\tMEMORY (free / allocatable)")
	for _, n := range r.Nodes {
		writeCapacityRow(tw, n.Name, n)
	}
	if len(r.Nodes) != 1 {
		writeCapacityRow(tw, "cluster total", r.Cluster)
	}
	_ = tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Top CPU consumers:")
	writeConsumers(w, r.TopCPU, true)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Top memory consumers:")
	writeConsumers(w, r.TopMemory, false)

	fmt.Fprintln(w)
	fmt.Fprintln(w, r.Verdict)
	fmt.Fprintln(w)
	fmt.Fprintln(w, r.UtilizationNote)
}

// writeCapacityRow prints one node (or the cluster total) row: free vs allocatable CPU and memory,
// in plain units.
func writeCapacityRow(tw *tabwriter.Writer, label string, n client.NodeCapacity) {
	fmt.Fprintf(tw, "%s\t%d\t%s / %s\t%s / %s\n",
		label, n.Pods,
		controlplane.HumanCPU(n.FreeCPUMillis), controlplane.HumanCPU(n.AllocCPUMillis),
		controlplane.HumanMemory(n.FreeMemBytes), controlplane.HumanMemory(n.AllocMemBytes))
}

// writeConsumers prints the top-consumers list, leading with the resource the list is ranked on.
func writeConsumers(w io.Writer, consumers []client.Consumer, cpuFirst bool) {
	if len(consumers) == 0 {
		fmt.Fprintln(w, "  (none requesting resources)")
		return
	}
	for _, c := range consumers {
		lead, rest := controlplane.HumanCPU(c.CPUMillis), controlplane.HumanMemory(c.MemBytes)
		if !cpuFirst {
			lead, rest = controlplane.HumanMemory(c.MemBytes), controlplane.HumanCPU(c.CPUMillis)
		}
		fmt.Fprintf(w, "  %s/%s — %s (%s)\n", c.Namespace, c.Name, lead, rest)
	}
}

// writeClusterReport prints the capability report as an aligned, human-readable table.
func writeClusterReport(w io.Writer, caps client.ClusterCapabilities) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Ingress\t%s\n", ingressLine(caps.Ingress))
	fmt.Fprintf(tw, "Storage\t%s\n", storageLine(caps.Storage))
	fmt.Fprintf(tw, "Load balancer\t%s\n", loadBalancerLine(caps.LoadBalancer))
	fmt.Fprintf(tw, "cert-manager\t%s\n", certManagerLine(caps.CertManager))
	fmt.Fprintf(tw, "Provider\t%s\n", providerLine(caps.Provider))
	fmt.Fprintf(tw, "DNS\t%s\n", dnsLine(caps.DNS))
	_ = tw.Flush()
}

func ingressLine(i client.IngressCapability) string {
	if !i.Present {
		if len(i.Classes) > 0 {
			// An IngressClass is cluster-scoped and can outlive its controller: a lingering class
			// with no running controller routes nothing. Report the orphan honestly, not as ready.
			return "no running ingress controller (orphan IngressClass " + strings.Join(i.Classes, ", ") + ")"
		}
		return "no ingress controller (no IngressClass found)"
	}
	return "IngressClass " + strings.Join(i.Classes, ", ")
}

func storageLine(s client.StorageCapability) string {
	if !s.DefaultPresent {
		if len(s.Classes) > 0 {
			return "no default StorageClass (have: " + strings.Join(s.Classes, ", ") + ")"
		}
		return "no StorageClass found"
	}
	return "default StorageClass " + s.DefaultClass
}

func loadBalancerLine(l client.LoadBalancerCapability) string {
	if l.Supported {
		switch l.Provider {
		case "servicelb":
			return "supported (k3s servicelb, free)"
		case "metallb":
			return "supported (MetalLB, free)"
		case "":
			return "supported (inferred from the provider)"
		default:
			return "supported (" + l.Provider + " cloud load balancer, billable)"
		}
	}
	return "not detected (no LoadBalancer provider; install MetalLB for a free public IP)"
}

func certManagerLine(c client.CertManagerCapability) string {
	if c.Present {
		return "installed"
	}
	return "not installed"
}

func providerLine(p client.ProviderCapability) string {
	if p.Name != "" {
		return p.Name
	}
	return "unknown (bare-metal or unrecognized)"
}

func dnsLine(d client.DNSCapability) string {
	if d.Configured {
		return "configured (" + strings.Join(d.Providers, ", ") + ")"
	}
	return "no DNS provider configured"
}

// capabilitySummary renders the one-line capability summary `burrow install` prints after install
// (ADR-0034), e.g. "nginx IngressClass · default StorageClass do-block-storage · provider
// DigitalOcean · cert-manager not installed". It names what is present and flags what is missing,
// without nagging — a cron-only cluster needs no ingress.
func capabilitySummary(caps client.ClusterCapabilities) string {
	var parts []string

	if caps.Ingress.Present {
		parts = append(parts, strings.Join(caps.Ingress.Classes, ",")+" IngressClass")
	} else {
		parts = append(parts, "no ingress controller")
	}

	if caps.Storage.DefaultPresent {
		parts = append(parts, "default StorageClass "+caps.Storage.DefaultClass)
	} else {
		parts = append(parts, "no default StorageClass")
	}

	if caps.Provider.Name != "" {
		parts = append(parts, "provider "+caps.Provider.Name)
	} else {
		parts = append(parts, "provider unknown")
	}

	if caps.CertManager.Present {
		parts = append(parts, "cert-manager installed")
	} else {
		parts = append(parts, "cert-manager not installed")
	}

	return strings.Join(parts, " · ")
}
