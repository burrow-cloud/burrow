# Burrow Plan — current execution plan

> **This file is the front line only.** It holds what is being worked now and next, in
> priority order, and is pruned as work lands — no growing TODO graveyard. Coarse
> milestones live in [ROADMAP.md](ROADMAP.md); a completed item's record survives in git
> history, its now-green test, and the shipped ADR/doc.

## Shipped: v0.1 — the thin vertical slice ✅

An agent operates a real application on the user's own Kubernetes cluster, end to end,
safely — proven against the reference DigitalOcean cluster. `burrow install` lands the
control plane and an in-cluster Postgres; the CLI and MCP server reach it through the
Kubernetes API-server proxy using the developer's kubeconfig
([ADR-0014](adr/0014-self-host-connectivity-via-kubeconfig.md),
[ADR-0015](adr/0015-token-header-only-x-burrow-token.md)); an agent connected over MCP can
`deploy` by image reference, then `status`, `logs`, `rollback`, and `scale`, every mutating
call passing through the control-plane guardrails
([ADR-0006](adr/0006-guardrails-in-the-control-plane.md)). `burrow upgrade` rolls the
control plane forward in place ([ADR-0016](adr/0016-cli-distribution-and-upgrade-lifecycle.md)).
The detail lives in git history, the now-green tests (unit + k3d integration + the capstone
e2e), and the ADRs.

## Shipped since v0.1

- **Private registry auth** — `burrow config registry login/logout/list` provisions a
  `dockerconfigjson` pull Secret with the developer's kubeconfig and attaches it to the app
  namespace's default ServiceAccount; the credential never crosses MCP
  ([ADR-0017](adr/0017-private-registry-authentication.md)). It also made the
  **setup-vs-operation boundary** explicit: `install`/`upgrade`/`registry` act with the
  kubeconfig; `deploy`/`status`/`logs`/`rollback`/`scale` go through burrowd.
- **CLI on Cobra** — the command surface moved to Cobra
  ([ADR-0019](adr/0019-cli-framework-cobra.md)), so the v0.2 commands are built on it.

## Shipped since v0.1 (continued)

- **Guardrails as configurable policy** — the compiled-in, deny-or-allow guardrails are now
  `allow | confirm | deny` policy stored in the control plane and read live by burrowd
  ([ADR-0020](adr/0020-guardrails-as-configurable-policy.md)). `burrow guard list` is
  read-only (and an MCP tool); `burrow guard set` is CLI-only — the agent cannot change its
  own guardrails. The DNS and exposure gates plug in as policy rather than new hardcodes.
  Operators must keep the control plane the agent's only cluster path for the guardrails to
  bind ([ADR-0021](adr/0021-guardrails-require-control-plane-only-agent-access.md),
  [docs/HARDENING.md](HARDENING.md)).

## Shipped: v0.2 — reach a deployed app at a URL (ingress, TLS, DNS) ✅

Released as **v0.2.0**. An agent can make a deployed app reachable at a real hostname over
HTTPS on the user's own cluster — the missing half of "deploy and operate." Reachability is a
chain (controller → Service/Ingress → TLS → DNS), built to be **introspectable** so the agent
can reason about which link is broken and act on the gaps it owns. The full design — including
the human-setup vs. agent-operation split — is **[ADR-0018](adr/0018-reaching-an-app-at-a-url.md)
(Accepted)**.

## v0.4 — agent-provisioned building blocks 🚧 in progress

The differentiator: the agent stands up and operates a whole stack on the user's cluster, not
just an app — **or connects to one the user already runs**. The model is a curated catalog plus
a **DB-backed registry** of installed and connected instances
([ADR-0025](adr/0025-building-block-addons.md)), an install-or-connect query seam
([ADR-0026](adr/0026-observability-query-adapters.md)), and the agent as the query layer. The
license bar (Apache / MIT / BSD) governs only what Burrow *installs*; *connecting* to an existing
backend queries it without distributing it (so AGPL Loki / proprietary Datadog are fair to
connect). Research put **observability first, cache later, connect alongside install**.

**Shipped:**

- **Logs** — `addon install logs` (VictoriaLogs + a Fluent Bit collector) or `addon connect
  loki`; the agent queries either through `burrow_logs_query`.
- **Metrics (connect)** — `addon connect prometheus`, queried via PromQL
  (`burrow_metrics_query`); one adapter serves Prometheus and VictoriaMetrics.
- **Connected-backend auth** — a bearer token in `burrow-credentials`, read at query time; only
  the Secret key crosses the API, never the token.
- **`app deploy -- <cmd>`** — container command override on the CLI, at parity with the deploy
  API and the agent's MCP deploy tool.
- **e2e** — deterministic k3d gates for install-logs, connect-Loki, and connect-Prometheus, plus
  a local headless-agent diagnosis test held out of CI (it costs API tokens).

**Next:**

- **`addon install metrics`** — VictoriaMetrics + a vmagent scraper, so metrics flow without a
  pre-existing Prometheus (the install counterpart to the connect path).
- **Backend selector** — let `addon logs` / `addon metrics` target a specific backend when both
  an installed and a connected one serve the same capability.
- **`app delete`** (behind a delete guardrail); a **cache** (ValKey, BSD-3) is later and conditional.

**Deferred until requested:** server-side build from a git reference
([ADR-0008](adr/0008-two-build-paths.md)); smaller TLS/DNS follow-ons (a DNS-01 issuer, folding
the provider's record into reachability) ride along when a slice needs them.

Shipped in **v0.3**: the CLI regrouped by task (`app`/`config`/`system`, `expose`→`publish` —
[ADR-0024](adr/0024-cli-command-taxonomy.md)) with `app list`; the Cloudflare adapter verifying
account-scoped (`cfat_`) tokens by listing zones; the app Ingress bound to the ingress-nginx
class so it gets an address; reachability resolving via public DNS so a freshly added record is
seen (the chain converges for an agent); and a burrowd request log.

Shipped in **v0.2.1** (patch): quieter `install`/`upgrade` output with `--verbose`, helpful
CLI argument errors, ko-built images (no Dockerfile) with a warm CI build cache, a read-only
`burrow_providers` MCP tool, and `domain add/remove` auto-selecting the sole configured DNS
provider so `--provider` is optional.

## Testing posture (unchanged)

Burrow **differs from Hamster** — there is no global simulation harness
([ADR-0010](adr/0010-testing-strategy.md)): seam-isolated unit tests against fakes (k8s, the
registry, the clock, the database, and now the DNS provider behind injected interfaces);
targeted deterministic fault injection for the reconcile/deploy paths; and ephemeral-cluster
(k3d) integration plus the capstone e2e for the real adapters.

## Status of the blocking decisions

- **License: settled.** [ADR-0001](adr/0001-license-and-dco.md) is **Accepted** — Apache-2.0
  client surface, FSL-1.1-ALv2 control plane and operator, sole ownership with CLA-gated
  outside code.
