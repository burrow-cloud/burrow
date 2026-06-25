# ADR-0021: Guardrails bound the control-plane path; the operator restricts the agent's other paths

## Status

Accepted.

## Context

[ADR-0006](0006-guardrails-in-the-control-plane.md) puts the guardrails in the control
plane, between the agent and the cluster, and [ADR-0020](0020-guardrails-as-configurable-policy.md)
makes them configurable while keeping `guard set` off the MCP surface, so an agent restricted
to Burrow's MCP tools cannot change its own guardrails.

That protection assumes **the control plane is the agent's only path to the cluster**. Real
coding agents (Claude Code, Cursor, and the like) run with a shell. With a shell, an agent
can:

- run the `burrow` CLI directly — including `burrow guard set` — bypassing the MCP-surface
  restriction; and, more seriously,
- use `kubectl` with the developer's kubeconfig to operate the cluster directly, never
  touching `burrowd`, so none of the guardrails apply.

Burrow cannot close this from the inside: it governs operations that flow through its control
plane; it has no control over an agent's other tools or the developer's ambient kubeconfig.
This is the **self-host** model, where the developer holds a privileged kubeconfig
([ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)). The **managed** model differs —
there the agent never receives a kubeconfig and the managed MCP server is the only path by
construction, so the side channel does not exist.

## Decision

State it plainly: **Burrow's guardrails are a real boundary only when the control plane is
the agent's sole path to the cluster, and making that true is the operator's responsibility,
enforced at the agent's own permission layer — not by Burrow.**

The recommended self-host posture is to configure the agent so its only cluster lever is
Burrow's MCP tools:

- **Deny the agent the `burrow` CLI** — so it cannot `guard set` and cannot shell around the
  guarded MCP tools; it must use `burrow_deploy` / `burrow_scale` / etc.
- **Deny direct cluster tooling** (`kubectl`, `helm`, anything that uses the kubeconfig) — so
  a `deny` / `confirm` guardrail cannot be sidestepped.
- **Allow image build/push** (`docker`) — the client-side build path still needs it
  ([ADR-0008](0008-two-build-paths.md)).

The exact mechanism is **per agent CLI**: Burrow documents the principle and gives a worked
example ([docs/HARDENING.md](../HARDENING.md)), but each operator applies it in their own
agent's permission system. `guard set` stays CLI-only (ADR-0020) — that keeps it off the MCP
surface; the agent deny-list closes the shell path.

This is **defense-in-depth for cooperative agents**, consistent with the honesty of the
`confirm` disposition ([ADR-0020](0020-guardrails-as-configurable-policy.md),
[ADR-0009](0009-honest-status.md)): a real boundary for an over-eager assistant that honors
its permission configuration, not a sandbox against a malicious or permission-bypassing agent.

## Consequences

- Burrow ships **no enforcement** of this — it cannot — so the documentation must carry the
  guidance. [docs/HARDENING.md](../HARDENING.md) holds the rationale and a concrete, verified
  example.
- **Honest status** ([ADR-0009](0009-honest-status.md)): the docs must not imply Burrow
  sandboxes the agent. The guardrails' strength is scoped to the control-plane path, and the
  README and guardrail docs say so.
- This makes the ADR-0020 claim precise: "`guard set` is CLI-only so the agent cannot change
  its guardrails" holds for an MCP-scoped agent; for a shell-capable agent the operator closes
  the gap at the permission layer.
- The managed product has a stronger story for free — no developer kubeconfig, managed MCP as
  the only path — so this operator burden is specific to self-host.

## Rejected alternatives

- **Enforce control-plane-only access from inside Burrow.** Rejected: impossible. Burrow has
  no control over the agent's other tools or the developer's kubeconfig.
- **Expose `guard set` over MCP behind its own confirm gate.** Rejected (ADR-0020): the agent
  must not be able to change its own guardrails at all, and it would not address the `kubectl`
  side channel regardless.
- **Remove the `guard set` CLI command and require editing the database.** Rejected: the human
  operator needs an ergonomic lever, and the CLI is the human's interface. The fix is denying
  the *agent* the CLI, not removing the human's.
- **Ship a hardened agent permission file in the repo.** Rejected as the primary answer: the
  mechanism differs per agent CLI and per user setup, so a single committed file would be wrong
  for most. The guide explains the principle and gives one worked example to adapt.
