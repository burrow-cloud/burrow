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
		LoadBalancer: client.LoadBalancerCapability{Supported: c.LoadBalancer.Supported, Inferred: c.LoadBalancer.Inferred},
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
	return cmd
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
		return "supported (inferred from the provider)"
	}
	return "NodePort only (no cloud load balancer inferred)"
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
