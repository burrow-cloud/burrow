# ADR-0006: Guardrails live in the control plane

## Status

Accepted.

## Context

Burrow is operated by an autonomous agent acting on a user's real infrastructure. Agents
make mistakes: they hallucinate arguments, misread state, retry destructively, or take a
plausible-but-wrong action. Burrow's promise is that the agent can operate infrastructure
*safely*. That promise has to be enforced somewhere structural — not left to the agent's
good behavior, and not scattered across clients.

There are three candidate homes for safety enforcement: the agent (trust it to be
careful), the MCP server (gate at the edge), or the control plane (gate at the boundary
that holds the credentials and talks to Kubernetes). Only the last is both trustworthy and
unavoidable: the agent is untrusted and interchangeable, and the MCP server is a thin,
credential-free, replaceable front-end with the CLI as a parallel client
([ADR-0002](0002-four-layer-architecture.md), [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)).
A guardrail in the MCP server is bypassed the moment someone uses the CLI or a second
front-end.

## Decision

**Guardrails live in the control plane, between the agent and the cluster** — at the same
boundary that holds the cluster credentials. Every operation that can touch the cluster
passes through them, no matter which client (MCP server, CLI, future dashboard) initiated
it.

Two rules define the guardrails:

1. **Dangerous operations are gated or refused in the control plane.** Destructive or
   high-blast-radius actions are checked against policy and either require explicit
   confirmation, are constrained, or are refused outright — decided behind the
   credentials, not at the edge.
2. **Every operation returns a structured result the agent can reason over.** Success or
   failure, what changed, the current state, and — where relevant — how to undo it, are
   returned as structured data, not prose. An autonomous caller needs machine-checkable
   outcomes to decide what to do next ([ADR-0003](0003-agent-neutral-mcp-control-surface.md)).

## Consequences

- **Safety holds for every client by construction.** Because the guardrails sit at the
  credential boundary, the CLI, the MCP server, and any later front-end are all equally
  governed; none can route around them.
- **Structured results are part of every operation's contract**, not an MCP-presentation
  concern. The control-plane API returns them; the MCP server passes them through.
- **What counts as "dangerous," and how each gate behaves** (refuse / confirm / constrain)
  is itself a design surface — the specific v0.1 guardrails are scoped in
  [docs/PLAN.md](../PLAN.md). This ADR fixes *where* guardrails live and *that* operations
  return structured results; it does not enumerate the policy.
- **Rollback is a first-class, guarded operation, not an afterthought.** Because every
  change returns how to undo it and the control plane records what was deployed
  ([ADR-0007](0007-explicit-deploy-by-image-reference.md)), recovering from a bad agent
  action is a supported path.
- Guardrail logic is control-plane code and is unit-testable against faked Kubernetes,
  registry, and database seams — safety is covered by tests, not hope.

## Rejected alternatives

- **Enforce safety in the MCP server (gate at the edge).** Rejected: the MCP server is
  thin, replaceable, and one of several clients; an edge guardrail is bypassed by the CLI
  or any other front-end. Safety must sit where the credentials are.
- **Trust the agent to operate carefully (guardrails in prompts/tool descriptions).**
  Rejected: the agent is untrusted and interchangeable, and prompt-level guidance is not
  enforcement. Tool descriptions can *advise*; they cannot *gate*.
- **Return prose results and let the agent parse them.** Rejected: autonomous callers need
  structured, machine-checkable outcomes to reason and recover; prose is lossy and
  ambiguous exactly when it matters most (failures, partial rollouts).
