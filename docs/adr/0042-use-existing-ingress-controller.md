# ADR-0042: Use the cluster's existing ingress controller

## Status

✅ Accepted

## TL;DR

Burrow uses whatever ingress controller is already running on the cluster instead of requiring
its own ingress-nginx. It detects the running controller and its IngressClass, binds each
exposed app's Ingress (and the cert-manager HTTP-01 solver) to that class, and installs
ingress-nginx only when no controller exists at all. This unblocks the large population of k3s
and k3d clusters, which ship traefik by default, and any cluster that already runs a controller,
without forcing a second, redundant one. It refines [ADR-0022](0022-routing-backend-and-supported-kubernetes.md)'s
"detect what the cluster supports and use it" from the Ingress-versus-Gateway axis to the
which-controller axis, and keeps to portable standard Ingress so one code path works across
controllers. Refines [ADR-0022](0022-routing-backend-and-supported-kubernetes.md); relates to
[ADR-0018](0018-reaching-an-app-at-a-url.md), [ADR-0034](0034-agent-native-onboarding.md), and
[ADR-0041](0041-flatten-path-to-a-reachable-app.md). Supersedes nothing.

## Context

ADR-0022 chose a single shared ingress controller and said Burrow "detects what the cluster
supports and uses it." The implementation went further toward ingress-nginx specifically:
`burrow cluster ingress install` installs ingress-nginx, the exposed app's Ingress and the
cert-manager HTTP-01 solver hardcode the `nginx` class, and the capability survey reports the
readiness of an ingress-nginx controller. That is fine on a bare DigitalOcean cluster where
Burrow installs the controller.

But k3s and k3d, which a large share of self-hosters and homelab and edge users run, ship
**traefik** by default and register a `traefik` IngressClass. On those clusters Burrow today
reports "no ingress controller" and pushes the user to install ingress-nginx alongside their
working traefik, which is redundant, confusing, and a real adoption barrier for exactly Burrow's
self-hoster audience.

Standard `networking.k8s.io/v1` Ingress is served by every major controller (nginx, traefik, and
others), and cert-manager's HTTP-01 challenge is controller-agnostic as long as the temporary
solver Ingress names the right class. So the barrier is not technical necessity; it is that the
class is hardcoded to `nginx` in three places: the app's Ingress, the issuer's HTTP-01 solver,
and the capability check.

## Decision

**Burrow uses the ingress controller the cluster already has.**

1. **Detect the controller and its class.** The capability survey recognizes a running ingress
   controller of any supported implementation (ingress-nginx and traefik first, others
   best-effort) and reports the IngressClass to bind to. A leftover IngressClass with no running
   controller is still reported as not usable (the orphan-class fix stands: a class alone is not
   a controller).
2. **Bind expose to the detected class.** The exposed app's Ingress sets `ingressClassName` to
   the detected class rather than a hardcoded `nginx`, and the cert-manager HTTP-01 solver uses
   that same class so the ACME challenge is served by whatever controller is present.
3. **Install only when nothing is there.** `burrow cluster ingress install` installs
   ingress-nginx only when the cluster has no running controller; when one already exists it
   adopts it (ensuring cert-manager and the ClusterIssuer, wired to the existing class) rather
   than adding a second controller.
4. **Stay portable.** Expose keeps to standard Ingress fields that every controller honors.
   Controller-specific annotations (for example nginx's `nginx.ingress.kubernetes.io/*`) are
   applied only when that controller is the one in use, never assumed.

This fulfills ADR-0022's "detect what the cluster supports and use it," extending it from the
Ingress-versus-Gateway backend axis to the which-ingress-controller axis. The `expose` surface
the user and agent see does not change; the user never types "nginx" or "traefik."

## Consequences

- k3s and k3d users (traefik), and anyone with an existing controller, can expose apps without
  installing a second controller; the "no ingress controller" wall disappears for them.
- `cluster ingress install` shifts from "install ingress-nginx" to "ensure a controller exists":
  it installs nginx only as the fallback when the cluster has none, and otherwise adopts what is
  there.
- The class is threaded from detection through expose and the issuer solver, replacing the three
  hardcoded `nginx` references.
- cert-manager HTTP-01 must be verified against each supported controller in the
  ephemeral-cluster tests; k3d ships traefik, which makes the traefik path cheap to test, and a
  nginx install covers the other.
- Officially supported controllers are a small set (ingress-nginx, traefik) with others served
  best-effort through standard Ingress. Handling controller-specific behavior (annotations,
  redirects) per detected controller is the main implementation cost, and the reason to keep the
  supported set small and the default path annotation-light.
- The capability readiness check generalizes from ingress-nginx-only to the detected controller;
  capability and expose stay consistent because both key off the same detection.

## Rejected alternatives

- **Require ingress-nginx (the status quo).** Forces k3s, k3d, and traefik users to run a
  redundant second controller, an adoption barrier for a core slice of the audience, for no
  technical reason given standard Ingress is portable.
- **Support traefik through its `IngressRoute` CRD instead of standard Ingress.** More native to
  traefik, but controller-specific and not portable, and it needs traefik's CRDs. Standard
  Ingress works across controllers with one code path and no extra CRDs.
- **Auto-install a controller the user did not ask for when one is missing.** Kept as an
  explicit, guarded step ([ADR-0034](0034-agent-native-onboarding.md),
  [ADR-0041](0041-flatten-path-to-a-reachable-app.md)): installing a controller can provision a
  billable load balancer, so it stays a deliberate `cluster ingress install`, not an implicit
  side effect.
