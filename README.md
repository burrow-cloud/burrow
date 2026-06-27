# Burrow

**Burrow is an agent-native cloud platform.** Tell your AI coding agent — [Claude
Code](https://claude.com/claude-code), Cursor, Codex, anything that speaks
[MCP](https://modelcontextprotocol.io) — what you want, and Burrow carries it out on **your
own** Kubernetes cluster, safely behind guardrails.

It is not another git-deploy host. Vercel and friends run your app on *their* platform;
Burrow operates *your* cluster — and it goes past the app to the whole stack. Your agent
writes the integration code; Burrow stands up and runs the backing service.

## Talk to your agent

What you can say today (✅), and where it is headed (🔭):

- ✅ **"Deploy `ghcr.io/me/app:1.4` and serve it at example.com over HTTPS."** — the image,
  ingress + TLS, and the DNS record, from one ask.
- ✅ **"Roll back the last release."** · **"Scale web to 3."** · **"Show me the logs."** ·
  **"Is my app reachable? If not, what's broken?"**
- 🔭 **"How is my app doing?"** · **"Why is it slow?"** → Burrow stands up logs
  ([VictoriaLogs](https://docs.victoriametrics.com/victorialogs/)) and metrics
  ([VictoriaMetrics](https://victoriametrics.com)) on your cluster, and your agent *queries*
  them and tells you in plain language — answers, not dashboards.
- 🔭 **"My site is slow — add a cache."** → your agent writes the [ValKey](https://valkey.io)
  integration; Burrow deploys ValKey to your cluster and wires it in.

The pattern is the same every time: the agent writes the code; **Burrow provisions the
vetted, permissively-licensed building block, wires it in with sane defaults, and operates
it** — every change gated by the control-plane guardrails. The ✅ items work now; the 🔭 items
are the [roadmap](docs/ROADMAP.md). The [version table](#version-status) never lags the code.

## Built for day two

The hard part of Kubernetes isn't the first deploy — it's everything after. The recurring
complaint from small teams is the **day-two tax**: upgrades that break prod, certs and ingress
that drift, and "why is my app slow?" debugging across a stack nobody has time to master. That
second day is Burrow's job:

- **Upgrades in place** — `burrow upgrade` rolls the control plane forward without losing state.
- **Reachability you can reason about** — `reachability` walks the whole chain (controller →
  ingress → TLS → DNS) and names the *one* broken link, so "it's down" becomes "the cert hasn't
  issued yet."
- **Operate by talking** — status, logs, rollback, scale — the agent does the work, the
  guardrails keep it on the rails.
- **Soon: "how is my app doing?"** — the agent stands up logs and metrics on your cluster and
  answers in plain language (v0.4).

And every change is gated: **the agent proposes, you approve, it executes** — with the deploy
record as the audit trail. That human-in-the-loop step is what makes letting an agent operate
production actually acceptable.

## How it works

Four layers ([architecture](docs/ARCHITECTURE.md)):

1. **Your agent** — any MCP client, not ours.
2. **The MCP server** — thin, agent-neutral, holds no cluster credentials. The remote control.
3. **The control plane** — the product: deploy, rollout and rollback, status and logs,
   scaling, reachability, the guardrails, and the record of who did what. Holds the cluster
   credentials; the only layer that talks to Kubernetes.
4. **Kubernetes** — your cluster.

Two invariants keep it safe and fast: **code never travels over MCP** — only tool calls and
small metadata; the built image moves through a container registry
([ADR-0004](docs/adr/0004-code-never-over-mcp.md)) — and **guardrails live in the control
plane**, between your agent and your cluster, returning a structured result the agent can
reason over ([ADR-0006](docs/adr/0006-guardrails-in-the-control-plane.md)). It is fully
self-hostable: the single-tenant control plane, the MCP server, and the CLI run entirely on
your own cluster.

## Try it

You need a cluster you can reach with `kubectl` (DigitalOcean is the reference target) and Go
to build the CLI — a Homebrew tap is
[proposed](docs/adr/0016-cli-distribution-and-upgrade-lifecycle.md) so this won't need a Go
toolchain later.

```sh
go build -o burrow ./cmd/burrow && go build -o burrow-mcp ./cmd/burrow-mcp

./burrow install                            # control plane → your cluster (uses your kubeconfig)
claude mcp add burrow "$(pwd)/burrow-mcp"    # point your agent at it (auto-connects via kubeconfig)

# then just talk to your agent — or drive it directly:
./burrow app deploy web --image nginx:alpine
./burrow app status web
```

`burrow upgrade` rolls the control plane forward in place, preserving your state.

## Version status

Burrow follows semver from v0.1 toward v1.0. This table never lags the code
([ADR-0009](docs/adr/0009-honest-status.md)).

| Version | Scope | Status |
| --- | --- | --- |
| **v0.1** | Install into a cluster · connect an agent over MCP · deploy by image reference · status · logs · rollback · scale · in-place upgrade | ✅ shipped ([v0.1.1](https://github.com/burrow-cloud/burrow/tree/v0.1.1)) |
| **v0.2** | Reach an app at a URL: shared-ingress routing · `publish` + cert-manager TLS · `reachability` surface · DNS automation (DigitalOcean / Cloudflare) · `ingress install` · configurable guardrails | ✅ shipped ([v0.2.1](https://github.com/burrow-cloud/burrow/tree/v0.2.1)) |
| **v0.3** | Operability + agent-experience hardening: CLI grouped by task (`app`/`config`/`system`) · `app list` · account-scoped Cloudflare tokens · public-DNS reachability · request log | ✅ shipped ([v0.3.0](https://github.com/burrow-cloud/burrow/tree/v0.3.0)) |
| v0.4 | Agent-provisioned building blocks on your cluster: observability first — logs (VictoriaLogs) + metrics (VictoriaMetrics) the agent queries to answer "how is my app doing?" · `app delete` · cache (ValKey) later | planned ([roadmap](docs/ROADMAP.md)) |
| v1.0 | Production self-host: the deploy-and-operate core and common day-two building blocks, hardened | planned ([roadmap](docs/ROADMAP.md)) |

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the system design: the four layers, the
  invariants, and the request paths.
- [docs/HARDENING.md](docs/HARDENING.md) — keep the control plane the agent's only path to
  the cluster, so the guardrails actually bound it.
- [docs/ROADMAP.md](docs/ROADMAP.md) — version milestones, v0.1 → v1.0.
- [docs/PLAN.md](docs/PLAN.md) — the current execution plan.
- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: every load-bearing
  decision, with its reasoning and rejected alternatives.
- [CLAUDE.md](CLAUDE.md) — the contributor and agent guide.

## License and contributing

How the code is licensed ([LICENSING.md](LICENSING.md),
[ADR-0001](docs/adr/0001-license-and-dco.md)):

- The **client surface is Apache-2.0** — the CLI (`cmd/burrow/`) and the MCP server
  (`mcp/`). Integrate against it freely.
- The **control plane and operator are source-available under FSL-1.1-ALv2** — read, modify,
  and self-host them, with the full source in the open. **Each release converts to Apache-2.0
  two years after it ships.**
- **Commercial licenses** are available for terms beyond the FSL grant — see
  [COMMERCIAL.md](COMMERCIAL.md).

**Contributions are welcome** — open an issue or a discussion. Bug reports, ideas, and design
feedback are the best way to help and to shape where Burrow goes. Commits are signed off under
the Developer Certificate of Origin (`git commit -s`). See [CONTRIBUTING.md](CONTRIBUTING.md).
