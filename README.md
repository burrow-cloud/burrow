# Burrow

**Burrow is an agent-native cloud platform.** It lets an AI coding agent — Claude Code,
Cursor, Codex, Cowork, anything that speaks [MCP](https://modelcontextprotocol.io) —
deploy and operate real applications on your own Kubernetes cluster. You tell your agent
"deploy this," "roll it back," "show me the logs," "scale it," and Burrow carries it out
safely on your cluster.

> ### ✅ v0.2 is shipped
>
> Install the control plane into your own Kubernetes cluster, point an MCP agent at it, and
> deploy and operate a real app — `deploy`, `status`, `logs`, `rollback`, `scale`, plus
> in-place `upgrade`. v0.2 adds the other half: **reach a deployed app at a URL** — `expose`
> an app over HTTPS (ingress + cert-manager TLS), register a DNS provider (DigitalOcean or
> Cloudflare), and point a domain at the cluster with `domain add`, all guided by an
> introspectable `reachability` report. Every mutating operation is guarded by the control
> plane. The [version status](#version-status) table tracks what's shipped vs. planned
> ([ADR-0009](docs/adr/0009-honest-status.md)). See
> [licensing](#license-and-contributing) for how the code is licensed.

## What it is

The first user is a solo developer or small agency who already has a Kubernetes cluster
(for example on DigitalOcean), installs Burrow into it, points their agent at it, and
operates their infrastructure by talking to the agent. **Compute first:** v0.1
deploys your code and runs it. Databases, domains, autoscaling, and cost controls come
later ([roadmap](docs/ROADMAP.md)).

It's fully self-hostable: the single-tenant control plane, the MCP server, and the CLI,
packaged so you can run the whole thing on your own cluster.

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

You need a Kubernetes cluster you can reach with `kubectl` (DigitalOcean is the reference
target) and Go to build the CLI — a Homebrew tap is
[proposed](docs/adr/0016-cli-distribution-and-upgrade-lifecycle.md) so this won't need a Go
toolchain later.

```sh
# Build the CLI and the MCP server
go build -o burrow     ./cmd/burrow
go build -o burrow-mcp ./cmd/burrow-mcp

# Install the control plane into your cluster (uses your kubeconfig; waits until ready)
./burrow install

# Point your agent at the MCP server — it auto-connects via your kubeconfig, no extra config.
#   (Claude Code, for example:)  claude mcp add burrow "$(pwd)/burrow-mcp"

# Using a private registry? Give the cluster a pull credential once (reuses your docker login):
./burrow config registry login ghcr.io --from-docker-config

# Build and push your image to a registry your cluster can pull from, then ask your agent to
# deploy it — or do it directly with the CLI:
./burrow app deploy web --image nginx:alpine
./burrow app status web
```

Update the control plane later with `burrow upgrade` — in place, preserving your state.

## Version status

Burrow follows semver from v0.1 toward v1.0. This table never lags the code
([ADR-0009](docs/adr/0009-honest-status.md)).

| Version | Scope | Status |
| --- | --- | --- |
| **v0.1** | Install into an existing cluster · connect an agent over MCP · deploy an image by reference · status · logs · rollback · scale · in-place upgrade | ✅ shipped ([v0.1.1](https://github.com/burrow-cloud/burrow/tree/v0.1.1)) |
| **v0.2** | Reach a deployed app at a URL: shared-ingress routing · `expose` + TLS via cert-manager · `reachability` surface · DNS automation (DigitalOcean / Cloudflare providers, `domain add/remove`) · `ingress install` setup · configurable guardrail policy | ✅ shipped ([v0.2.1](https://github.com/burrow-cloud/burrow/tree/v0.2.1)) |
| **v0.3** | Operability + agent-experience hardening: CLI grouped by task (`app`/`config`/`system`, `expose`→`publish`) · `app list` · account-scoped Cloudflare tokens · ingress-class binding · public-DNS reachability checks · burrowd request log | 🚧 in progress |
| v0.4 | Server-side build from a git reference · DNS-01 issuer · provider-record reachability · `app delete` · further day-two operations | planned ([roadmap](docs/ROADMAP.md)) |
| v1.0 | Production self-host: hardened deploy-and-operate core with day-two operations | planned ([roadmap](docs/ROADMAP.md)) |

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the system design: the four layers, the
  invariants, and the request paths.
- [docs/HARDENING.md](docs/HARDENING.md) — securing your agent: keep the control plane its
  only path to the cluster so the guardrails actually bound it.
- [docs/ROADMAP.md](docs/ROADMAP.md) — version milestones, v0.1 → v1.0.
- [docs/PLAN.md](docs/PLAN.md) — the current execution plan and the v0.1 scope.
- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: every load-bearing
  decision, with its reasoning and rejected alternatives.
- [CLAUDE.md](CLAUDE.md) — the contributor and agent guide: invariants, Go conventions, code
  layout, build/test commands, and workflow.

## License and contributing

How the code is licensed ([LICENSING.md](LICENSING.md),
[ADR-0001](docs/adr/0001-license-and-dco.md)):

- The **client surface is Apache-2.0** — the CLI (`cmd/burrow/`) and the MCP server
  (`mcp/`). Integrate against it freely.
- The **control plane and operator are source-available under FSL-1.1-ALv2** — read,
  modify, and self-host them, with the full source in the open. **Each release converts to
  Apache-2.0 two years after it ships** — a posture that opens up over time.
- **Commercial licenses** are available for teams that want terms beyond the FSL grant —
  see [COMMERCIAL.md](COMMERCIAL.md).

**Contributions are welcome** — open an issue or a discussion. Bug reports, ideas, and
design feedback are the best way to help and to shape where Burrow goes. Commits are signed
off under the Developer Certificate of Origin (`git commit -s`). See
[CONTRIBUTING.md](CONTRIBUTING.md) for the details.
