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

## Shipped: v0.4 — agent-provisioned building blocks ✅

Released as **v0.4.0**. The agent stands up and operates backing services on the user's cluster —
**or connects to ones they already run** — with the agent as the query layer. The model is a
curated catalog plus a **DB-backed registry** of installed and connected instances
([ADR-0025](adr/0025-building-block-addons.md)) and an install-or-connect query seam
([ADR-0026](adr/0026-observability-query-adapters.md)); the license bar (Apache / MIT / BSD)
governs only what Burrow *installs*, since *connecting* queries a backend without distributing it.
What shipped:

- **Logs** — `addon install logs` (VictoriaLogs + a Fluent Bit collector) or `addon connect loki`;
  queried through `burrow_logs_query`.
- **Metrics** — `addon install metrics` (VictoriaMetrics + a vmagent scraper) or `addon connect
  prometheus`, queried via PromQL (`burrow_metrics_query`); `app deploy --metrics-port` marks a
  pod for scraping. One adapter serves Prometheus and VictoriaMetrics.
- **Backend selector** — `addon logs` / `addon metrics` can target a specific backend when an
  installed and a connected one both serve a capability.
- **Connected-backend auth** — a bearer token in `burrow-credentials`, read at query time (its
  write transport moved through burrowd in v0.5 — [ADR-0030](adr/0030-credentials-through-the-control-plane.md)).
- **Cache** — `addon install cache` (ValKey, BSD-3), a backing service the agent wires an app to.
- **`app delete`** — remove an app, its routing, and release history behind a confirm guardrail;
  **`app deploy -- <cmd>`** — container command override at parity with the MCP deploy tool.
- **e2e** — deterministic k3d gates for install-logs, connect-Loki, connect-Prometheus,
  install-metrics + the full metrics loop, and cache; plus a local headless-agent diagnosis test
  and a blind-workspace **examples** library that exercise the full agent loop by hand.

## Shipped: v0.5 — app config, secrets, credentials, and the audit log ✅

Released as **v0.5.0**. Makes apps real to *run* and hardens how Burrow handles sensitive values —
the groundwork the web UI and managed product depend on.

- **App config & secrets** ([ADR-0028](adr/0028-app-config-and-secrets.md)) — an `app config` /
  `app secret` lifecycle store (`set`/`list`/`unset`, `--no-restart`), managed independently of
  deploy (`deploy` no longer takes config). Config renders inline as environment variables and
  auto-rolls; secrets live only in a per-app Secret, inject via `envFrom`, and `secret list` shows
  keys only.
- **Secrets & credentials through the control plane**
  ([ADR-0029](adr/0029-secrets-through-the-control-plane.md),
  [ADR-0030](adr/0030-credentials-through-the-control-plane.md)) — app secrets, vendor tokens, and
  connected-backend auth all flow over burrowd's **authenticated API**, written to a Secret by
  burrowd, **never over MCP**, never logged, never in Postgres. RBAC namespace- or name-scoped; no
  `ClusterRole`.
- **Audit log** ([ADR-0027](adr/0027-audit-log.md)) — an append-only Postgres record of guarded
  operations and their guardrail decision (allowed / held / denied / executed / failed), redacted
  to key names (never a value), read via `burrow audit [--app --operation --outcome --limit]`.
- **Dedicated app namespace** — new installs deploy apps into **`burrow-apps`**, not the shared
  `default` namespace, isolating the per-app secrets grant.

## Shipped: v0.6 — first backend block, agent-native onboarding, and the Apache relicense ✅

Released as **v0.6.0**. Opens the backend tier, makes first-touch onboarding agent-native, and
relicenses the whole repository to Apache-2.0.

- **Postgres add-on** ([ADR-0031](adr/0031-postgres-addon.md)) — `addon install postgres` (one shared
  instance in `burrow-addons`) + `addon attach postgres <app>` (a per-app database + login role, the
  generated `DATABASE_URL` written into the app's per-app Secret). Passwords generated server-side,
  so attach is agent-drivable with no secret value over MCP.
- **Postgres backups** ([ADR-0032](adr/0032-postgres-backups.md)) — `addon backup` / `backups` /
  `restore postgres` via `pg_dump`/`pg_restore` Jobs to a backup PVC, recorded in the control-plane
  database; restore is confirm-gated.
- **Read-only audit MCP tool** — `burrow_audit` over the guarded-operation log.
- **Agent-native onboarding** ([ADR-0034](adr/0034-agent-native-onboarding.md)) — cluster-capability
  detection (live, over one narrow read-only grant), cost-aware ingress/TLS provisioning
  (LoadBalancer vs NodePort), and a converged "live at https://…" reachability verdict with a wait.
  All agent-driven; no new command.
- **Dotted guardrail codes** — `resource.operation` form (`app.delete`), forward-compatible with
  per-environment scoping.
- **Apache-2.0 relicense** ([ADR-0033](adr/0033-relicense-to-apache.md)) — the whole repository is now
  Apache-2.0; managed cloud and the enterprise tier stay separate proprietary products.
- **Homebrew distribution** ([ADR-0016](adr/0016-cli-distribution-and-upgrade-lifecycle.md)) — the
  `burrow` and `burrow-mcp` CLIs publish to a Homebrew tap on release.

## Shipped: v0.7 — environments and a self-contained, kubectl-free CLI ✅

Released as **v0.7.0**. One Burrow operates many environments safely, and `burrow` is now a single
self-contained binary. The CLI and the agent both resolve every operation through an active
environment, with prod gated while staging stays permissive.

- **Environments** ([ADR-0035](adr/0035-environments.md)): **cluster-per-env** via kubeconfig-context
  routing (`--context`, plus per-call routing so one MCP server spans contexts) and **namespace-per-env**
  via a burrowd registry (`burrow env add`), each with its own RBAC. **Per-environment guardrails**
  (`burrow guard set --env prod app.delete deny`) gate prod while staging and the rest inherit the
  global policy.
- **Environment selection** ([ADR-0036](adr/0036-environment-selection.md)): one `burrow env` surface
  over named local handles in `~/.burrow/config` that **follows the kube context by default**
  (`use`/`follow`/`list`/`rename`/`scan`); retires `burrow context`.
- **CLI onboarding and organization** ([ADR-0037](adr/0037-cli-onboarding-and-organization.md)):
  intent-based `--help` groups, an explicit positional `burrow install <context>` that names and records
  the environment, a first-run banner, shell completion, and `system` folded into `cluster`. **`burrow`
  no longer needs `kubectl`** (client-go server-side apply).
- **Surface cleanups**: the `app env`→`app config` rename, a cleaner `burrow version`, and connection
  errors that name the targeted context.

## Shipped: v0.8 — autoscaling and deploy-safety hardening ✅

Released as **v0.8.0**. Application autoscaling, plus a batch of least-privilege and deploy-safety
hardening.

- **Autoscaling** — `burrow app autoscale <app>` applies an autoscaling/v2 HorizontalPodAutoscaler
  (1..10 replicas at 80% CPU by default), its max bounded by the replica-ceiling guardrail;
  `app autoscale <app> off` removes it, and an `app.autoscale` guardrail gates it. Warns when
  metrics-server is absent.
- **Scoped agent credential** ([ADR-0038](adr/0038-scoped-agent-credential.md)) — install mints a
  `burrow-agent` ServiceAccount with narrow RBAC and writes a burrowd-only kubeconfig; the human keeps
  the admin kubeconfig, and `burrow-mcp` fails closed if the scoped credential is missing.
- **Deploy safety** — an `app.deploy` guardrail (gate or require sign-off per environment); every
  deploy rolls the workload (release-stamped, so a re-deploy or a pull-credential fix always takes
  effect) while preserving the running replica count.
- **Version skew** ([ADR-0039](adr/0039-cli-control-plane-version-skew.md)) — a client-version header
  turns a new CLI against an old control plane into an actionable "run `burrow upgrade`".
- **Burrowd never contacts the registry** ([ADR-0040](adr/0040-burrowd-never-contacts-the-registry.md))
  — the pre-deploy image resolve is gone; Kubernetes resolves and pulls via the imagePullSecret and the
  digest is read back from pod status.
- **Registry / credentials UX** — a secure token prompt for private-registry credentials, agent guidance
  toward durable credentials and versioned tags, and actionable errors for an unknown environment or a
  failed pull.
- **Ingress cost-approval** — `--approve` before Burrow stands up a billable LoadBalancer, honest
  capability detection, clean adoption of an orphaned IngressClass, and public exposure steered toward a
  LoadBalancer over NodePort.
- **Add-on RBAC staged per-add-on** by the CLI at install time (least privilege): the base install no
  longer carries the metrics vmagent grant; `addon install metrics` applies it kubeconfig-side and
  burrowd verifies it read-only, failing cleanly on the agent path if absent.

## Shipped: v0.9 — the single-VPS, cheap-self-hoster on-ramp ✅

Released as **v0.9.0**. Turns a bare VPS into a Burrow cluster with no cloud LoadBalancer cost, so a
solo developer can self-host the whole thing on one cheap box. Proven end to end by dogfooding: a public
app served over the node's own IP through servicelb and the ingress on a 2GB droplet.

- **Single-VPS bootstrap** ([ADR-0044](adr/0044-single-vps-k3s-cluster.md)) — a one-time on-VPS
  `curl | sh` runs `burrow cluster bootstrap`, which installs k3s + burrowd and prints a
  `burrow join <token>`; running join on the laptop lands both admin and scoped credentials, so after
  the single SSH bootstrap every operation runs from the laptop. Burrow never SSHes.
- **Free LoadBalancer detection** ([ADR-0043](adr/0043-public-reachability-is-a-loadbalancer.md)) —
  servicelb and MetalLB are detected as real LoadBalancer providers, so a single node's public IP serves
  a `type=LoadBalancer` Service at no cloud cost; public reachability is a LoadBalancer, not NodePort.
- **Existing ingress controller** ([ADR-0042](adr/0042-use-existing-ingress-controller.md)) and a flatter
  path to a reachable app ([ADR-0041](adr/0041-flatten-path-to-a-reachable-app.md)).
- **Bootstrap safety** — a 2GB RAM preflight with a memory breakdown that steers away from undersized
  boxes, a wait for the k3s API instead of trusting the installer exit, and a confirm before turning a
  machine into a cluster node.
- **Small-cluster tuning** — lean Postgres config and memory limits, and bounded database-wait attempts
  so burrowd startup retries fast.
- **Honest surfaces** — `ingress install` frames servicelb / MetalLB LoadBalancers as free (not
  billable); `app logs` prints the source note and context above the logs; `env scan` folds into
  `env list --discover`.

**Next: the v0.10 theme is not yet chosen.** v0.9 proved the cheap-self-hoster on-ramp; the following
milestone is open. Live candidates:

- **Self-hoster day-2 hardening** — scheduled Postgres backups + retention (the
  [ADR-0032](adr/0032-postgres-backups.md) follow-on, a CronJob or a burrowd in-process scheduler),
  richer guardrails / blast-radius limits, and cost visibility.
- **Managed-product groundwork** — the one refactor ADR-0045 names (make the CLI's control-plane
  transport an explicit interface before it accretes more coupled call sites), plus richer per-principal
  identity with an auth ADR.
- **Database-provisioning depth** — managed Postgres as a first-class deploy dependency, heavier than the
  current attach model.

**Deferred until requested:** registry onboarding (ADR-0046, Proposed, held pending a user signal that
onboarding is painful); server-side build from a git reference
([ADR-0008](adr/0008-two-build-paths.md)).

Shipped in **v0.7.1** (patch): a `burrow mcp <tool> [install]` command that connects Burrow's MCP
server to Claude Code, Cursor, Codex, Copilot, or OpenCode (preview by default, idempotent, and it
backs up any file it edits), with a generic fallback for any other agent and a new
[getting-started guide](getting-started.md); a kubectl-style `--help` layout (usage at the bottom,
examples, a first-run `burrow` vs `burrow -h` split); a one-command `addon install` that stages an
add-on's RBAC client-side then installs through the API (the metrics vmagent grant moved out of the
base install for least privilege); a TTY-aware install progress indicator; and a consistent
`burrow env` listing plus an install context list with an installed-status column.

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

- **License: settled.** The whole repository is **Apache-2.0**
  ([ADR-0033](adr/0033-relicense-to-apache.md), superseding ADR-0001's split); sole copyright
  ownership with CLA-gated outside code. The managed cloud and enterprise tier are a separate
  product that does not live in this repository.
