# Burrow Roadmap

> **Status: v0.1–v0.3 shipped; v0.4 next.** These are version milestones; each unshipped one is
> a goal until it ships ([ADR-0009](adr/0009-honest-status.md)). The
> [README](../README.md) status table is the authoritative shipped/in-progress/planned
> surface. This file holds the coarse milestones; [PLAN.md](PLAN.md) holds the current
> execution detail.

Burrow follows semver from v0.1 toward v1.0. The theme of the 0.x series is **compute
first**: deploy someone's code and run it well, safely, agent-driven. Databases, domains,
autoscaling, and cost controls come after the deploy-and-operate core is solid.

## v0.1 — Deploy and operate (the vertical slice) ✅ shipped

The thin end-to-end slice that proves the architecture, shipped and validated on the
reference DigitalOcean cluster. Install Burrow into an existing cluster, point an agent at
it over MCP, and deploy and operate a real application by image reference. The record lives
in git history, the now-green tests, and the ADRs.

- Install the control plane and MCP server into an existing Kubernetes cluster.
- Connect any MCP agent to the MCP server.
- `deploy` an image by reference ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)),
  with client-side build-and-push ([ADR-0008](adr/0008-two-build-paths.md)).
- `status`, `logs`, `rollback`, `scale` — each guarded and returning structured results
  ([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)).
- Integration tests running the real deploy/rollback/logs/scale paths against an ephemeral
  local cluster (kind or k3d).

## v0.2 — Reach an app at a URL ✅ shipped

Make a deployed app reachable at a public hostname over HTTPS: shared-ingress routing,
`publish` + cert-manager TLS, a `reachability` surface, DNS automation (DigitalOcean /
Cloudflare) with `domain add/remove`, and `ingress install` setup
([ADR-0018](adr/0018-reaching-an-app-at-a-url.md)).

## v0.3 — Operability + agent-experience hardening ✅ shipped

Tighten the v0.2 surface for real agent-driven use: the CLI regrouped by task
(`app`/`config`/`system`, `expose`→`publish` — [ADR-0024](adr/0024-cli-command-taxonomy.md))
with `app list`; account-scoped Cloudflare tokens; the app Ingress bound to its controller;
reachability resolving via public DNS so the chain converges for an agent; and a burrowd
request log. A breaking CLI change, taken while the surface is small.

## v0.4 — Agent-provisioned building blocks 🚧 next

The differentiator: an agent that stands up and operates a whole stack on the user's own
cluster, not just an app. The user asks a question or for a capability; the agent does the
app-side work; **Burrow provisions a vetted, self-hostable backing service with sane defaults
and operates it behind the guardrails** — or **connects to one the user already runs**. The
add-on model — a curated catalog plus a **DB-backed registry** of installed and connected
instances, with the agent as the query layer for observability — is
[ADR-0025](adr/0025-building-block-addons.md); the install-or-connect query seam is
[ADR-0026](adr/0026-observability-query-adapters.md). The license bar (**Apache / MIT / BSD**)
governs only what Burrow *installs* — **connecting** to an existing backend queries it without
distributing it, so AGPL Loki and proprietary Datadog are fair game to connect. Research into
what small operators actually struggle with (Day-2 ops, "how is my app doing?", a hard no on
autonomous prod changes — and that most already run a cluster *with* logging) puts
**observability first, cache later, and connect alongside install**. Slices:

- **Logs** ✅ shipped — install [VictoriaLogs](https://docs.victoriametrics.com/victorialogs/)
  (Apache-2.0) with a Fluent Bit collector, or `connect` an existing Loki; the agent queries
  either through `burrow_logs_query` to answer "what happened / what changed before it broke".
- **Metrics** — `connect` an existing Prometheus / VictoriaMetrics and query it via PromQL
  (`burrow_metrics_query`) ✅ shipped; **`addon install metrics`** (VictoriaMetrics + a vmagent
  scraper, so metrics flow without a pre-existing Prometheus) is the next slice.
- **Connected-backend auth** ✅ shipped — a bearer token in the `burrow-credentials` Secret,
  read at query time; only the Secret key crosses the API, never the token.
- **Observability answers, not dashboards** ✅ — no bundled Grafana (AGPL); the agent is the
  query interface over the logs + metrics it set up or connected.
- **Cache** — [ValKey](https://valkey.io) (BSD-3): later and conditional — a backing service only
  some apps need, orthogonal to the observability story.
- **`app delete`** — remove an app and its routing, behind a delete guardrail.

Each shipped slice has a deterministic k3d e2e (install-logs, connect-Loki, connect-Prometheus);
a local headless-agent diagnosis test is held out of CI (it costs API tokens).

## Deferred until requested

- **Server-side build from a git reference** ([ADR-0008](adr/0008-two-build-paths.md)) — a
  real second build path, but parked until a user actually needs it; client-side build plus
  deploy-by-image-reference covers the common case today.

## Later — candidate themes (unsequenced)

- **Richer guardrails / blast-radius limits** for destructive operations.
- **Database provisioning** — managed Postgres (and friends) as a first-class deploy
  dependency (a heavier building block than the cache/metrics blocks above).
- **Autoscaling** — horizontal/vertical scaling driven by load.
- **Cost controls and caps** — visibility and limits on cluster spend.
- **Optional passive deploy mode** — GitOps-style tag-watching as an *option* layered on the
  explicit path, never replacing it ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)).
- **Self-host dashboard** — an HTMX dashboard over the control-plane API, if and when a
  self-host UI is warranted.

## v1.0 — Production self-host

A self-host Burrow a solo developer or small agency can run their real infrastructure on:
the deploy-and-operate core hardened, the guardrails mature, the common day-two operations
(databases, domains, scaling) covered, and the upgrade and operational story documented and
tested. The multi-tenant managed cloud built on top of this core remains a separate,
private product.
