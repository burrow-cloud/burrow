# ADR-0046: Registry onboarding — auto-wire the code provider's registry first; an in-cluster registry for the no-cost self-hoster

## Status

🟡 Proposed

<!-- Held deliberately at Proposed: the decision shape is recorded, but building waits on a
signal from users that registry onboarding is a real friction point (see Context). -->

## TL;DR

Reduce the friction of getting a built image into a registry, ranked by who the user is.
**Primary path: the developer's existing code-provider registry** (GitHub Container Registry,
GitLab Container Registry), auto-wired by burrow using the code-host token the user already
grants — target the package registry, configure the laptop's `docker login`, and mint the
cluster pull secret, so the "go set up an external registry" step disappears without bundling
anything. **Second path, for the no-added-cost self-hoster: an in-cluster registry (zot)
published at a subdomain** with a real Let's Encrypt certificate and auth, reached through the
ingress a VPS already fronts for free (servicelb) — native throughput, nothing to distribute, no
cloud LoadBalancer. A developer who will pay for a cloud LoadBalancer almost certainly already
uses their code provider's registry, so the in-cluster option is specifically the zero-cost VPS
case, not a general default. Builds on [ADR-0017](0017-private-registry-authentication.md) (pull
secret), [ADR-0023](0023-provider-credentials.md) / [ADR-0030](0030-credentials-through-the-control-plane.md)
(provider credentials through the control plane), [ADR-0004](0004-code-never-over-mcp.md) (the
registry is the conveyor belt), and [ADR-0040](0040-burrowd-never-contacts-the-registry.md)
(burrowd never contacts the registry). **Proposed, not scheduled** — it captures the decision
while we wait to learn whether users actually find registry onboarding complicated. Supersedes
nothing.

## Context

Burrow's positioning is "one box, point your agent at it, deploy" ([ADR-0044](0044-single-vps-k3s-cluster.md)).
The built image has to live in a container registry ([ADR-0004](0004-code-never-over-mcp.md)),
and today that means an external one the developer sets up by hand: create an account, log in,
provision a pull secret ([ADR-0017](0017-private-registry-authentication.md)). Dogfooding surfaced
that "registry dance" as the sharpest-feeling step in the flow, which prompted an instinct to
bundle a registry into the cluster.

Two observations reshape that instinct:

- **Most agent-driven developers already host code on GitHub or GitLab**, both of which include a
  container registry (GHCR, GitLab Container Registry). For them the registry already exists; the
  friction is not "there is no registry" but "wiring up credentials by hand." That is plumbing
  burrow can automate against a registry the user already has, without bundling anything.
- **Cost sensitivity splits the audience.** A developer willing to pay for a cloud LoadBalancer is
  already comfortable in the GHCR/GitLab world and will use it. The user who needs a *zero-added-cost*
  path is the single-VPS self-hoster, where servicelb makes the node IP a free LoadBalancer and a
  cloud LB is exactly the cost to avoid. So an in-cluster registry is not a mainstream default; it
  is the no-cost fallback for that specific user.

This ADR is therefore held at **Proposed**. It is not yet clear that registry onboarding is
painful enough to justify building either the auto-wiring or the in-cluster registry. The
decision shape is recorded so the conversation is not lost; the trigger to implement is a signal
from users that the onboarding is complicated.

## Decision

### 1. Primary path — auto-wire the code provider's registry

Given the code-host token the user already grants (with a package/registry write scope), burrow
does the setup the developer would otherwise do by hand: it targets the right registry namespace
(`ghcr.io/<owner>/...` or the GitLab project registry), configures `docker login` on the laptop
(writing user-owned `~/.docker/config.json`, no root), and mints the cluster imagePullSecret
([ADR-0017](0017-private-registry-authentication.md)). Those provider credentials travel through
the control plane, never MCP ([ADR-0023](0023-provider-credentials.md) / [ADR-0030](0030-credentials-through-the-control-plane.md)).

The "set up an external registry" step collapses into "grant a scope on a token you already use."
The honest cost is that a package-write scope (GitHub `write:packages`, the GitLab registry scope)
is broad-ish, so asking for it is a real trust ask: it is opt-in and scoped as narrowly as the
provider allows.

### 2. Second path — an in-cluster registry published at a subdomain

For the user who wants everything self-contained and pays for no cloud infrastructure, burrow
runs **zot** (a lightweight, OCI-native registry) in-cluster and **publishes it at
`registry.<domain>`** through the ingress, with a real Let's Encrypt certificate (cert-manager)
and production auth. This reuses the exact publish machinery apps already use; on a VPS, servicelb
fronts the ingress on the node IP for free, so it is a DNS record, not a paid LoadBalancer.

This path is deliberately the one that:

- **stays stable for large images.** Traffic goes node → ingress → zot at native line speed and
  never transits the Kubernetes API server, so a 1GB Java image pushes without pressuring a
  small single-node control plane.
- **hides its own security.** The real certificate means there is nothing to distribute to the
  developer's Docker; burrow mints the auth credential and runs `docker login`. The only
  prerequisite — a domain plus the ingress/cert-manager stack — is what a public HTTPS app needs
  anyway.

An **optional cold-start escape** covers the first deploy before a domain or ingress exists, and
small images: a burrow-orchestrated `port-forward` push to a `localhost` registry needs no
certificate and opens no public surface (Docker trusts `localhost`). It is explicitly **not for
large images**, because it streams through the API server and pressures a resource-constrained
single node.

### 3. Scope and positioning

The in-cluster registry is the **no-added-cost path, not a general default** — anyone with budget
for a cloud LoadBalancer uses path 1. If the registry is bundled, zot is the engine (light,
OCI-native, fits the 2GB floor); Harbor is too heavy and is available only as a connect /
bring-your-own option.

## Consequences

- **If path 1 lands, much of the original "bundle a registry" motivation evaporates** — the
  friction was largely credential plumbing burrow can hide against a registry the user already
  has. That is a good reason to build path 1 first and treat the in-cluster registry as secondary.
- **The in-cluster registry stays a real but niche option.** Its cost is a new (auth- and
  TLS-protected) surface plus disk and garbage-collection management, justified only for the
  zero-cost self-hoster.
- **The invariants hold in both paths.** The registry stays the conveyor belt
  ([ADR-0004](0004-code-never-over-mcp.md)); burrowd never contacts it
  ([ADR-0040](0040-burrowd-never-contacts-the-registry.md)) — the laptop pushes and the kubelet
  pulls; and provider credentials traverse the control plane, never MCP
  ([ADR-0030](0030-credentials-through-the-control-plane.md)).
- **Honest status** ([ADR-0009](0009-honest-status.md)): Proposed and unscheduled; nothing here is
  advertised as shipped. The trigger to build is user signal that onboarding is painful — validate
  before implementing.

## Rejected alternatives

- **A self-signed NodePort registry.** The laptop-side CA trust cannot be hidden without either
  elevated privilege the developer must approve (root for `/etc/docker/certs.d`, or a keychain
  entry plus a Docker restart) or disabling TLS verification, which is not secure. It is dominated
  on both friction and security, so it is dropped from the design.
- **Control-plane-mediated push (burrowd streams the image).** This would make burrowd a
  data-plane conduit, force bulk image bytes through the API-server proxy (not built for bulk
  transfer), and violate the spirit of [ADR-0040](0040-burrowd-never-contacts-the-registry.md).
- **A `type=LoadBalancer` registry.** Mints a billable cloud LoadBalancer on a managed cluster,
  contradicting the cheap-tier thesis ([ADR-0043](0043-public-reachability-is-a-loadbalancer.md));
  the subdomain-via-servicelb path is free on a VPS.
- **Harbor as the bundled engine.** A multi-component system with its own Postgres and Redis,
  wanting several GB; it cannot fit the 2GB floor. It remains available as a connect /
  bring-your-own option (mirroring [ADR-0026](0026-observability-query-adapters.md)'s
  install-vs-connect split) for teams that want scanning, RBAC, and a UI.
- **Bundling an in-cluster registry as the primary answer.** Over-indexes on self-containment; for
  most users the code-provider registry already exists and only needs wiring, so bundling is the
  secondary, no-cost-self-hoster path rather than the default.
