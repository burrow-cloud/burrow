# Burrow Plan — current execution plan

> **This file is the front line only.** It holds what is being worked now and next, in
> priority order, and is pruned as work lands — no growing TODO graveyard. Coarse
> milestones live in [ROADMAP.md](ROADMAP.md); a completed item's record survives in git
> history, its now-green test, and the shipped ADR/doc.

> **Status: awaiting plan approval.** No feature code has been written. The foundations
> (module, layout, docs, ADRs) are in place; the v0.1 scope below is **proposed for
> maintainer review** and is not yet being built.

## v0.1 — the thin vertical slice

**Goal:** an agent operates a real application on the user's own Kubernetes cluster,
end to end, safely. One slice that exercises every layer of the architecture
([ARCHITECTURE.md](ARCHITECTURE.md)) rather than a broad-but-shallow feature set.

The slice, in order:

1. **Install into an existing cluster.** A developer who already has a Kubernetes cluster
   installs the Burrow control plane and MCP server into it. The control plane gets the
   cluster credentials ([ADR-0005](adr/0005-mcp-server-holds-no-cluster-credentials.md));
   the MCP server gets none. Reference target: a DigitalOcean cluster.
2. **Connect an agent over MCP.** Point any MCP client at the MCP server
   ([ADR-0003](adr/0003-agent-neutral-mcp-control-surface.md)); it discovers Burrow's
   tools.
3. **Deploy an image by reference.** The agent (or CLI) has already built and pushed an
   image to a registry the cluster can pull from
   ([ADR-0008](adr/0008-two-build-paths.md), client-side path). The agent calls `deploy`
   with the **image reference** plus small metadata — env vars, command, replica count —
   no code over MCP ([ADR-0004](adr/0004-code-never-over-mcp.md)). The control plane runs
   the guardrails, rolls it out via the cluster credentials, records the deploy, and
   returns a structured result ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)).
4. **Status.** `status` reports what is running for a deployment — replicas, rollout
   state, health — as a structured result.
5. **Logs.** `logs` returns recent logs for a deployment.
6. **Rollback.** `rollback` redeploys the previously-recorded reference for a deployment
   through the same guarded path — recovery from a bad deploy is first-class.
7. **Scale.** `scale` changes the replica count, guarded (e.g. scale-to-zero and large
   scale-ups are gated, not silent).

Every mutating tool (`deploy`, `rollback`, `scale`) passes through the control-plane
guardrails and returns a structured result the agent can reason over
([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)).

### What the slice forces us to build (the real v0.1 work)

- **The three binaries' skeletons wired together:** `burrowd` (control plane),
  `burrow-mcp` (MCP server), `burrow` (CLI) — see [ARCHITECTURE.md](ARCHITECTURE.md).
- **The control-plane API** between the MCP server / CLI and the control plane, with
  control-plane-side **authentication of its clients**
  ([ADR-0005](adr/0005-mcp-server-holds-no-cluster-credentials.md)).
- **A Kubernetes seam** — the interface the control plane uses to deploy, query, stream
  logs, and scale — with a real client adapter and a fake for unit tests.
- **A registry/pull assumption** — the cluster can pull the referenced image (image-pull
  secrets); the control plane handles bytes never ([ADR-0004](adr/0004-code-never-over-mcp.md)).
- **The deploy record** in the control plane's own database (Neon/Postgres) behind a
  database interface, holding the rollback handles
  ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)).
- **The MCP tool surface** (`deploy`, `status`, `logs`, `rollback`, `scale`) with schemas
  and structured result types, agent-neutral
  ([ADR-0003](adr/0003-agent-neutral-mcp-control-surface.md)).
- **The v0.1 guardrails** — the first concrete gates: confirm/refuse on destructive or
  high-blast-radius operations (scale-to-zero, large scale-ups, redeploy over a healthy
  newer deploy). The exact policy is a design task within v0.1.
- **The install path** — how the control plane and MCP server land in an existing cluster
  and how the control plane receives cluster credentials.

### Testing posture for the slice

Burrow **differs from Hamster here** — there is no global simulation harness. The decision
and its rationale are [ADR-0010](adr/0010-testing-strategy.md).

- **Seam-isolated unit tests:** control-plane logic is kept pure and seam-isolated —
  Kubernetes, the registry, the **clock**, and the database behind injected interfaces, no
  ambient time or I/O in core logic — and unit-tested against **fakes**. Determinism comes
  from injected dependencies, not a global event simulator.
- **Targeted fault injection:** the deploy state machine and the operator reconcile loops get
  deterministic, seeded adversarial event orderings and injected API errors (fake k8s client
  + controller-runtime envtest) — the only parts with distributed-systems-shaped bugs.
- **Integration tests:** the real `deploy`, `rollback`, `logs`, and `scale` paths run against
  an **ephemeral local cluster (kind or k3d)** in CI.

## Out of scope for v0.1 (explicit)

These are deliberately excluded from the first slice. Naming them keeps the slice thin and
keeps the docs honest ([ADR-0009](adr/0009-honest-status.md)).

- **Build from source / server-side build** — v0.1 is client-side build-and-push only
  ([ADR-0008](adr/0008-two-build-paths.md)). The platform building from a git reference
  comes later.
- **Database provisioning** — no managed Postgres/other datastores for deployed apps.
- **Domains and TLS** — no public ingress, certificate, or routing management.
- **Autoscaling** — `scale` is explicit replica-count only; no load-driven scaling.
- **Cost caps and cost controls** — no spend visibility or limits.
- **Multi-tenancy** — single-tenant self-host only; teams, billing, SSO, and the dashboard
  live in the separate private managed product.
- **Passive/GitOps auto-deploy** — deploy is the explicit by-reference call only
  ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)); tag-watching is a later
  optional mode.
- **A self-host web dashboard** — the v0.1 surfaces are MCP and CLI; HTMX dashboard comes
  if/when warranted.

## Next (after the slice lands)

Refined from [ROADMAP.md](ROADMAP.md) once v0.1 is real. Not started; listed only so the
immediate horizon is visible. Likely first: hardening the guardrails and the server-side
build path.

---

## Status of the blocking decisions

- **License: settled.** [ADR-0001](adr/0001-license-and-dco.md) is **Accepted** — Apache-2.0
  client surface, FSL-1.1-ALv2 control plane and operator, sole ownership with CLA-gated
  outside code. The public-repo gate is cleared on the licensing front.
- **v0.1 scope: still awaiting approval.** Feature work on the slice above should not begin
  until the v0.1 scope here is approved by the maintainer.
