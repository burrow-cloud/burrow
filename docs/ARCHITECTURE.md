# Burrow Architecture

> **Status:** the core is built and runs on a real cluster — install, deploy, rollout and
> rollback, logs, status, scaling, and the guardrails — as the [validated
> quickstart](QUICKSTART.md) exercises end to end. This document describes the system's
> *shape*, and some of that shape is decided ahead of the code, so it is not a statement of
> what has shipped ([ADR-0009](adr/0009-honest-status.md)). The [README](../README.md) status
> table is the authoritative record of what is released and what is planned, and
> [CAPABILITIES.md](CAPABILITIES.md) is the reference for what is built today versus decided
> but not yet built.

Burrow is an agent-native cloud platform. It lets an AI coding agent deploy and operate
real applications on a Kubernetes cluster by driving Burrow through the `burrow-agent` CLI,
its scoped control channel ([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)).
The agent says "deploy this," "roll it back," "show me the logs," "scale it," and Burrow does
it safely on the user's own cluster.

This repository is **open source**: the single-tenant control plane, the `burrow-agent`
control channel, and the `burrow` CLI, packaged so a developer can self-host the whole thing. The multi-tenant managed
cloud (billing, teams, dashboard, SSO) is a separate product and does not live
here.

## The four layers

Burrow is four layers; the line between "ours" and "not ours" is sharp
([ADR-0002](adr/0002-four-layer-architecture.md)).

```
┌──────────────────────────────────────────────────────────────────┐
│ 1. The agent          Claude Code / Cursor / Codex / Cowork / …   │  not ours
│                        any agent that can run a command           │
└───────────────┬──────────────────────────────────────────────────┘
                │  burrow-agent  (verbs + small metadata only — no code)
┌───────────────▼──────────────────────────────────────────────────┐
│ 2. The agent CLI       burrow-agent · agent-neutral · NO creds    │  ours (thin)
│                        translates verbs → control-plane calls     │
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

   the registry — the conveyor belt — runs alongside the channel, not through it:
   builder ──push image──▶ container registry ──pull──▶ Kubernetes nodes
```

1. **The agent** — not ours. Any agent that can run a command. Burrow is agent-neutral and
   assumes nothing about which agent drives it
   ([ADR-0003](adr/0003-agent-neutral-mcp-control-surface.md)).
2. **The agent control channel** — `burrow-agent`, a thin, capability-reduced CLI the agent
   invokes directly ([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)). It
   carries the operate-verbs, outputs JSON first so the agent can compose the result, and
   translates each verb into a control-plane call. Holds **no cluster credentials**
   ([ADR-0005](adr/0005-mcp-server-holds-no-cluster-credentials.md)) and contains no
   orchestration logic. The remote control, not the engine.
3. **The control plane** — **the product.** Deploy orchestration, the build-to-image
   pipeline, rollout and rollback, logs and status, scaling, the guardrails
   ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)), and the record of who
   deployed what. Holds the cluster credentials; the only layer that talks to Kubernetes.
4. **Kubernetes** — not ours. The runtime Burrow targets.

The human `burrow` CLI is a fourth-wall client: it talks to the same control-plane API
`burrow-agent` does, for the human who wants to drive Burrow directly or build-and-push an
image.

## Load-bearing invariants

These are the decisions everything else rests on. Each has an ADR.

1. **Code never travels over the agent control channel**
   ([ADR-0004](adr/0004-code-never-over-mcp.md); its "over MCP" wording is generalized to any
   control channel by [ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)). The
   channel carries verbs and small metadata (an image reference, env vars, a command). The
   built image moves through a **container registry**, never the control channel. *The control
   channel is the remote control; the registry is the conveyor belt.*
2. **The agent control channel holds no cluster credentials; the control plane does**
   ([ADR-0005](adr/0005-mcp-server-holds-no-cluster-credentials.md), migrated to `burrow-agent`
   by [ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)). The security boundary is
   the control plane, not the thin `burrow-agent` client.
3. **Guardrails live in the control plane**
   ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)), between agent and cluster.
   Dangerous operations are gated or refused there, and every operation returns a
   structured result the agent can reason over.
4. **Deploy is an explicit call by image reference**
   ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)). Passive image-tag
   watching (GitOps auto-deploy) may exist later as an optional mode but is never the
   spine — the explicit call is where the guardrails, the structured feedback, and the
   rollback handle live.
5. **Two build paths for two users**
   ([ADR-0008](adr/0008-two-build-paths.md)). The agent or CLI builds the image and pushes
   it — the default; or the build runs from a git reference off the developer's machine.
   The second path is served today by the optional in-cluster build
   ([ADR-0053](adr/0053-in-cluster-build-from-source.md)), which builds inside the user's own
   cluster; a Burrow-*hosted* build service is deferred. Both converge on a reference in a
   registry.
6. **Honest status** ([ADR-0009](adr/0009-honest-status.md)). Everything in the docs is a
   goal until it ships. Never describe unbuilt behavior as done.

## Request paths

### Deploy

1. The image is already built and pushed to a registry the cluster can pull from — by the
   agent or CLI, or by the optional in-cluster build
   ([ADR-0008](adr/0008-two-build-paths.md), [ADR-0053](adr/0053-in-cluster-build-from-source.md)).
   The bytes rode the conveyor belt, not the control channel.
2. The agent runs `burrow-agent deploy` with an **image reference** plus small
   metadata (command, replica count) — no code
   ([ADR-0004](adr/0004-code-never-over-mcp.md)). Config and secrets are not passed here:
   they are a separate store, sourced at deploy time
   ([ADR-0028](adr/0028-app-config-and-secrets.md)).
3. `burrow-agent` forwards the call to the control plane over the authenticated
   control-plane API. It holds no cluster credentials and makes no cluster calls itself.
4. The control plane runs the guardrails
   ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)). If the app has a `pre-deploy`
   hook configured for that environment, it runs that command first, as a Job from the image
   **being deployed**, with the app's config and Secret — the supported place for a schema
   migration ([ADR-0072](adr/0072-deploy-and-run-lifecycle-hooks.md) §2). A failure aborts here:
   nothing reaches the cluster and the running version keeps serving (§3). Then — using the
   cluster credentials it alone holds — it instructs Kubernetes to roll out the referenced image.
5. The control plane records the deploy (image digest, when, by whom, what it replaced) —
   the rollback handle ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)) — and
   returns a structured result describing what changed and how to undo it.
6. Kubernetes pulls the image from the registry and runs it.

### Status, logs, scale

The agent runs the corresponding `burrow-agent` verb; it forwards the call; the control plane
queries or mutates Kubernetes through its credentials, applies guardrails to mutating
operations (e.g. scale), and returns a structured result.

### Rollback

The agent calls `rollback`; the control plane looks up the recorded prior deploy for the
target and redeploys that reference through the same guarded path — recovery is a
first-class, supported operation, not guesswork. A rollback fires the `pre-rollback` hook and
**never** `pre-deploy` ([ADR-0072](adr/0072-deploy-and-run-lifecycle-hooks.md) §8), and it runs
from the image being rolled back *away from*: that is where the code that knows how to undo its
own migration lives. `pre-rollback` is unset by default, so a rollback runs nothing unless
someone deliberately configured it.

A failed `pre-rollback` hook aborts the rollback, because letting the older code serve against a
half-reverted schema is what the ordering exists to prevent. When the hook failed for a reason
that has nothing to do with the schema — it will not pull, it will not schedule, the command is
wrong — `burrow app rollback <app> --skip-hooks` rolls back without running it
([ADR-0080](adr/0080-a-rollback-is-not-blocked-by-its-own-hook.md)). The hook stays configured, the
skip is stated on the result and recorded in the audit log, and the flag is on the operator CLI
only: the abort an agent meets names the command a human runs, which the agent relays.

## Components and code layout

All code is licensed Apache-2.0 ([ADR-0033](adr/0033-relicense-to-apache.md),
[LICENSING.md](../LICENSING.md)). The control plane and operator sit outside the top-level
`internal/` so the separate private managed module can import their public API — a module
boundary, not a license boundary.

- **Client surface:** `cmd/burrow` — the human **admin CLI**; `cmd/burrow-agent` — the thin,
  agent-neutral, credential-free **agent control channel**
  ([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)), a capability-reduced,
  JSON-first CLI the agent invokes directly and the **only** agent-facing surface
  ([ADR-0062](adr/0062-remove-the-mcp-server.md) removed the MCP server that preceded it);
  `internal` — module-private shared helpers only.
- **The product:** `controlplane` (public API) with `controlplane/internal`
  (guts) and binary `cmd/burrowd` — the **control plane** that holds cluster credentials, runs
  orchestration and guardrails, and owns the deploy record and its database state; `operator`
  with `operator/internal` — the Kubernetes **operator** (CRD types and reconcilers).

Control-plane logic is kept **pure and seam-isolated**: anything that touches Kubernetes, the
container registry, the clock, or the database lives behind an interface so it can be
faked in tests. See [CLAUDE.md](../CLAUDE.md) for the package conventions and
[ADR-0010](adr/0010-testing-strategy.md) for the testing posture.

## State

The control plane keeps its own state — deploy records, rollout history, and operational
metadata — in **Postgres running in the cluster** (ADR-0012), behind a database interface
so it can be faked in unit tests. Burrow's own state lives in the user's cluster, not an
external managed service. That Postgres runs on **CloudNativePG**, the same operator every
Postgres add-on runs on, so there is one stack to operate, one repair path, and one way to
back a database up ([ADR-0086](adr/0086-burrow-installs-one-kind-of-postgres.md));
`burrow cluster install --database plain` runs it as a single Deployment instead, for a
cluster that will not accept the operator's cluster-scoped CustomResourceDefinitions. This state is independent of Kubernetes cluster state; the
cluster is the source of truth for what is running, and the control plane's database is
the source of truth for the deploy history and the rollback handles.

The same database holds an **append-only audit log** (ADR-0027): one record per guarded,
mutating operation and the guardrail decision that applied — allowed, held for confirmation,
denied, executed, or failed — written at the control-plane boundary, the single choke point
that holds both the credentials and the decision. Its arguments are redacted to safe metadata
(names, image reference, replica count, env/secret key names — never a value), and it is
read-only through the API: the operator and the agent can review it with `burrow audit`, but
nothing can write to or alter it.

Beside it, in separate tables, sits the **failure ledger** (ADR-0074): what the cluster *did*
afterwards. burrowd runs a background observer over the objects the registry says it owns and
records one row per (object, reason) with a first-seen, a last-seen, a resolution and a count —
so "when did this start" and "has this happened before" are answerable, which they are not from
the cluster, whose Events expire in an hour. The observer **watches** those objects rather than
sampling them, and **latches** each transition on both edges before recording it (ADR-0079): a
condition must persist for a per-reason dwell to open a row and be clear for one to close it, so
the ledger holds failures rather than the flapping Kubernetes status is full of. It is deliberately
not merged with the audit log: that record is append-only and complete, and this one is pruned.
Alongside the rows the ledger records **its own observation coverage**, so a stretch in which
nothing was watching — a restart, or a watch that lost its place — reads as a gap rather than as an
hour in which nothing broke. Both are read with `burrow failures` (and
`burrow-agent failures`), which groups a cascade by shared reason for a human reader while the
API returns the rows ungrouped for an agent. Current state is never served from here: `burrow app
status` stays a live read, because a cache is most stale during the incident it exists to help.

## Where to look for what is built

This document describes the shape. What is actually built — every capability, the command that
reaches it, and its limits — is [CAPABILITIES.md](CAPABILITIES.md), which also lists the
decisions that are recorded but not yet implemented. The version milestones toward v1.0 are in
[ROADMAP.md](ROADMAP.md) and the current front line is in [PLAN.md](PLAN.md); those describe
the sequencing.
