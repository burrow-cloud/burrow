# Burrow

**Burrow is an agent-native cloud platform.** It lets an AI coding agent — Claude Code,
Cursor, Codex, Cowork, anything that speaks [MCP](https://modelcontextprotocol.io) —
deploy and operate real applications on your own Kubernetes cluster. You tell your agent
"deploy this," "roll it back," "show me the logs," "scale it," and Burrow carries it out
safely on your cluster.

> ### 🚧 Status: pre-implementation
>
> Burrow is being designed in the open, ahead of the code. **Nothing described here works
> yet** — this repository currently holds the foundations: the architecture, the design
> records, and the v0.1 plan. Everything below is a goal until it ships and is marked in
> the [version status](#version-status) table ([ADR-0009](docs/adr/0009-honest-status.md)).
> Burrow is **open core**, not unqualified "open source" — see
> [licensing](#license-and-contributing).

## What it is

The first user is a solo developer or small agency who already has a Kubernetes cluster
(for example on DigitalOcean), installs Burrow into it, points their agent at it, and
operates their infrastructure by talking to the agent. **Compute first:** the v0.1 job is
deploying your code and running it. Databases, domains, autoscaling, and cost controls come
later ([roadmap](docs/ROADMAP.md)).

This repository is the **open core**: the single-tenant control plane, the MCP server, and
the CLI, packaged so you can self-host the whole thing. A separate, private product — the
multi-tenant managed cloud (billing, teams, dashboard, SSO) — is built on top of this core
and does not live here.

## How it works

Burrow is four layers ([architecture](docs/ARCHITECTURE.md)):

1. **Your agent** — not ours. Any MCP client.
2. **The MCP server** — thin, agent-neutral, holds no cluster credentials. The remote
   control.
3. **The control plane** — the product. Deploy orchestration, build-to-image pipeline,
   rollout and rollback, logs and status, scaling, the safety guardrails, and the record of
   who deployed what. Holds the cluster credentials; the only layer that talks to
   Kubernetes.
4. **Kubernetes** — your cluster, the runtime Burrow operates on top of.

Two ideas keep it safe and fast:

- **Code never travels over MCP.** MCP carries only tool calls and small metadata — an
  image reference, env vars, a command. The built container image moves through a container
  registry, never the agent connection. *MCP is the remote control; the registry is the
  conveyor belt.* ([ADR-0004](docs/adr/0004-code-never-over-mcp.md))
- **Guardrails live in the control plane**, between your agent and your cluster. Dangerous
  operations are gated or refused there, and every operation returns a structured result
  the agent can reason over. ([ADR-0006](docs/adr/0006-guardrails-in-the-control-plane.md))

## How to try it

**Not yet — there is nothing to install.** When the v0.1 slice ships, the flow will be:
install the control plane and MCP server into your existing cluster, point your agent at
the MCP server, build and push your image to a registry, and ask your agent to deploy it by
reference — then `status`, `logs`, `rollback`, and `scale`. The exact v0.1 scope is in
[docs/PLAN.md](docs/PLAN.md).

## Version status

Burrow follows semver from v0.1 toward v1.0. This table never lags the code
([ADR-0009](docs/adr/0009-honest-status.md)).

| Version | Scope | Status |
| --- | --- | --- |
| **v0.1** | Install into an existing cluster · connect an agent over MCP · deploy an image by reference · status · logs · rollback · scale | 🚧 in progress (pre-implementation; [scope proposed](docs/PLAN.md)) |
| v0.2+ | Server-side build · richer guardrails · databases · domains/TLS · autoscaling · cost controls | planned ([roadmap](docs/ROADMAP.md)) |
| v1.0 | Production self-host: hardened deploy-and-operate core with day-two operations | planned ([roadmap](docs/ROADMAP.md)) |

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the system design: the four layers, the
  invariants, and the request paths.
- [docs/ROADMAP.md](docs/ROADMAP.md) — version milestones, v0.1 → v1.0.
- [docs/PLAN.md](docs/PLAN.md) — the current execution plan and the v0.1 scope.
- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: every load-bearing
  decision, with its reasoning and rejected alternatives.
- [CLAUDE.md](CLAUDE.md) — the contributor and agent guide: invariants, Go conventions, code
  layout, build/test commands, and workflow.

## License and contributing

Burrow is **open core** ([LICENSING.md](LICENSING.md),
[ADR-0001](docs/adr/0001-license-and-dco.md)):

- The **client surface is Apache-2.0** — the CLI (`cmd/burrow/`) and the MCP server
  (`mcp/`). Integrate against it freely.
- The **control plane and operator are source-available under FSL-1.1-ALv2** — you can read,
  modify, and self-host them; the only thing forbidden is reselling Burrow as a competing
  hosted service. **Each release converts to Apache-2.0 two years after it ships.** This is
  a deliberate starting posture, opening over time — not a permanent enclosure.
- **Commercial licenses are available** for use outside the FSL grant (e.g. offering Burrow
  as a service) — see [COMMERCIAL.md](COMMERCIAL.md), contact Nicholas Phillips
  <hello@burrow-cloud.dev>.

**Contributing:** issues and discussions are the way to contribute and are fully open; the
maintainer keeps sole copyright (so Burrow can be offered under commercial licenses), so
outside *code* PRs are not merged except under a CLA. All commits are signed off under the
Developer Certificate of Origin (`git commit -s`). See
[CONTRIBUTING.md](CONTRIBUTING.md).
