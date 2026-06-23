# ADR-0007: Deploy is an explicit MCP call by image reference

## Status

Accepted.

## Context

There are two broad ways to trigger a deployment. The **explicit** way: the agent makes a
`deploy` call naming an image reference, and the control plane acts on it. The **passive**
way: a watcher notices a new image tag in a registry and rolls it out automatically — the
GitOps / auto-deploy model. Passive watching is convenient and popular, and Burrow may
offer it as an option. But it cannot be the spine of the system, because the explicit call
is where Burrow's value concentrates: it is where the guardrails run, where structured
feedback is produced, and where the rollback handle is created.

## Decision

**Deploy is an explicit MCP (or CLI) call, by image reference.** The agent names an image
that is already in a registry the cluster can pull from
([ADR-0004](0004-code-never-over-mcp.md)), plus small metadata (env vars, command, replica
count), and the control plane performs a guarded rollout.

Passive image-tag watching (GitOps-style auto-deploy) **may exist as an optional mode, but
is never the spine.** It is not in scope for v0.1 ([docs/PLAN.md](../PLAN.md)). The
explicit, by-reference call is the canonical deploy path; any passive mode is a
convenience layered beside it, not the foundation.

## Consequences

- **Every deploy passes through the guardrails and yields a structured result.** Because
  the deploy is an explicit call into the control plane, it is gated
  ([ADR-0006](0006-guardrails-in-the-control-plane.md)) and returns what changed and how to
  undo it — the agent always knows the outcome.
- **Every deploy creates a rollback handle.** The control plane records the deploy (which
  image digest, when, by whom, replacing what), so `rollback` is a well-defined operation:
  redeploy the previously-recorded reference. This record is part of the product
  ([ADR-0002](0002-four-layer-architecture.md)).
- **Deploys are deterministic and auditable.** A deploy names a specific reference (ideally
  a digest), so what runs is exactly what was asked for — no ambiguity about which build a
  floating tag resolved to at some unobserved moment.
- **A passive mode, if added, must route through the same guarded path** to produce the
  same record and feedback — it cannot become a second, unguarded way into the cluster.
- The deploy contract is "reference in → guarded rollout → structured result out," which
  pairs directly with the registry-as-conveyor-belt model
  ([ADR-0004](0004-code-never-over-mcp.md)).

## Rejected alternatives

- **Make passive tag-watching the primary deploy mechanism.** Rejected: auto-rollout on a
  tag push happens with no explicit agent call, so it has no natural place to run the
  guardrails, no structured result to hand back, and a fuzzier rollback story. It moves
  Burrow's core value (guarded, legible, reversible operations) out of the loop.
- **Deploy by git reference in v0.1** (build-from-source as the default trigger). Rejected
  for v0.1: server-side build is a later build path ([ADR-0008](0008-two-build-paths.md));
  the v0.1 spine is deploy-by-image-reference, which keeps the control plane out of the
  build business for the first slice.
- **Deploy without recording the prior state.** Rejected: it would make `rollback`
  best-effort guesswork. The recorded deploy *is* the rollback handle.
