# ADR-0005: The MCP server holds no cluster credentials

## Status

Accepted.

## Context

Something in Burrow must hold the credentials that talk to the Kubernetes cluster — the
kubeconfig, service-account token, or equivalent that grants real power over the user's
infrastructure. Where those credentials live determines where the security boundary is.

The MCP server is the layer closest to the agent, and the agent is untrusted and
interchangeable ([ADR-0002](0002-four-layer-architecture.md),
[ADR-0003](0003-agent-neutral-mcp-control-surface.md)). It is also the layer most exposed:
it speaks a network protocol to whatever client connects. Putting cluster credentials
there would make the most-exposed, least-trusted-adjacent component the keeper of the
keys.

## Decision

**The MCP server holds no cluster credentials. The control plane does.** Cluster
credentials live only in the control plane, which is the one layer that talks to
Kubernetes ([ADR-0002](0002-four-layer-architecture.md)). The MCP server authenticates to
the control plane and forwards tool calls; it never possesses, proxies, or sees the
credentials that operate the cluster.

The security boundary is therefore the **control plane**, not the thin MCP layer. The
question "can this operation touch the cluster?" is answered in one place, behind the
credentials, by the guardrails ([ADR-0006](0006-guardrails-in-the-control-plane.md)) —
not at the edge where the agent connects.

## Consequences

- **Compromising or impersonating the MCP server does not yield cluster credentials.** An
  attacker at the MCP layer can attempt control-plane calls, but those calls hit the
  control plane's authentication and guardrails; the keys to Kubernetes are never at the
  edge to steal.
- **The MCP server can be deployed in more-exposed positions** (closer to the agent, in a
  less-trusted network) without widening the blast radius, because it is credential-free.
- **The control plane authenticates its callers.** Since the MCP server (and the CLI) are
  clients, the control plane needs its own auth for those clients — defining that is part
  of the v0.1 work (see [docs/PLAN.md](../PLAN.md)).
- **There is exactly one credential holder to secure, audit, and rotate.** Cluster-access
  hardening has a single home.
- This reinforces the thinness of the MCP server ([ADR-0002](0002-four-layer-architecture.md)):
  with no credentials and no orchestration, there is little there to attack.

## Rejected alternatives

- **Store cluster credentials in the MCP server (or let it hold a delegated cluster
  token).** Rejected: it puts the keys in the most-exposed, agent-adjacent layer and moves
  the security boundary to exactly the wrong place.
- **Give the agent cluster credentials directly and make Burrow a pass-through.** Rejected:
  the agent is untrusted and interchangeable; Burrow exists precisely so that the agent
  *never* holds cluster power and instead drives it through a guarded boundary.
- **Split credentials across MCP server and control plane.** Rejected: two credential
  holders means two things to secure and audit, and any cluster power at the MCP layer
  defeats the purpose. One holder, one boundary.
