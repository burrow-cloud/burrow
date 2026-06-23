# Burrow Roadmap

> **Status: pre-implementation.** These are version milestones, not shipped features.
> Each milestone is a goal until it ships ([ADR-0009](adr/0009-honest-status.md)); the
> [README](../README.md) status table is the authoritative shipped/in-progress/planned
> surface. This file holds the coarse milestones; [PLAN.md](PLAN.md) holds the current
> execution detail.

Burrow follows semver from v0.1 toward v1.0. The theme of the 0.x series is **compute
first**: deploy someone's code and run it well, safely, agent-driven. Databases, domains,
autoscaling, and cost controls come after the deploy-and-operate core is solid.

## v0.1 — Deploy and operate (the vertical slice)

The thin end-to-end slice that proves the architecture. Install Burrow into an existing
cluster, point an agent at it over MCP, and deploy and operate a real application by image
reference. Detailed scope and the explicit out-of-scope list are in [PLAN.md](PLAN.md).

- Install the control plane and MCP server into an existing Kubernetes cluster.
- Connect any MCP agent to the MCP server.
- `deploy` an image by reference ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)),
  with client-side build-and-push ([ADR-0008](adr/0008-two-build-paths.md)).
- `status`, `logs`, `rollback`, `scale` — each guarded and returning structured results
  ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)).
- Integration tests running the real deploy/rollback/logs/scale paths against an ephemeral
  local cluster (kind or k3d).

## v0.2 and beyond — candidate themes (unsequenced)

The order and contents below are provisional and will be refined as v0.1 lands. They are
listed to show direction, not commitment.

- **Server-side build from a git reference** ([ADR-0008](adr/0008-two-build-paths.md)) —
  the second build path, toward the managed experience.
- **Richer guardrails and policy** — configurable gates, confirmation flows, blast-radius
  limits for destructive operations.
- **Database provisioning** — managed Postgres (and friends) as a first-class deploy
  dependency.
- **Domains and TLS** — public ingress, certificates, routing.
- **Autoscaling** — horizontal/vertical scaling driven by load.
- **Cost controls and caps** — visibility and limits on cluster spend.
- **Optional passive deploy mode** — GitOps-style tag-watching as an *option* layered on
  the explicit path, never replacing it ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)).
- **Self-host dashboard** — an HTMX dashboard over the control-plane API, if and when a
  self-host UI is warranted.

## v1.0 — Production self-host

A self-host Burrow a solo developer or small agency can run their real infrastructure on:
the deploy-and-operate core hardened, the guardrails mature, the common day-two operations
(databases, domains, scaling) covered, and the upgrade and operational story documented and
tested. The multi-tenant managed cloud built on top of this core remains a separate,
private product.
