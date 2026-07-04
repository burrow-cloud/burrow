# ADR-0046: A bundled in-cluster registry for quick starts — zot, NodePort push, containerd-mirror pull

## Status

✅ Accepted

## TL;DR

Burrow bundles an **optional** in-cluster OCI registry (zot) so a solo developer can deploy
without first standing up an external registry account. The laptop pushes to the registry over
a **NodePort** (authentication and TLS required); the kubelet pulls **in-cluster** through a
containerd registry-mirror pointed at the registry Service, so burrowd never touches an image
byte and no billable LoadBalancer is minted. The bundled registry is a **quick-start
convenience, not the spine**: the external-registry path (GHCR/ECR/DO, [ADR-0008](0008-two-build-paths.md)
/ [ADR-0017](0017-private-registry-authentication.md)) stays first-class and is the recommended
choice for security-conscious teams. Realizes the "one box" promise of
[ADR-0044](0044-single-vps-k3s-cluster.md); keeps the registry as the conveyor belt
([ADR-0004](0004-code-never-over-mcp.md)); preserves burrowd's hands-off stance toward the
registry ([ADR-0040](0040-burrowd-never-contacts-the-registry.md)); stays cost-uniform across
VPS and cloud by refusing a per-registry LoadBalancer ([ADR-0043](0043-public-reachability-is-a-loadbalancer.md)).
Supersedes nothing.

## Context

Burrow's positioning is "one box, point your agent at it, deploy" ([ADR-0044](0044-single-vps-k3s-cluster.md)).
That promise quietly breaks the first time a developer deploys: the built image has to live in a
container registry ([ADR-0004](0004-code-never-over-mcp.md)), and today the only option is an
**external** one — create a Docker Hub / GHCR account, log in, provision a pull secret
([ADR-0017](0017-private-registry-authentication.md)). For a solo developer who just installed
Burrow into a single VPS, that external dependency is the sharpest onboarding wart in the whole
flow, and it appears at the worst moment: the very first deploy, before any app has been
published and before the ingress or cert-manager stack exists.

A registry that lives **in the cluster** closes the gap — but it has to close it *uniformly*.
Burrow already treats the public-reachability seam this way: LoadBalancer support is detected
"from whatever services LoadBalancers" (servicelb, MetalLB, or a cloud provider — one feature,
the seam varies underneath, [ADR-0043](0043-public-reachability-is-a-loadbalancer.md)). The
bundled registry must follow the same rule: one feature that behaves identically on a 2GB VPS
and a multi-node managed cluster, with only the external seam differing. Three forces shape the
design:

- **Reference symmetry.** An image reference is a string baked into the Deployment. The laptop
  pushes it; the kubelet pulls it. Both must resolve the same registry from opposite vantage
  points — and the laptop is *external to every cluster*, so it can never resolve an in-cluster
  Service name. Something has to bridge external → registry on the push side, while the pull side
  stays in-cluster.
- **Cost uniformity.** The cheap-tier thesis ([ADR-0043](0043-public-reachability-is-a-loadbalancer.md),
  [ADR-0044](0044-single-vps-k3s-cluster.md)) is that a public app costs nothing beyond the box.
  A registry that demanded a `type=LoadBalancer` Service would mint a *billable* cloud LB on a
  managed cluster — the exact cost the rest of the design avoids.
- **Footprint.** The honest memory floor for a public HTTPS site is already 2GB
  ([ADR-0044](0044-single-vps-k3s-cluster.md)). A bundled registry has to fit inside that floor,
  which rules out heavyweight registry products.

## Decision

### 1. Bundle zot as an optional in-cluster registry

Burrow ships **zot** (the CNCF OCI-native registry) as the bundled engine: a single static
binary (~50MB), native htpasswd/bearer authentication, scheduled garbage collection and
retention policies, and optional vulnerability scanning. It runs comfortably inside the 2GB
floor and scales up to a real cluster unchanged — the same feature everywhere. It is deployed in
`burrow-system` with a PVC for image storage, GC and a storage quota enabled by default, and
resource limits sized for the cheap tier.

The bundled registry is **optional**. It is enabled by default for `burrow cluster bootstrap`
(the single-VPS quick start, [ADR-0044](0044-single-vps-k3s-cluster.md)) so the first deploy
needs no external account; on any cluster it can be declined in favor of an external registry.

### 2. Push over a NodePort — not a LoadBalancer, and not the control plane

The laptop reaches the registry to push through a **NodePort** Service on a pinned port. This is
chosen over the two alternatives deliberately:

- **Not a `type=LoadBalancer` Service.** That would provision a billable cloud LoadBalancer on a
  managed cluster, contradicting the cheap-tier thesis. A NodePort is **free on every
  environment** — servicelb, MetalLB, or cloud — so the registry adds no infrastructure cost.
- **Not through the control plane.** Streaming image bytes through burrowd would make it a
  data-plane conduit, push hundreds of megabytes through the Kubernetes API-server proxy (which
  is not built for bulk transfer), and violate the spirit of
  [ADR-0040](0040-burrowd-never-contacts-the-registry.md). With a NodePort, the laptop pushes
  directly and the kubelet pulls directly; **burrowd never touches an image byte.**

NodePort was dropped as a *user-facing app front door* ([ADR-0043](0043-public-reachability-is-a-loadbalancer.md))
because an end-user URL needs a real public IP, DNS, and TLS, for which a high NodePort is a poor
door. That reasoning does not transfer here: this is a **developer push endpoint**, and
`host:port` is the native idiom for `docker push`. The two roles are distinct, so the NodePort
registry does not reopen the ADR-0043 decision.

### 3. Pull in-cluster via a containerd registry-mirror

The kubelet pulls using a **canonical registry reference** that a containerd registry-mirror
(k3s `registries.yaml`, or the equivalent containerd config elsewhere) maps to the in-cluster
registry Service. Pulls therefore stay inside the cluster and never hairpin out to the node's
public IP. The pull credentials travel in the per-app imagePullSecret Burrow already provisions
([ADR-0017](0017-private-registry-authentication.md)). The stored image reference is the one the
kubelet resolves; the NodePort is only the external door the laptop pushes through.

### 4. Authentication and TLS are mandatory

A NodePort binds on every node IP, and on a VPS the node IP *is* public, so the registry
endpoint is internet-reachable by default. Two protections are therefore non-negotiable:

- **Authentication.** An anonymous registry on a public port lets anyone pull images (the app's
  code and baked-in configuration) and — far worse — *push*, overwriting a tag with a malicious
  image the cluster then runs. zot runs with htpasswd/bearer auth; Burrow mints the credentials,
  places the pull side in the imagePullSecret, and the password never crosses MCP
  ([ADR-0029](0029-secrets-through-the-control-plane.md)).
- **TLS.** A raw NodePort is cleartext: basic-auth credentials are sniffable, layers are
  tamperable, and Docker refuses a non-localhost HTTP registry unless it is added to
  `insecure-registries` (which is both friction and a disabled safety check). zot terminates its
  own TLS. Where a DNS name is available the certificate is issued by cert-manager; where only a
  bare IP exists, a self-signed certificate is used with Burrow injecting CA trust on both ends
  (the laptop's client and the cluster's containerd). This trust-distribution step is the honest
  rough edge of the bundled path, and part of why it is positioned as a convenience rather than
  the hardened default.

### 5. A quick-start convenience, not the spine

The bundled registry exists to remove first-deploy friction for the solo developer. The
**external-registry path stays first-class** and is the recommended choice for anyone
security-conscious: pointing Burrow at GHCR, ECR, or a managed registry keeps the image store
off the cluster's public surface entirely. `docs/HARDENING.md` documents the tradeoff and how to
switch. Per honest status ([ADR-0009](0009-honest-status.md)), this ADR is a decision ahead of
the code; nothing here is advertised as shipped until it is.

## Consequences

- **The one-box promise holds.** A solo developer can bootstrap a VPS and deploy without ever
  creating an external registry account.
- **Cost stays uniform.** No billable LoadBalancer is minted for the registry on any
  environment; the NodePort is free on servicelb, MetalLB, and cloud alike.
- **The invariants hold.** The registry is still the conveyor belt ([ADR-0004](0004-code-never-over-mcp.md)),
  and burrowd still never contacts it ([ADR-0040](0040-burrowd-never-contacts-the-registry.md)) —
  the laptop pushes to the NodePort and the kubelet pulls; burrowd is not in the path.
- **A new public attack surface exists**, so auth and TLS are mandatory, not optional. The
  security posture is only as good as the minted credentials and the TLS trust; this is the cost
  of the convenience and the reason the external path remains the hardened recommendation.
- **NodePort exposure is not uniform across environments** — public-by-default on a VPS, possibly
  firewalled on a managed cluster. A host-firewall allowlist is recommended where the environment
  permits one; this caveat is documented rather than hidden.
- **Disk is a shared, writable resource.** A reachable registry sharpens the disk-exhaustion risk,
  so GC and a storage quota are on by default rather than left to the operator.
- **The registry is single-tenant and shared** ([ADR-0045](0045-oss-enterprise-boundary.md)): all
  apps share one registry namespace, so a push credential can overwrite any app's images.
  Acceptable for the solo-developer case; noted for anyone extending it.
- **A memory cost of roughly 50MB** is added inside the 2GB floor when the bundled registry is
  enabled — small, but not free, and disabled when an external registry is used.

## Rejected alternatives

- **Harbor as the bundled registry.** Harbor is the enterprise registry — a multi-component
  system with its *own* Postgres and Redis, a portal, jobservice, and Trivy, wanting several GB.
  It cannot run inside the 2GB floor, so bundling it would give parity only on large clusters —
  the opposite of the goal. Harbor remains available as a **connect / bring-your-own** option
  (mirroring the install-vs-connect split of [ADR-0026](0026-observability-query-adapters.md)) for
  teams that want scanning, RBAC, and a UI and have the headroom.
- **Push image bytes through the control plane.** Making burrowd stream the image to an in-cluster
  registry would turn it into a data-plane conduit, force hundreds of megabytes through the
  API-server proxy (not built for bulk transfer, with real timeout risk), and violate the spirit
  of [ADR-0040](0040-burrowd-never-contacts-the-registry.md). Rejected in favor of a direct
  laptop→NodePort push.
- **Expose the registry via `type=LoadBalancer` or the shared ingress.** A dedicated LoadBalancer
  mints a billable cloud LB on managed clusters (contradicting the cheap-tier thesis), and routing
  through the ingress requires the ingress-nginx and cert-manager stack to be installed — which
  raises the memory floor and is not present at first deploy, exactly when the registry is first
  needed. A NodePort is free and independent of that stack. Reusing an already-present ingress LB
  as an optional TLS front for the registry stays possible later, but is not the default.
- **Port-forward the registry for pushes.** A `kubectl port-forward` tunnel works on any cluster
  but is a single-use, manually re-established connection, clunky for repeated pushes, and still
  needs the reference-symmetry handling a NodePort plus containerd-mirror provides directly. A
  stable NodePort endpoint is the better door.
- **No bundled registry (status quo — external only).** Keeps the external-account friction that
  breaks the one-box promise at first deploy for the solo developer, which is the entire problem
  this ADR set out to remove.
