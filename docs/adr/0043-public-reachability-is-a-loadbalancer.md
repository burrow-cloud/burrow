# ADR-0043: Public reachability is a LoadBalancer, not NodePort

## Status

✅ Accepted

## TL;DR

Making an app publicly reachable exposes the shared ingress controller through a
`type: LoadBalancer` Service, which yields a real public IP to point DNS at. That is universal:
a cloud provider, k3s's built-in servicelb, or MetalLB all provide it. Internal traffic stays on
ClusterIP (already [ADR-0041](0041-flatten-path-to-a-reachable-app.md)'s Service-by-default).
**NodePort is dropped as a user-facing choice** because it exposes the controller on high ports,
not :80/:443, so it cannot serve a turnkey public site and defeats Let's Encrypt HTTP-01, and it
is unnecessary for internal traffic. Capability detection must report LoadBalancer support from
**whatever actually services LoadBalancers** (a recognized cloud provider, servicelb, or MetalLB),
not only a known cloud, because an e2e proved a LoadBalancer gets a real external IP on k3s's
servicelb while detection wrongly reported it unsupported. When no LoadBalancer provider exists,
Burrow guides the user to install MetalLB, not NodePort. Refines
[ADR-0034](0034-agent-native-onboarding.md); complements
[ADR-0042](0042-use-existing-ingress-controller.md); relates to
[ADR-0041](0041-flatten-path-to-a-reachable-app.md) and
[ADR-0022](0022-routing-backend-and-supported-kubernetes.md). Supersedes nothing.

## Context

ADR-0034 offered a LoadBalancer-versus-NodePort choice at `burrow cluster ingress install`.
NodePort is the free path, but it exposes the ingress controller on a **NodePort Service (high
ports, 30000-32767)**, not on :80/:443. DNS has no concept of a port: an A record maps a name to
an IP, and a browser opening `https://host` always connects on :443. So a NodePort-exposed
controller cannot serve a turnkey public website, and Let's Encrypt's HTTP-01 challenge (fetched
on :80) fails. NodePort is only useful when the operator brings their own front door on :80/:443
(a reverse proxy, a CDN, or an external load balancer), which Burrow's non-Kubernetes audience
neither has nor wants to reason about. And it is unnecessary for internal service-to-service
traffic, which ClusterIP (ADR-0041's default Service) already covers.

So the two real needs are: **public reachability wants a public IP; internal wants a stable
in-cluster address.** NodePort serves neither well and sits confusingly between them.

A `type: LoadBalancer` Service provides a public IP on every target Burrow supports: on a cloud
(DigitalOcean, EKS) a billable cloud load balancer; on k3s/k3d the built-in **servicelb**
(klipper), which binds node host-ports and assigns the node IP; on bare metal **MetalLB**. An
ephemeral-cluster test ([#193]) confirmed this empirically: on k3s with no cloud provider, a
LoadBalancer Service received the real external address `172.18.0.2` from servicelb. The same test
also confirmed a gap: Burrow's capability detection sets `LoadBalancer.Supported` only from a
recognized cloud provider's node `providerID`, so on k3s it reported `Supported=false` even though
the LoadBalancer worked. A LoadBalancer-centric model therefore requires detection to recognize
non-cloud LoadBalancer providers.

## Decision

1. **Public reachability is a LoadBalancer.** Making an app publicly reachable exposes the shared
   ingress controller via a `type: LoadBalancer` Service, yielding a real public IP to point DNS
   at. This is the same concept on cloud, k3s, and bare metal.
2. **Internal is ClusterIP.** Already ADR-0041's Service-by-default; apps reach each other by
   in-cluster DNS. No NodePort is needed for internal traffic.
3. **Drop NodePort as a user-facing choice.** Remove the `--expose nodeport` option and the
   LoadBalancer-versus-NodePort prompt from the CLI and the agent guidance. NodePort cannot serve
   a turnkey public site and is unnecessary for internal use.
4. **Detect LoadBalancer capability by what services LoadBalancers, not just a known cloud.**
   Capability detection reports LoadBalancer supported when any LoadBalancer provider is present:
   a recognized cloud provider, k3s's servicelb, or MetalLB. It must not report unsupported on a
   cluster where a LoadBalancer Service in fact gets an address (the [#193] gap).
5. **When no LoadBalancer provider exists, guide MetalLB.** On a bare cluster with no cloud LB and
   no servicelb, the answer is to install MetalLB (a free bare-metal LoadBalancer that assigns a
   real IP), surfaced as a structured prerequisite ([ADR-0006](0006-guardrails-in-the-control-plane.md)).
   Never NodePort.
6. **Cost disclosure is scoped to billable LoadBalancers.** A cloud load balancer is billable, so
   it is disclosed and gated with `--approve`; servicelb and MetalLB are free and need no cost
   approval. This generalizes the `--approve` gate from "the LoadBalancer path" to "a billable
   LoadBalancer."

## Consequences

- Public exposure is one concept, a LoadBalancer giving a public IP, identical on cloud, k3s, and
  bare metal; the user never chooses NodePort or reasons about node ports.
- LoadBalancer detection must be extended to recognize servicelb and MetalLB (the confirmed gap);
  until it is, k3s/bare-metal clusters are wrongly told LoadBalancer is unsupported.
- `--expose nodeport` is removed from the CLI; the agent guidance no longer offers it. The
  `--approve` cost gate keys off "billable" (a cloud LB), not "LoadBalancer," so a free servicelb
  or MetalLB install needs no approval.
- k3s, k3d, homelab, and VPS self-hosters, a core slice of the audience, get a working free public
  IP via servicelb, with no cloud load balancer and no second ingress controller.
- The [#193] e2e locks servicelb LoadBalancer behavior in as regression coverage for the
  bare-metal path.

## Rejected alternatives

- **Keep NodePort as the free option.** It exposes high ports, not :80/:443, so it cannot serve a
  turnkey public site and breaks HTTP-01, and it confuses the target user. The free option that
  actually works is a LoadBalancer via servicelb or MetalLB, which yields a real IP.
- **Make NodePort work by binding the node's :80/:443 via hostPort/hostNetwork.** That is exactly
  the plumbing servicelb and MetalLB manage as a LoadBalancer, and it is proven (servicelb uses
  host-ports under the hood). Exposing raw hostPort to users is a leakier, single-node abstraction;
  give them a LoadBalancer instead.
- **Detect LoadBalancer support by creating a probe LoadBalancer Service.** Side-effecting and slow
  for a read-only capability survey; detect the provider (cloud, servicelb, or MetalLB) instead.
- **Require a cloud provider for public exposure.** Excludes k3s/bare-metal self-hosters who can
  serve a public IP for free via servicelb/MetalLB, which contradicts the self-host audience.
