# Burrow

<p align="center">
  <img src="docs/assets/mascot.jpg" alt="The Burrow mascot, a vigilant groundhog standing watch" width="300">
</p>

> For developers moving fast with AI who don't want to break things

**Production grade self hosting for your apps, operated by your AI agent, with guardrails.**
Point [Claude Code](https://claude.com/claude-code), Cursor, Codex, or bring your own at your
Kubernetes cluster. It deploys, scales, debugs, and ships your apps to a URL over
[MCP](https://modelcontextprotocol.io). You own the infrastructure and everything that drives
it, and every change the agent makes is gated by a guardrail, so it can't break prod.

It is not another git deploy host. Vercel and friends run your app on *their* platform; Burrow
operates *your* cluster (real Kubernetes, done right), and goes past the app to the whole
stack. The agent writes the integration code, and Burrow stands up and runs the backing
service behind the same guardrails.

## Why Burrow

- **Own your infrastructure, no lock-in.** Burrow self-hosts in your own cluster. It is open source (Apache-2.0), and there is no vendor platform in the path to migrate off later.
- **Production grade by default.** Real Kubernetes done right, controlled by your agent: self-healing workloads, rolling deploys, ingress and TLS, in-place upgrades, all with sane defaults.
- **Guardrails you control.** You decide what the agent can and cannot do, per environment. Lock prod down while staging stays free.

## Talk to your agent

Once Burrow is connected, you operate your apps by talking to your agent. Things you can say today:

- `Deploy my app to prod`
  - Burrow figures out which app you mean from the folder you are in, builds it, and rolls it out to your cluster.
- `Roll back the last release`
- `Scale web up` or `Scale web to 3`
- `Show me any 500 errors from my web app and figure out what happened`
  - Your agent reads the logs and tells you what broke, in plain language.
- `Why isn't my site reachable at example.com?`
  - Your agent walks the chain (DNS, TLS, cluster), finds the broken link, and proposes a fix.
- `How is my app doing?` or `Why is my app slow?`
  - Straight answers, no dashboards to dig through.
- `My site is slow, would a cache help?`
  - Your agent checks your logs and metrics, tells you if it would, and if you say yes, sets one up and wires your app to it.

Coming soon: `Make web autoscale at 90% CPU` and `Limit web to 500MB of memory and 1 CPU`.

## Guardrails

Guardrails are how you decide what your agent can and cannot do. Every risky action is checked against your policy before it runs, so the agent proposes and you stay in control.

    burrow guard set --env prod app.delete deny   # never let the agent delete an app in prod
    burrow guard set dns.write allow               # let it add DNS records
    burrow guard set dns.delete deny               # but never delete them

App-level rules can be scoped per environment, so the agent moves fast in staging while prod stays locked down.

## Addons

Building something new, or adding a capability? Ask your agent to stand up a backing service and wire your app to it:

- `Add a Postgres database and connect my app to it`
- `Set up logging so I can see what my app is doing`
- `Add a cache to speed up my site`

Available today: **Postgres** (a cluster-shared database), **logs** (VictoriaLogs), **metrics** (VictoriaMetrics), and **cache** (ValKey).

By default the agent asks before standing anything up: it proposes, you approve. Make it hands-off with `burrow guard set addon.install allow`, or install it yourself with `burrow addon install postgres`.

Want an addon we do not have yet? [Request one](https://github.com/burrow-cloud/burrow/issues/new?labels=addon).

## Built for day two

The hard part isn't the first deploy, it's everything after. Once your app is live, this is
what you get:

- **It keeps running.** Your app self-heals when something falls over, and new releases roll
  out without taking the old one down.
- **It stays reachable, over HTTPS.** Your app gets a URL with TLS, and when something is off
  you get a straight answer about which link in the chain broke ("the cert hasn't issued yet"),
  not just "it's down."
- **You operate it by talking.** Status, logs, rollback, scale: your agent does the work, and
  the guardrails keep it from breaking prod.
- **You get plain-language answers.** Ask "how is my app doing?" or "why is it slow?" and your
  agent reads the logs and metrics and tells you, no dashboards to dig through.

And every change is gated: **the agent proposes, you approve, it executes**, with a record of
what happened. That human-in-the-loop step is what makes letting an agent operate production
actually acceptable.

## How it works

Three things:

1. **Your agent** - any AI coding agent you already use (Claude Code, Cursor, Codex, Copilot,
   OpenCode).
2. **Burrow** - the piece you install. It holds your cluster credentials, does the work
   (deploy, scale, logs, backing services), and gates every change behind guardrails so the
   agent cannot break prod.
3. **Your Kubernetes cluster** - your infrastructure, which you own.

Two things keep it safe: your code never passes through Burrow (it moves through a container
registry, not the agent connection), and every change the agent tries to make is checked
before it runs. Nothing lands on your cluster that you did not approve.

For a deeper look at the architecture, see [Architecture](docs/ARCHITECTURE.md).

## Try it

You need a cluster you can reach with `kubectl`. Three
commands get you from nothing to an agent operating your cluster:

```sh
brew install burrow-cloud/tap/burrow     # installs burrow and burrow-mcp
burrow install <context>                 # installs Burrow into the named kube context (run `burrow install` to list them)
burrow mcp claude install                # connect your agent (or: cursor, codex, copilot, opencode)
```

Then just talk to your agent ("deploy this and serve it at example.com over HTTPS"), or drive
Burrow directly with `burrow app deploy web --image nginx:alpine` and `burrow app status web`.

See [docs/getting-started.md](docs/getting-started.md) for the full walkthrough, including the
complete list of supported agents and how to connect any other MCP-capable tool.

Prefer to build from source? `go build -o burrow ./cmd/burrow && go build -o burrow-mcp ./cmd/burrow-mcp`.

Upgrading later? See [Upgrade](docs/getting-started.md#upgrade).

## Shell completion

`burrow` ships completion for bash, zsh, fish, and PowerShell. Load it for your shell:

```sh
source <(burrow completion bash)                                  # bash (current shell)
burrow completion zsh > "${fpath[1]}/_burrow"                     # zsh
burrow completion fish > ~/.config/fish/completions/burrow.fish   # fish
burrow completion powershell | Out-String | Invoke-Expression     # PowerShell
```

Run `burrow completion <shell> --help` for how to install it permanently.

## Changelog

Burrow follows semver from v0.1 toward v1.0. This table never lags the code.

| Version | Scope | Status |
| --- | --- | --- |
| **v0.1** | Install into a cluster · connect an agent over MCP · deploy by image reference · status · logs · rollback · scale · in-place upgrade | ✅ shipped ([v0.1.1](https://github.com/burrow-cloud/burrow/tree/v0.1.1)) |
| **v0.2** | Reach an app at a URL: shared-ingress routing · `publish` + cert-manager TLS · `reachability` surface · DNS automation (DigitalOcean / Cloudflare) · `ingress install` · configurable guardrails | ✅ shipped ([v0.2.1](https://github.com/burrow-cloud/burrow/tree/v0.2.1)) |
| **v0.3** | Operability + agent-experience hardening: CLI grouped by task (`app`/`config`/`system`) · `app list` · account-scoped Cloudflare tokens · public-DNS reachability · request log | ✅ shipped ([v0.3.0](https://github.com/burrow-cloud/burrow/tree/v0.3.0)) |
| **v0.4** | Agent-provisioned building blocks: install logs (VictoriaLogs) / metrics (VictoriaMetrics + vmagent) — or **connect** the logs/metrics you already run (Loki, Prometheus) — and query them to answer "how is my app doing?" · cache (ValKey) · `app delete` · tunable rollback guardrail | ✅ shipped ([v0.4.0](https://github.com/burrow-cloud/burrow/tree/v0.4.0)) |
| **v0.5** | App config, secrets, credentials, and the audit log: `app config` / `app secret` lifecycle (`deploy` takes no config) · secrets & vendor/connected-backend credentials flow through Burrow's own API, **never MCP**, never logged · `burrow audit` trail of guarded operations · apps default to a dedicated `burrow-apps` namespace | ✅ shipped ([v0.5.0](https://github.com/burrow-cloud/burrow/tree/v0.5.0)) |
| **v0.6** | First backend building block, agent-native onboarding, and the Apache relicense: Postgres addon (`addon install`/`attach postgres`, a per-app database + role, generated `DATABASE_URL` wired into the app) · `pg_dump`/`pg_restore` backups behind a confirm guardrail · read-only `burrow_audit` MCP tool · agent-native onboarding (cluster-capability detection, cost-aware ingress/TLS provisioning with a LoadBalancer-vs-NodePort choice, a verified "live at https://…" check) · dotted guardrail codes (`app.delete`) · **whole repository relicensed to Apache-2.0** | ✅ shipped ([v0.6.0](https://github.com/burrow-cloud/burrow/tree/v0.6.0)) |
| **v0.7** | Environments and a self-contained, kubectl-free CLI: cluster-per-env via kubeconfig-context routing (`--context`, per-call agent routing) and namespace-per-env via a burrowd registry (`burrow env add`), with **per-environment guardrails** that gate prod while staging stays permissive (`burrow guard set --env prod app.delete deny`) · one `burrow env` surface over named local handles that follows the kube context by default (`use`/`follow`/`list`/`rename`/`scan`), resolving both CLI and agent operations through the active environment (retires `burrow context`) · intent-based `--help` groups, explicit `burrow install <context>` that names the environment, a first-run banner, shell completion, `system` folded into `cluster` · **`burrow` no longer needs `kubectl`** (client-go server-side apply) · the `app env`→`app config` rename | ✅ shipped ([v0.7.1](https://github.com/burrow-cloud/burrow/tree/v0.7.1)) |
| v1.0 | Production self-host: the deploy-and-operate core and common day-two building blocks, hardened | planned ([roadmap](docs/ROADMAP.md)) |

## Documentation

- [docs/getting-started.md](docs/getting-started.md) — set up Burrow on your cluster and connect
  your agent, start to finish.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the system design: the four layers, the
  invariants, and the request paths.
- [docs/HARDENING.md](docs/HARDENING.md): keep Burrow the agent's only path to
  the cluster, so the guardrails actually bound it.
- [docs/ROADMAP.md](docs/ROADMAP.md) — version milestones, v0.1 → v1.0.
- [docs/PLAN.md](docs/PLAN.md) — the current execution plan.
- [docs/adr/](docs/adr/README.md) — Architecture Decision Records: every load-bearing
  decision, with its reasoning and rejected alternatives.
- [CLAUDE.md](CLAUDE.md) — the contributor and agent guide.

## License and contributing

How the code is licensed ([LICENSING.md](LICENSING.md)):

- **All of Burrow's code in this repository is Apache-2.0**: the CLI, the MCP server, and the
  software that runs in your cluster. Read, modify, self-host, and integrate against it freely.
- Burrow is **open core**: the managed cloud and the enterprise tier (SSO, teams, compliance,
  support) are separate, proprietary products — see [COMMERCIAL.md](COMMERCIAL.md).

**Contributions are welcome** — open an issue or a discussion. Bug reports, ideas, and design
feedback are the best way to help and to shape where Burrow goes. Commits are signed off under
the Developer Certificate of Origin (`git commit -s`). See [CONTRIBUTING.md](CONTRIBUTING.md).
