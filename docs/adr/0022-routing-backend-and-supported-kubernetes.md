# ADR-0022: HTTP routing via a shared ingress (Ingress now, Gateway-ready) and supported Kubernetes versions

## Status

Accepted. Refines [ADR-0018](0018-reaching-an-app-at-a-url.md) with the routing-backend and
supported-version details surfaced while planning the v0.2 URL work.

## Context

Reaching a deployed app at a URL needs an HTTP entry point into the cluster. Three facts
shape the choice:

1. **Cost.** A `Service type=LoadBalancer` provisions one cloud load balancer per Service —
   on the reference target (DigitalOcean) a flat monthly fee each, plus traffic. One LB per
   app does not scale cost-wise.
2. **API maturity.** The `Ingress` API (`networking.k8s.io/v1`) is GA and present on every
   cluster since Kubernetes 1.19, with no CRDs to install. The **Gateway API** is its
   feature-frozen successor — more expressive and the direction the ecosystem is moving — but
   it requires its CRDs *and* a Gateway-capable controller to be installed first, which a
   vanilla or older cluster does not have.
3. **Audience.** The first user knows little about Kubernetes and wants their app reachable at
   their domain with a valid TLS cert. They should not have to know what an Ingress or a
   Gateway is. An advanced user — or the agent — should still be able to see what exists and
   what can change.

## Decision

**Route HTTP through a single shared ingress controller, not a load balancer per app.** The
controller owns one `LoadBalancer` Service (one cloud LB, one flat fee); every exposed host
is routed through it by Host header. This is why `expose` creates a ClusterIP Service + an
Ingress rather than a `LoadBalancer` Service ([ADR-0018](0018-reaching-an-app-at-a-url.md)).

**Backend: Ingress now, Gateway API behind the same seam later.** v0.2 targets
`networking.k8s.io/v1` Ingress because it is universal and needs no extra install. The
`expose` operation and the reachability surface are the abstraction the user and agent see;
the Ingress-vs-Gateway choice lives behind them. Burrow **detects what the cluster supports**
and uses it; a later release can add a Gateway backend without changing the `expose` surface.
The user never types "Ingress" or "Gateway."

**Supported Kubernetes versions.** Burrow officially supports the **upstream-supported window
— roughly the latest three minor releases — and whatever DigitalOcean offers**, with
**`networking.k8s.io/v1` (Kubernetes 1.19+) as the hard API floor**: nothing Burrow uses
(Deployments, Services, Ingress v1, ServiceAccount tokens, the API-server service proxy)
requires anything newer than 1.19, so older clusters likely work but are not a support
commitment. The concrete floor is documented and revisited each release; adding a Gateway
backend later raises the requirement for users who opt into it (CRDs + a Gateway controller).

**Layered introspection (novice / advanced / agent).** Burrow exposes the reachability chain
at two altitudes from the same facts: a one-line, k8s-free verdict for the novice ("not
reachable yet — DNS isn't pointing at your cluster; do X") and the full structured chain
(controller, external address, Ingress, cert, DNS) for advanced users and the agent. The
**agent gauges the user's technical aptitude and decides how much to surface** — Burrow
returns the layered data, the agent renders it. The reachability surface (ADR-0018) is the
"what exists" half; capability detection (which controller, Ingress vs Gateway, TLS issuer)
is the "what can change" half.

## Consequences

- One cloud load balancer fronts all exposed apps — a single flat baseline cost, not
  per-app. The controller's LB is the one place an external address is allocated, which the
  reachability surface reads and reports.
- The `expose` / reachability surfaces are the stable contract; the routing backend is an
  implementation detail Burrow detects and may evolve (Ingress → Gateway) without breaking
  them.
- A documented supported-version policy sets expectations and bounds testing (the k3d
  integration job pins a representative supported version).
- Structured, layered reachability output is a requirement on the reachability surface: it
  must carry both a plain summary and the full chain so the agent can choose the altitude.

## Rejected alternatives

- **A `LoadBalancer` Service per app.** Rejected: one cloud LB per app multiplies cost and
  provisioning latency; a shared ingress controller routes many hosts through one LB.
- **Gateway API as the v0.2 backend.** Deferred: it needs CRDs and a Gateway controller a
  plain cluster lacks, raising setup friction for the first user; Ingress is universal today.
  The seam keeps Gateway a later, non-breaking addition.
- **Expose the Ingress/Gateway choice in the CLI/MCP surface.** Rejected for the default
  path: the novice should not need to choose; Burrow detects and uses what is available, and
  advanced users/the agent can inspect and override.
- **Support every Kubernetes version back to Ingress v1beta1 (pre-1.19).** Rejected: the
  v1beta1 Ingress API differs and is long removed; pinning the floor at `networking.k8s.io/v1`
  keeps one code path.
