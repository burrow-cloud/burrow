# ADR-0041: A flatter path to a reachable app

## Status

✅ Accepted

## TL;DR

Deploying an app gives it a stable in-cluster Service by default (a ClusterIP on the app's
port), so every app is addressable internally whether or not it is ever exposed, and exposing it
later only adds the Ingress and DNS. Making an app reachable becomes a single operation ("make
`<app>` reachable at `<host>`") that ensures the Service, creates the Ingress with TLS, writes the
DNS record when a provider is configured, waits for the certificate, and confirms reachability,
with structured "run this" feedback for the one-time cluster prerequisites (a running ingress
controller and a DNS provider). The app declares its port at deploy (an optional port on the
deploy call); no port, no Service. Builds on [ADR-0007](0007-explicit-deploy-by-image-reference.md)
(explicit deploy), [ADR-0022](0022-routing-backend-and-supported-kubernetes.md) (shared ingress),
and [ADR-0006](0006-guardrails-in-the-control-plane.md) (structured feedback); relates to
[ADR-0034](0034-agent-native-onboarding.md) (onboarding). Supersedes nothing.

## Context

Getting an app reachable from the internet today is several distinct steps a non-Kubernetes user
has to hold in their head at once: deploy the workload; expose it (which creates a Service and an
Ingress); add a DNS record (a separate operation); and, one time per cluster, install an ingress
controller (billable) and configure a DNS provider (a credential). The app's serving port is only
supplied at expose time, so the Service does not exist until then, and there is no in-cluster
address for the app before it is exposed. The result is a lot of moving parts (exposure, DNS,
ingress) surfacing at once, which overwhelms the target user and gives the agent a wide,
order-dependent surface to get wrong.

Two problems compound it. An app has no stable internal Service until it is publicly exposed, so
apps cannot address each other and metrics have nothing stable to scrape. And the reachability
prerequisites (a working ingress controller, a DNS provider) are one-time cluster setup that must
be surfaced clearly rather than discovered by a failed exposure. (The capability survey has also
reported ingress as available from an IngressClass alone, which a leftover class defeats; that
accuracy fix is being made separately, and this ADR assumes the survey tells the truth.)

## Decision

### 1. A Service at deploy, by default

Deploying an app creates a **ClusterIP Service** for it (a stable in-cluster name,
`<app>.<namespace>.svc`), not just the Deployment. It is internal only: a ClusterIP exposes
nothing to the internet and costs nothing. Every app is then addressable in-cluster the moment it
is deployed, which enables service-to-service traffic and gives metrics a stable target, and it
means exposing the app later only adds the Ingress. This mirrors how a PaaS treats a deployed
service.

### 2. The app declares its port at deploy

A Service needs the app's container port, so the app declares it at deploy time (an optional
`port` on `burrow app deploy` / `burrow_deploy`). The agent already discovers it (it reads the
app's serving port from the code), so it simply supplies it earlier. If no port is given, no
Service is created (the app runs but is not yet addressable) and it can be added on a later
deploy. This is the recommended approach over the rejected alternatives (inferring the port from
the image, or defaulting it) below. *(This is the one open decision for the maintainer to confirm:
explicit optional port is the recommendation.)*

### 3. One "make it reachable" operation

Reachability collapses into a single guardrailed publish ("make `<app>` reachable at `<host>`"):
ensure the Service (from the deploy port, or create it now), create the Ingress with TLS, write
the DNS record when a DNS provider is configured (otherwise return the external address for the
user to point DNS at manually), wait for the certificate to issue, and confirm the app answers.
The existing guardrails still apply (public exposure and DNS writes are held for confirmation by
default), and it is one intent for the agent to carry out instead of an expose-then-add-DNS
sequence. The lower-level operations remain available as primitives beneath it.

### 4. One-time prerequisites are surfaced, not stumbled into

Installing the ingress controller (`burrow cluster ingress install`, billable) and configuring a
DNS provider (`burrow config provider add`, a credential) remain deliberate, human, one-time
steps, because they cost money or hold a secret. But the publish operation reports them as
structured, actionable prerequisites ([ADR-0006](0006-guardrails-in-the-control-plane.md)) when
they are missing, and the cluster-capability survey reports them accurately (a controller must
actually be running, not merely have left an IngressClass behind), so the agent guides the user to
the exact command instead of discovering the gap by a failed exposure.

## Consequences

- Far fewer moving parts for the user: deploy makes the app addressable, one publish makes it
  reachable, and the two one-time setup steps are surfaced clearly instead of stumbled into.
- Apps are addressable in-cluster by default, unlocking service-to-service traffic and a stable
  metrics target without a separate step.
- Deploy grows an optional port; the agent supplies it (it already finds it). An app deployed
  without a port simply has no Service yet and gains one when a port is next supplied.
- A ClusterIP per app is created on deploy: negligible cost, no external surface.
- The publish operation orchestrates the Service, Ingress, DNS, and certificate chain; each link
  still enforces its own guardrail.
- Backward compatible: apps deployed before this keep working and gain a Service when a port is
  next supplied; the separate expose and DNS operations stay as primitives beneath the unified
  publish.

## Rejected alternatives

- **Keep the separate deploy / expose / add-DNS steps.** The status quo that overwhelms the user
  and gives the agent an order-dependent, easy-to-fumble surface. Removing that surface is the
  point of this ADR.
- **Infer the app's port from the image (the `EXPOSE` directive).** Unreliable: `EXPOSE` is
  frequently unset or wrong, and it is metadata, not a guarantee. Declaring the port is explicit,
  and the agent already knows it.
- **Default the port to a fixed value.** Wrong as often as right, and a silent wrong default is
  worse than asking for it.
- **Auto-expose every app publicly on deploy.** No. Public exposure is a deliberate cost and
  security decision, guardrailed on purpose ([ADR-0007](0007-explicit-deploy-by-image-reference.md),
  [ADR-0020](0020-guardrails-as-configurable-policy.md)). The Service-by-default is internal only;
  going public stays an explicit, confirmed act.
