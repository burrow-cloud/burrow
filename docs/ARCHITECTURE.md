# Burrow Architecture

> **Status: pre-implementation.** This document describes the design being built, not
> shipped behavior. Everything here is a goal until it ships and is marked accordingly in
> the [README](../README.md) status table ([ADR-0009](adr/0009-honest-status.md)).

Burrow is an agent-native cloud platform. It lets an AI coding agent deploy and operate
real applications on a Kubernetes cluster by driving Burrow through an MCP server. The
agent says "deploy this," "roll it back," "show me the logs," "scale it," and Burrow does
it safely on the user's own cluster.

This repository is the **open core**: the single-tenant control plane, the MCP server, and
the CLI, packaged so a developer can self-host the whole thing. The multi-tenant managed
cloud (billing, teams, dashboard, SSO) is a separate, private product and does not live
here.

## The four layers

Burrow is four layers; the line between "ours" and "not ours" is sharp
([ADR-0002](adr/0002-four-layer-architecture.md)).

```
┌──────────────────────────────────────────────────────────────────┐
│ 1. The agent          Claude Code / Cursor / Codex / Cowork / …   │  not ours
│                       any MCP client                              │
└───────────────┬──────────────────────────────────────────────────┘
                │  MCP  (tool calls + small metadata only — no code)
┌───────────────▼──────────────────────────────────────────────────┐
│ 2. The MCP server     thin · agent-neutral · NO cluster creds     │  ours (thin)
│                       translates tool calls → control-plane calls │
└───────────────┬──────────────────────────────────────────────────┘
                │  control-plane API  (authenticated)
┌───────────────▼──────────────────────────────────────────────────┐
│ 3. The control plane  THE PRODUCT                                 │  ours
│                       deploy orchestration · build-to-image       │
│                       pipeline · rollout/rollback · logs/status   │
│                       scaling · GUARDRAILS · deploy record        │
│                       holds the cluster credentials               │
└───────────────┬──────────────────────────────────────────────────┘
                │  Kubernetes API  (cluster credentials)
┌───────────────▼──────────────────────────────────────────────────┐
│ 4. Kubernetes         the runtime Burrow operates on top of       │  not ours
└──────────────────────────────────────────────────────────────────┘

   the registry — the conveyor belt — runs alongside, not through MCP:
   builder ──push image──▶ container registry ──pull──▶ Kubernetes nodes
```

1. **The agent** — not ours. Any MCP client. Burrow is agent-neutral and assumes nothing
   about which agent drives it ([ADR-0003](adr/0003-agent-neutral-mcp-control-surface.md)).
2. **The MCP server** — thin. Exposes Burrow's tools and translates agent calls into
   control-plane calls. Holds **no cluster credentials**
   ([ADR-0005](adr/0005-mcp-server-holds-no-cluster-credentials.md)) and contains no
   orchestration logic. The remote control, not the engine.
3. **The control plane** — **the product.** Deploy orchestration, the build-to-image
   pipeline, rollout and rollback, logs and status, scaling, the guardrails
   ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)), and the record of who
   deployed what. Holds the cluster credentials; the only layer that talks to Kubernetes.
4. **Kubernetes** — not ours. The runtime Burrow targets.

The CLI is a fourth-wall client: it talks to the same control-plane API the MCP server
does, for the human who wants to drive Burrow directly or build-and-push an image.

## Load-bearing invariants

These are the decisions everything else rests on. Each has an ADR.

1. **Code never travels over MCP** ([ADR-0004](adr/0004-code-never-over-mcp.md)). MCP
   carries tool calls and small metadata (an image reference, env vars, a command). The
   built image moves through a **container registry**, never the MCP connection. *MCP is
   the remote control; the registry is the conveyor belt.*
2. **The MCP server holds no cluster credentials; the control plane does**
   ([ADR-0005](adr/0005-mcp-server-holds-no-cluster-credentials.md)). The security
   boundary is the control plane, not the thin MCP layer.
3. **Guardrails live in the control plane**
   ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)), between agent and cluster.
   Dangerous operations are gated or refused there, and every operation returns a
   structured result the agent can reason over.
4. **Deploy is an explicit MCP call by image reference**
   ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)). Passive image-tag
   watching (GitOps auto-deploy) may exist later as an optional mode but is never the
   spine — the explicit call is where the guardrails, the structured feedback, and the
   rollback handle live.
5. **Two build paths for two users**
   ([ADR-0008](adr/0008-two-build-paths.md)). The agent or CLI builds the image and pushes
   it (self-host developer, v0.1); or the platform builds from a git reference server-side
   (managed user, later). Both converge on a reference in a registry.
6. **Honest status** ([ADR-0009](adr/0009-honest-status.md)). Everything in the docs is a
   goal until it ships. Never describe unbuilt behavior as done.

## Request paths

### Deploy

1. The image is already built and pushed to a registry the cluster can pull from — by the
   agent or CLI in v0.1 ([ADR-0008](adr/0008-two-build-paths.md)). The bytes rode the
   conveyor belt, not MCP.
2. The agent calls the `deploy` tool over MCP with an **image reference** plus small
   metadata (env vars, command, replica count) — no code
   ([ADR-0004](adr/0004-code-never-over-mcp.md)).
3. The MCP server forwards the call to the control plane over the authenticated
   control-plane API. It holds no cluster credentials and makes no cluster calls itself.
4. The control plane runs the guardrails
   ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)), then — using the cluster
   credentials it alone holds — instructs Kubernetes to roll out the referenced image.
5. The control plane records the deploy (image digest, when, by whom, what it replaced) —
   the rollback handle ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)) — and
   returns a structured result describing what changed and how to undo it.
6. Kubernetes pulls the image from the registry and runs it.

### Status, logs, scale

The agent calls the corresponding tool; the MCP server forwards it; the control plane
queries or mutates Kubernetes through its credentials, applies guardrails to mutating
operations (e.g. scale), and returns a structured result.

### Rollback

The agent calls `rollback`; the control plane looks up the recorded prior deploy for the
target and redeploys that reference through the same guarded path — recovery is a
first-class, supported operation, not guesswork.

## Components and code layout

The package layout is shaped by the license boundary — the license follows the package
boundary, and the FSL-licensed product packages sit outside the top-level `internal/` so the
separate private managed module can import their public API (see
[ADR-0001](adr/0001-license-and-dco.md) and [LICENSING.md](../LICENSING.md)).

- **Apache-2.0 (client surface):** `cmd/burrow` — the **CLI**; `mcp` (with binary
  `cmd/burrow-mcp`) — the thin, agent-neutral, credential-free **MCP server** that translates
  tool calls into control-plane API calls; `internal` — module-private shared helpers only.
- **FSL-1.1-ALv2 (the product):** `controlplane` (public API) with `controlplane/internal`
  (guts) and binary `cmd/burrowd` — the **control plane** that holds cluster credentials, runs
  orchestration and guardrails, and owns the deploy record and its database state; `operator`
  with `operator/internal` — the Kubernetes **operator** (CRD types and reconcilers).

The intended shape (filled in with the v0.1 slice — see [PLAN.md](PLAN.md)) keeps
control-plane logic **pure and seam-isolated**: anything that touches Kubernetes, the
container registry, the clock, or the database lives behind an interface so it can be
faked in tests. See [CLAUDE.md](../CLAUDE.md) for the package conventions and
[ADR-0010](adr/0010-testing-strategy.md) for the testing posture.

## State

The control plane keeps its own state — deploy records, rollout history, and operational
metadata — in **Postgres running in the cluster** (ADR-0012), behind a database interface
so it can be faked in unit tests. Burrow's own state lives in the user's cluster, not an
external managed service. This state is independent of Kubernetes cluster state; the
cluster is the source of truth for what is running, and the control plane's database is
the source of truth for the deploy history and the rollback handles.

## What is in scope, and when

The v0.1 vertical slice — install into an existing cluster, connect an agent over MCP,
deploy an image by reference, then status, logs, rollback, and scale — and everything
explicitly out of scope for it, are defined in [PLAN.md](PLAN.md). The version milestones
toward v1.0 are in [ROADMAP.md](ROADMAP.md). This document describes the shape; those two
describe the sequencing.
