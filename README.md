# Burrow

**Production grade self hosting for your apps, operated by your AI agent, with guardrails.**
Point [Claude Code](https://claude.com/claude-code), Cursor, Codex, or bring your own at your
Kubernetes cluster. It deploys, scales, debugs, and ships your apps to a URL over
[MCP](https://modelcontextprotocol.io). You own the infrastructure and the control plane that
drives it, and every change the agent makes is gated by a guardrail, so it can't break prod.

It is not another git deploy host. Vercel and friends run your app on *their* platform; Burrow
operates *your* cluster (real Kubernetes, done right), and goes past the app to the whole
stack. The agent writes the integration code, and Burrow stands up and runs the backing
service behind the same guardrails.

## Why Burrow

- **Own your infrastructure, no lock-in.** The control plane is yours. It self hosts in your
  own cluster, holds the cluster credentials, and is open source (Apache-2.0). No vendor
  platform sits in the path, and there is nothing to migrate off of later.
- **Production grade by default.** It is real Kubernetes done right: self-healing workloads,
  rolling deploys, ingress and TLS, in-place upgrades, with sane defaults, so the hard parts
  are handled rather than left as homework.
- **A guardrailed agent, safe enough for prod.** The agent is a tool you drive, not an
  autonomous operator turned loose on a cluster. It proposes, you approve, and every mutating
  operation passes the control-plane guardrails and lands in an audit trail. Secrets never
  travel over MCP. That control model is what makes letting an agent operate production
  acceptable.

## Talk to your agent

What you can say today (✅), and where it is headed (🔭):

- ✅ **"Deploy `ghcr.io/me/app:1.4` and serve it at example.com over HTTPS."** The image,
  ingress and TLS, and the DNS record, from one ask.
- ✅ **"Roll back the last release."** · **"Scale web to 3."** · **"Show me the logs."** ·
  **"Is my app reachable? If not, what's broken?"**
- ✅ **"How is my app doing?"** · **"Why is it slow?"** → Burrow installs logs
  ([VictoriaLogs](https://docs.victoriametrics.com/victorialogs/)) on your cluster, *or connects*
  to the logs and metrics you already run ([Loki](https://grafana.com/oss/loki/),
  [Prometheus](https://prometheus.io)), and your agent *queries* them and answers in plain
  language: answers, not dashboards.
- ✅ **"My site is slow, add a cache."** → your agent writes the [ValKey](https://valkey.io)
  integration; Burrow deploys ValKey to your cluster and wires it in.
- 🔭 **"Autoscale web on load."** · **"Show me this month's cluster spend."** → autoscaling and
  cost controls are on the [roadmap](docs/ROADMAP.md).

The pattern is the same every time: the agent writes the code; **Burrow provisions the
vetted, permissively-licensed building block, wires it in with sane defaults, and operates
it**, with every change gated by the control-plane guardrails. The ✅ items work now, and the
🔭 items are the [roadmap](docs/ROADMAP.md). The [version table](#version-status) never lags
the code.

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
- **"How is my app doing?"** — the agent installs logs on your cluster, or connects to the logs
  and metrics you already run, and answers in plain language (shipped in v0.4).

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

## What "guardrails" mean

Letting an agent operate a cluster is only acceptable if it cannot do something you did not
approve. In Burrow, guardrails are policy that lives in the **control plane, not in the
agent**, so the agent can't bypass them or change its own limits. Every mutating operation
passes through them and gets one of three dispositions:

- **allow**: the operation runs.
- **confirm**: it is held until you approve. The agent gets back a structured "needs
  confirmation" result, not a success, so it has to ask first.
- **deny**: it is blocked outright.

You set the policy from the CLI. There is deliberately **no MCP tool for it**, so an agent can
never loosen its own guardrails:

```sh
burrow guard list                       # every guardrail and its current disposition
burrow guard set app.delete deny            # never let the agent delete an app
burrow guard set app.scale_to_zero confirm  # scaling to zero needs your sign-off
burrow guard set app.expose_public confirm  # putting something on the public internet needs approval
```

Guardrails cover the operations that carry risk: deleting an app, scaling to zero, exposing a
service publicly, DNS changes, installing or removing add-ons, restoring a database, rolling
back. Every attempt and its outcome (allowed, held, denied, executed) lands in an append-only
audit log you read with `burrow audit`. And secrets never travel over MCP at all: the agent
references them by key, you set the values, and they are written straight into a Kubernetes
Secret.

*Per-environment guardrails, so prod can be locked down while staging stays permissive, are on
the [roadmap](docs/ROADMAP.md).*

## Try it

You need a cluster you can reach with `kubectl` (DigitalOcean is the reference target). Install
the CLIs with Homebrew:

```sh
brew install burrow-cloud/tap/burrow     # installs burrow and burrow-mcp

burrow install                           # control plane to your cluster (uses your kubeconfig)
claude mcp add burrow burrow-mcp         # point your agent at it (auto-connects via kubeconfig)

# then just talk to your agent, or drive it directly:
burrow app deploy web --image nginx:alpine
burrow app status web
```

Prefer to build from source? `go build -o burrow ./cmd/burrow && go build -o burrow-mcp ./cmd/burrow-mcp`.

`burrow upgrade` rolls the control plane forward in place, preserving your state.

## Version status

Burrow follows semver from v0.1 toward v1.0. This table never lags the code
([ADR-0009](docs/adr/0009-honest-status.md)).

| Version | Scope | Status |
| --- | --- | --- |
| **v0.1** | Install into a cluster · connect an agent over MCP · deploy by image reference · status · logs · rollback · scale · in-place upgrade | ✅ shipped ([v0.1.1](https://github.com/burrow-cloud/burrow/tree/v0.1.1)) |
| **v0.2** | Reach an app at a URL: shared-ingress routing · `publish` + cert-manager TLS · `reachability` surface · DNS automation (DigitalOcean / Cloudflare) · `ingress install` · configurable guardrails | ✅ shipped ([v0.2.1](https://github.com/burrow-cloud/burrow/tree/v0.2.1)) |
| **v0.3** | Operability + agent-experience hardening: CLI grouped by task (`app`/`config`/`system`) · `app list` · account-scoped Cloudflare tokens · public-DNS reachability · request log | ✅ shipped ([v0.3.0](https://github.com/burrow-cloud/burrow/tree/v0.3.0)) |
| **v0.4** | Agent-provisioned building blocks: install logs (VictoriaLogs) / metrics (VictoriaMetrics + vmagent) — or **connect** the logs/metrics you already run (Loki, Prometheus) — and query them to answer "how is my app doing?" · cache (ValKey) · `app delete` · tunable rollback guardrail | ✅ shipped ([v0.4.0](https://github.com/burrow-cloud/burrow/tree/v0.4.0)) |
| **v0.5** | App config, secrets, credentials, and the audit log: `app env` / `app secret` lifecycle (`deploy` takes no env) · secrets & vendor/connected-backend credentials flow through the control-plane API, **never MCP**, never logged · `burrow audit` trail of guarded operations · apps default to a dedicated `burrow-apps` namespace | ✅ shipped ([v0.5.0](https://github.com/burrow-cloud/burrow/tree/v0.5.0)) |
| **v0.6** | First backend building block, agent-native onboarding, and the Apache relicense: Postgres add-on (`addon install`/`attach postgres`, a per-app database + role, generated `DATABASE_URL` wired into the app) · `pg_dump`/`pg_restore` backups behind a confirm guardrail · read-only `burrow_audit` MCP tool · agent-native onboarding (cluster-capability detection, cost-aware ingress/TLS provisioning with a LoadBalancer-vs-NodePort choice, a verified "live at https://…" check) · dotted guardrail codes (`app.delete`) · **whole repository relicensed to Apache-2.0** | ✅ shipped ([v0.6.0](https://github.com/burrow-cloud/burrow/tree/v0.6.0)) |
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
[ADR-0033](docs/adr/0033-relicense-to-apache.md)):

- **All of Burrow's code in this repository is Apache-2.0** — the CLI, the MCP server, the
  control plane, and the operator. Read, modify, self-host, and integrate against it freely.
- Burrow is **open core**: the managed cloud and the enterprise tier (SSO, teams, compliance,
  support) are separate, proprietary products — see [COMMERCIAL.md](COMMERCIAL.md).

**Contributions are welcome** — open an issue or a discussion. Bug reports, ideas, and design
feedback are the best way to help and to shape where Burrow goes. Commits are signed off under
the Developer Certificate of Origin (`git commit -s`). See [CONTRIBUTING.md](CONTRIBUTING.md).
