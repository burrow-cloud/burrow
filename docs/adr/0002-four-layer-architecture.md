# ADR-0002: Four-layer architecture; the control plane is the product

## Status

Accepted.

## Context

Burrow lets an AI coding agent deploy and operate real applications on a Kubernetes
cluster by driving Burrow through an MCP server. Building this, there is a recurring
temptation to collapse layers — to let the MCP server talk to Kubernetes directly, or to
push orchestration logic into the CLI, or to treat the agent as part of the system. Each
collapse trades away a property Burrow depends on (a clean security boundary, a single
place for guardrails, agent-neutrality). This ADR fixes the layering so those properties
are structural rather than incidental.

## Decision

Burrow is four layers, with a sharp line about which ones are "ours":

1. **The agent.** Not ours. Any MCP client — Claude Code, Cursor, Codex, Cowork, or
   anything else that speaks MCP. Burrow makes no assumptions about which agent is
   driving it (see [ADR-0003](0003-agent-neutral-mcp-control-surface.md)).
2. **The MCP server.** Thin. It exposes Burrow's tools to the agent and translates tool
   calls into control-plane calls. It holds no cluster credentials
   ([ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)) and contains no
   orchestration logic. It is the remote control, not the engine.
3. **The control plane.** **This is the product.** Deploy orchestration, the
   build-to-image pipeline, rollout and rollback, logs and status, scaling, the safety
   guardrails ([ADR-0006](0006-guardrails-in-the-control-plane.md)), and the record of
   who deployed what. It holds the cluster credentials and is the only layer that talks
   to Kubernetes.
4. **Kubernetes.** The runtime Burrow operates on top of. Burrow targets it; it is not
   part of Burrow.

The substance of the system lives in layer 3. Layers 1 and 4 are not ours, and layer 2
is deliberately thin. Engineering effort, the security boundary, and the design attention
concentrate on the control plane.

## Consequences

- **The MCP server and the control plane are separate processes with a defined API
  between them.** The MCP server is a client of the control plane, exactly as the CLI is
  ([ADR-0008](0008-two-build-paths.md)). The control plane's API is the real interface;
  MCP is one front-end onto it.
- **The control plane is the only layer with cluster credentials and the only layer that
  calls the Kubernetes API.** This makes it the security boundary
  ([ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)) and the natural and only
  home for guardrails ([ADR-0006](0006-guardrails-in-the-control-plane.md)).
- **The MCP server stays thin and replaceable.** Because translation is all it does, a
  second front-end (the CLI today, a self-host dashboard later) is just another client of
  the same control-plane API — no orchestration logic is duplicated.
- **Agent-neutrality is structural.** Nothing in layers 2 or 3 is specialized to a
  particular agent ([ADR-0003](0003-agent-neutral-mcp-control-surface.md)).
- The control plane keeps its own state (deploy records, rollout history) in its own
  database, independent of cluster state.

## Rejected alternatives

- **Collapse the MCP server and control plane into one process.** Rejected: it would put
  cluster credentials behind the MCP connection and erase the security boundary
  ([ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)), and it would couple the
  control surface (which may change as MCP evolves) to the engine (which should be
  stable).
- **Let the MCP server call Kubernetes directly for "simple" operations.** Rejected:
  guardrails and the deploy record would then have two homes, and "simple" operations are
  exactly the ones that get dangerous at scale (a careless scale-to-zero, a delete). One
  path to the cluster, through the guardrails, every time.
- **Treat the agent as a trusted internal component.** Rejected: the agent is untrusted
  and interchangeable. Burrow's safety cannot depend on a particular agent behaving well;
  it must hold for any MCP client.
