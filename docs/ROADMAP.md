# Burrow Roadmap

> **Status: v0.1 through v0.12 shipped.** These are version milestones; each unshipped one is
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

## v0.4 — Agent-provisioned building blocks ✅ shipped

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
- **Metrics** ✅ shipped — `addon install metrics` (VictoriaMetrics + a vmagent scraper, so
  metrics flow without a pre-existing Prometheus) or `connect` an existing Prometheus /
  VictoriaMetrics, queried via PromQL (`burrow_metrics_query`); `app deploy --metrics-port` marks
  a pod for scraping.
- **Backend selector** ✅ shipped — `addon logs` / `addon metrics` can target a specific backend
  when an installed and a connected one both serve a capability.
- **Connected-backend auth** ✅ shipped — a bearer token in the `burrow-credentials` Secret,
  read at query time; only the Secret key crosses the API, never the token.
- **Observability answers, not dashboards** ✅ — no bundled Grafana (AGPL); the agent is the
  query interface over the logs + metrics it set up or connected.
- **Cache** ✅ shipped — `addon install cache` ([ValKey](https://valkey.io), BSD-3), a backing
  service the agent wires an app to (no query seam — apps connect to it directly).
- **`app delete`** ✅ shipped — remove an app, its routing, and release history behind a confirm
  guardrail.

Each shipped slice has a deterministic k3d e2e (install-logs, connect-Loki, connect-Prometheus,
install-metrics + the full metrics loop, cache); a local headless-agent diagnosis test and a
blind-workspace examples library exercise the full agent loop by hand, held out of CI (they cost
API tokens).

## v0.5 — App config, secrets, credentials, and the audit log ✅ shipped

The release that makes apps real to *run* and hardens how Burrow handles sensitive values — the
groundwork the web UI and managed product depend on.

- **App config & secrets** ([ADR-0028](adr/0028-app-config-and-secrets.md)) — an `app config` /
  `app secret` lifecycle store (`set`/`list`/`unset`, `--no-restart`), managed independently of
  deploy (`deploy` no longer takes config). Config renders inline and auto-rolls; secrets live only in a
  per-app Kubernetes Secret and inject via `envFrom`. `secret list` shows keys only.
- **Secrets & credentials through the control plane**
  ([ADR-0029](adr/0029-secrets-through-the-control-plane.md),
  [ADR-0030](adr/0030-credentials-through-the-control-plane.md)) — app secrets, vendor tokens, and
  connected-backend auth all flow over burrowd's **authenticated API**, written to a Secret by
  burrowd, **never over MCP** (the agent references keys; the human/UI sets values) and never
  logged or stored in the database. RBAC stays namespace- or name-scoped; no `ClusterRole`.
- **Audit log** ([ADR-0027](adr/0027-audit-log.md)) — an append-only Postgres record of every
  guarded operation and its guardrail decision (allowed / held / denied / executed), read with
  `burrow audit`. Args are redacted to key names; no secret value is ever recorded.
- **Dedicated app namespace** — new installs deploy apps into **`burrow-apps`**, not the cluster's
  shared `default` namespace, so the per-app secrets grant stays isolated.

## v0.6 — First backend block, agent-native onboarding, and the Apache relicense ✅ shipped

The release that opens the backend tier, makes first-touch onboarding agent-native, and removes the
source-available friction from the license.

- **Postgres add-on** ([ADR-0031](adr/0031-postgres-addon.md)) — `addon install postgres` stands up
  one shared instance in `burrow-addons`; `addon attach postgres <app>` gives each app its own
  database and login role and writes the generated `DATABASE_URL` into the app's per-app Secret.
  burrowd generates the passwords server-side, so attach is agent-drivable yet no secret value
  crosses MCP. BYO Neon/Supabase and a provisioned database reach the app the same way.
- **Postgres backups** ([ADR-0032](adr/0032-postgres-backups.md)) — `addon backup` / `backups` /
  `restore postgres` run `pg_dump`/`pg_restore` as Jobs to a backup PVC, recorded in the control-plane
  database; restore is confirm-gated. Scheduled backups and retention are a later slice.
- **Read-only audit MCP tool** — `burrow_audit` lets the agent query the guarded-operation log
  (allowed / held / denied / executed) with the same key-only redaction as `burrow audit`.
- **Agent-native onboarding** ([ADR-0034](adr/0034-agent-native-onboarding.md)) — `burrow install`
  detects the cluster's capabilities and burrowd reads them live over one narrow read-only grant;
  `cluster ingress install` provisions the substrate (ingress-nginx, cert-manager, an issuer) on a
  cost-aware confirmation with a LoadBalancer-vs-NodePort choice; and `reachability` converges to a
  verified "live at https://…" URL. All agent-driven, no new command.
- **Dotted guardrail codes** — guardrail codes moved to a hierarchical `resource.operation` form
  (`app.delete`, `dns.write`, `addon.install`), forward-compatible with per-environment scoping.
- **Apache-2.0 relicense** ([ADR-0033](adr/0033-relicense-to-apache.md)) — the whole repository is now
  Apache-2.0; the managed cloud and the enterprise tier remain separate proprietary products.
- **Homebrew distribution** ([ADR-0016](adr/0016-cli-distribution-and-upgrade-lifecycle.md)) — the
  `burrow` and `burrow-mcp` CLIs publish to a Homebrew tap on each release.

## v0.7 — Environments and a self-contained, kubectl-free CLI ✅ shipped

The release that makes one Burrow operate many environments safely, and turns `burrow` into a
single self-contained binary. The same agent and CLI now drive dev, staging, and prod through an
active environment, with prod gated while staging stays permissive.

- **Environments** ([ADR-0035](adr/0035-environments.md)): two shapes of environment under one
  model. **Cluster-per-env** via kubeconfig-context routing (`--context`, with per-call routing for
  the agent so one MCP server spans contexts) and **namespace-per-env** via a burrowd registry
  (`burrow env add`), each carrying its own RBAC. **Per-environment guardrails** are the answer to
  "don't let AI touch prod": `burrow guard set --env prod app.delete deny` locks prod while staging
  and the rest inherit the permissive global policy.
- **Environment selection** ([ADR-0036](adr/0036-environment-selection.md)): one `burrow env`
  surface over named local handles in `~/.burrow/config` that **follows the kube context by
  default**, with `use`, `follow`, `list`, `rename`, and `scan`. The CLI and the agent both resolve
  every operation through the active environment. Retires `burrow context`.
- **CLI onboarding and organization** ([ADR-0037](adr/0037-cli-onboarding-and-organization.md)):
  intent-based `--help` groups, an explicit positional `burrow install <context>` that names and
  records the environment, a first-run banner, shell completion, and `system` folded into `cluster`.
  **`burrow` no longer needs `kubectl`**: manifests apply through client-go server-side apply, so the
  binary is self-contained.
- **Surface cleanups**: the `app env`→`app config` rename, plus a cleaner `burrow version` and
  connection errors that name the targeted context.

## v0.8 — Autoscaling and deploy-safety hardening ✅ shipped

Application autoscaling plus a batch of least-privilege and deploy-safety hardening. `burrow app
autoscale` applies a HorizontalPodAutoscaler bounded by the replica-ceiling guardrail; install mints a
scoped `burrow-agent` credential ([ADR-0038](adr/0038-scoped-agent-credential.md)) and `burrow-mcp`
fails closed without it; an `app.deploy` guardrail gates deploys and every deploy rolls the workload
while preserving replicas; burrowd no longer contacts the registry
([ADR-0040](adr/0040-burrowd-never-contacts-the-registry.md)); and `ingress install` gates a billable
LoadBalancer behind `--approve`.

## v0.9 — The single-VPS, cheap-self-hoster on-ramp ✅ shipped

Turns a bare VPS into a Burrow cluster with no cloud LoadBalancer cost, so a solo developer can
self-host the whole thing on one cheap box. A one-time on-VPS `curl | sh` runs `burrow cluster
bootstrap` (installs k3s + burrowd, prints a `burrow join <token>`), and after that single SSH bootstrap
every operation runs from the laptop ([ADR-0044](adr/0044-single-vps-k3s-cluster.md)); Burrow never
SSHes. servicelb and MetalLB are detected as real LoadBalancer providers, so a single node's public IP
serves a `type=LoadBalancer` Service for free
([ADR-0043](adr/0043-public-reachability-is-a-loadbalancer.md)); publish reuses the cluster's existing
ingress controller ([ADR-0042](adr/0042-use-existing-ingress-controller.md)) on a flatter path to a
reachable app ([ADR-0041](adr/0041-flatten-path-to-a-reachable-app.md)). Bootstrap preflights RAM (a 2GB
minimum with a memory breakdown), Postgres runs lean on small clusters, and the cost framing calls
servicelb free. Proven end to end by dogfooding on a 2GB droplet.

## v0.10 — Internal: version-skew handshake and the OSS/enterprise transport seam ✅ shipped

Internal groundwork with no new user-facing surface. A **client-version handshake**
([ADR-0039](adr/0039-cli-control-plane-version-skew.md)): every client sends its version, burrowd
serves any client within one minor and never hard-blocks on a version difference alone, and it turns
genuine skew into an actionable error — a client too old is told to `brew upgrade`, and a newer client
calling a route this control plane lacks is told to `burrow upgrade` — instead of an opaque failure;
the acting client version is recorded in the audit log next to the principal. And the CLI's
control-plane transport is extracted into an explicit interface shared by both binaries
([ADR-0045](adr/0045-oss-enterprise-boundary.md)), so an alternate transport (for a private managed
layer) slots in without forking the request methods.

## v0.11 — Agent environment safety ✅ shipped

Make the agent's environment target **explicit and sticky** so a mutating operation never lands on —
or wanders to — the wrong cluster ([ADR-0047](adr/0047-agent-environment-safety.md)). A deploy /
scale / rollback / delete that names no environment is refused with a structured, alternatives-listing
error whenever more than one is registered — at both the MCP handle layer (cluster-per-env) and
burrowd's own registry (namespace-per-env), judged by registration not reachability — while a single
environment proceeds without ceremony. An unreachable target names the other registered environments
so a human can redirect, but Burrow never switches, retries elsewhere, or auto-fails-over. Read-only
tools echo the environment they read. And `burrow env remove` finally lets a user drop a stale local
handle (clearing the pin and its scoped credential), closing the ADR-0036 gap.

## v0.12 — The scoped agent CLI and one-off commands ✅ shipped

The agent's control channel is now **`burrow-agent`**, a scoped, capability-reduced, JSON-first CLI
the agent invokes directly ([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md)),
superseding the MCP server: the agent composes its `--json` output with pipes, `grep`, and `jq`, and
its dangerous admin verbs are absent by construction. `burrow agent <tool> install` wires an agent to
it (allow `burrow-agent`, deny the human `burrow` CLI), replacing `burrow mcp <tool> install`, and the
`burrow-mcp` server is retired from releases (its code stays in-tree for now, ADR-0049 §7). Alongside
it, **`burrow_run`** ([ADR-0048](adr/0048-one-off-command-runner.md)) runs a one-off command
(migrations, seeds, tasks) in the app's own image, gated by an `app.run` guardrail; and a **validated
laptop quickstart** on k3d, pinned by a CI e2e, takes a user from nothing to their agent deploying an
app — and hitting the delete guardrail — on their own machine.

## Deferred until requested

- **Server-side build from a git reference** ([ADR-0008](adr/0008-two-build-paths.md)) — a
  real second build path, but parked until a user actually needs it; client-side build plus
  deploy-by-image-reference covers the common case today.

## Later — candidate themes (unsequenced)

- **Richer guardrails / blast-radius limits** for destructive operations.
- **Database provisioning** — managed Postgres (and friends) as a first-class deploy
  dependency (a heavier building block than the cache/metrics blocks above).
- **Vertical autoscaling / right-sizing** — driven by observed load (horizontal autoscaling
  via the HorizontalPodAutoscaler shipped in v0.8).
- **Registry onboarding** — reduce the friction of getting an image into a registry, per
  ADR-0046 (Proposed); held pending a user signal that onboarding is painful.
- **Cost controls and caps** — visibility and limits on cluster spend.
- **Optional passive deploy mode** — GitOps-style tag-watching as an *option* layered on the
  explicit path, never replacing it ([ADR-0007](adr/0007-explicit-deploy-by-image-reference.md)).
- **Self-host dashboard** — an HTMX dashboard over the control-plane API, if and when a
  self-host UI is warranted.
- **App-runtime API and capability envelopes** *(exploratory)* — a programmatic control-plane
  API a deployed app can call at runtime to provision infrastructure for its own users, bounded
  by an operator-granted capability envelope, per [ADR-0050](adr/0050-app-runtime-api-and-capability-envelopes.md)
  (Proposed); a captured direction, not a committed milestone, deferred behind the compute-first
  core.

## v1.0 — Production self-host

A self-host Burrow a solo developer or small agency can run their real infrastructure on:
the deploy-and-operate core hardened, the guardrails mature, the common day-two operations
(databases, domains, scaling) covered, and the upgrade and operational story documented and
tested. The multi-tenant managed cloud built on top of this core remains a separate,
private product.
