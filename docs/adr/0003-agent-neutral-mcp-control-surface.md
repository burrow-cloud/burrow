# ADR-0003: An agent-neutral MCP control surface

## Status

Accepted.

## Context

Burrow is operated by an AI coding agent. There are many such agents — Claude Code,
Cursor, Codex, Cowork — and more will appear. Burrow could tie itself to one (richer
integration, a tighter demo) or stay neutral across all of them. The agent ecosystem is
young and moving fast; betting the control surface on one vendor's evolving,
proprietary integration API would couple Burrow's fate to that vendor's roadmap and
shut out every other agent's users.

The Model Context Protocol (MCP) is the emerging open standard for exactly this: a
client-agnostic way for agents to discover and call tools. It is the natural neutral
interface.

## Decision

Burrow's control surface is an **MCP server**, and it is **agent-neutral**: it targets
the MCP standard, not any specific agent. Any MCP-speaking client can drive Burrow with
no Burrow-side, agent-specific code.

- The MCP server exposes Burrow's operations as MCP tools (`deploy`, `status`, `logs`,
  `rollback`, `scale`, and so on) with clear schemas and structured results.
- Tool definitions, descriptions, and result shapes are written for a general
  tool-using agent — not tuned to one agent's prompting quirks or proprietary extensions.
- Nothing in the MCP server or the control plane branches on "which agent is calling."
  Agent-neutrality is a property of the design, not a configuration.

## Consequences

- **Every MCP client is a first-class Burrow operator on day one** — no per-agent
  integration work, no waiting for a vendor partnership.
- **Tools are designed for autonomy.** Because the caller is an autonomous agent, every
  tool returns a structured, machine-reasoned result (success/failure, what changed, how
  to undo it) rather than prose meant for a human — this dovetails with guardrails
  returning structured outcomes ([ADR-0006](0006-guardrails-in-the-control-plane.md)).
- **The MCP surface can track the MCP standard** as it matures, independently of any one
  agent's release cycle.
- We forgo deep, agent-specific niceties that a single-vendor integration could offer.
  This is an accepted trade: neutrality and reach beat one-vendor polish for a platform
  whose value grows with how many agents can use it.
- A richer integration for a specific agent, if ever justified, is layered *on top of*
  the neutral MCP surface — never by specializing the surface itself.

## Rejected alternatives

- **Bind to a single agent's native integration API.** Rejected: it couples Burrow to one
  vendor's roadmap and excludes every other agent's users, for a young market where no
  winner is settled.
- **Invent a Burrow-specific agent protocol.** Rejected: it asks every agent to add
  bespoke Burrow support, which is precisely the adoption friction MCP exists to remove.
- **Support several agent protocols in parallel.** Rejected: multiplies surface area and
  guardrail/test burden with no benefit over the one open standard the agents already
  converge on.
