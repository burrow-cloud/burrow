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

## Shipped: v0.10 — internal: the version-skew handshake and the transport seam ✅

Released as **v0.10.0**. Internal groundwork with no new user-facing surface — it hardens how the CLI,
the MCP server, and burrowd get along across versions, and prepares the OSS/enterprise boundary.

- **Version-skew handshake** ([ADR-0039](adr/0039-cli-control-plane-version-skew.md)) — every client
  sends its release version in `X-Burrow-Client-Version`; burrowd is the compatibility anchor: it serves
  any client within one minor and never hard-blocks on a version difference alone, but turns genuine skew
  into an actionable error. A client too old is told to `brew upgrade burrow`; a newer client calling a
  route this control plane lacks is told to ask an operator to run `burrow upgrade`, instead of an opaque
  404. The acting client version is recorded in the audit log next to the principal (migration 00012).
- **Control-plane transport seam** ([ADR-0045](adr/0045-oss-enterprise-boundary.md)) — the CLI's
  control-plane transport/auth is an explicit `client.Transport` interface (a direct-URL transport and
  the kubeconfig API-server proxy), importable by both `burrow` and `burrow-mcp`; the `Client`'s request
  methods are auth-agnostic, so a private managed layer can add an SSO transport without forking them.
  One of the three seams ADR-0045 names for keeping the managed product a thin layer over the OSS core.

## Shipped: v0.11 — agent environment safety ✅

Released as **v0.11.0**. The agent's environment target is now **explicit and sticky** so a mutating
operation never lands on — or wanders to — the wrong cluster. Designed in
[ADR-0047](adr/0047-agent-environment-safety.md); the v0.10 MCP-instructions hardening was the
guidance half, and this milestone added the code-level forcing function across four phases, plus a
command to drop a stale environment:

- **Phase 1 — refuse an implicit mutating target in the MCP selector (cluster-per-env).** A mutating
  tool called with no `env`/`context` is refused with a structured error listing the registered local
  handles when more than one is registered; a single handle proceeds without ceremony. No silent
  fall-back to the ambient context (ADR-0047 §1–2). This is the axis the incident lived on.
- **Phase 2 — the same guard in burrowd (namespace-per-env).** A mutating request with no environment
  is refused when burrowd's registry holds more than one environment (the implicit `default` plus any
  named one), rather than defaulting to `default`; the sole-`default` case proceeds unchanged
  (ADR-0047 §1–2).
- **Phase 3 — unreachable errors name the alternatives.** When a target's control plane is unreachable
  or a call errors, the result names the other registered environments (and, where cheap, their
  reachability) so the human can redirect — without the system ever switching or retrying elsewhere
  (ADR-0047 §4).
- **Phase 4 — reconcile the default and the echo.** Scope the ambient-context default to the
  read-only survey path and the single-environment case so the selector's contract holds in code, and
  ensure every tool echoes the environment it acted in (ADR-0047 §3, §5).
- **`burrow env remove`** — drop a stale local environment handle (ADR-0036 gap), clearing the pin if
  it was current and deleting the handle's orphaned scoped credential under `~/.burrow/`. Local-only:
  it does not tear down a namespace-per-env registration or any cluster namespace/RBAC.

## Shipped: v0.12 — the scoped agent CLI and one-off commands ✅

Released as **v0.12.0**. The agent's control channel becomes a dedicated, capability-reduced CLI, and
one-off commands get a home.

- **`burrow-agent`, the scoped agent CLI** ([ADR-0049](adr/0049-burrow-agent-scoped-cli-control-channel.md))
  — the agent now drives a dedicated, JSON-first, composable binary (pipe / `grep` / `jq`) behind the same
  control-plane boundary, with its dangerous admin verbs absent by construction. `burrow agent <tool> install`
  wires an agent to it (allow `burrow-agent`, deny the human `burrow`), replacing `burrow mcp <tool> install`;
  the `burrow-mcp` server is **retired from releases**, its code kept in-tree for now (ADR-0049 §7).
- **One-off command runner** ([ADR-0048](adr/0048-one-off-command-runner.md)) — `burrow_run` runs a one-off
  command (migrations, seeds, tasks) in the app's own image, gated by an `app.run` guardrail.
- **Validated laptop quickstart** — a k3d quickstart pinned by a CI e2e takes a user from nothing to their
  agent deploying an app (and hitting the delete guardrail) on their own machine.
- **App-runtime direction recorded** ([ADR-0050](adr/0050-app-runtime-api-and-capability-envelopes.md),
  Proposed) — a captured direction for a programmatic runtime API bounded by capability envelopes, deferred
  behind the compute-first core; no code.

## v0.13 — the optional in-cluster build (release being cut)

Everything below landed on `main` since v0.12.0 and ships as **v0.13** — honest status, unreleased until the
tag ([ADR-0009](adr/0009-honest-status.md)). The full record lives in [ROADMAP.md](ROADMAP.md), the
[README](../README.md) status table, and the shipped ADRs; the headline:

- **In-cluster build from a git source** ([ADR-0053](adr/0053-in-cluster-build-from-source.md)) — an optional
  path off the explicit deploy spine: `burrow app build <app> --source <git-ref>` (and a `burrow-agent build`
  verb) clone a git ref and build the image **inside the user's own cluster** as a Kubernetes Job (buildah for a
  Dockerfile, Cloud Native Buildpacks otherwise), push it to a registry, and the built reference rejoins the
  guarded deploy path. Code never crosses the control channel — only the git ref does. Validated end to end on
  managed Kubernetes, where the OSS build container runs **privileged**
  ([ADR-0059](adr/0059-oss-build-container-runs-privileged.md), superseding
  [ADR-0056](adr/0056-build-security-context-for-the-oss-builder.md)) in a dedicated `burrow-builds` namespace
  with capacity fail-fast (#274) and TTL-reaped Jobs (#280). **Source-provider credentials**
  ([ADR-0057](adr/0057-source-provider-credentials.md)) let it clone a private repo and push to its registry with
  one control-plane-set token. Known limit: the no-Dockerfile Buildpacks path cannot yet push to the plain-HTTP
  in-cluster registry.
- **`install` provisions only the control plane** ([ADR-0054](adr/0054-install-is-control-plane-only.md)) — every
  additive cluster component is an opt-in `burrow cluster <component>` step: `burrow cluster registry` installs the
  in-cluster registry, `burrow cluster capacity` reports scheduling headroom (#275), and metrics-server is
  auto-ensured as a baseline. The `--with-registry` / `--with-ingress` install flags are removed.
- **Opt-in auto-deploy** ([ADR-0052](adr/0052-pull-based-passive-deploy.md),
  [ADR-0058](adr/0058-auto-deploy-is-opt-in.md)) — burrowd polls each app's image repository and auto-applies
  upgrades within a per-app, per-environment level (`burrow app auto-deploy <app> [patch|minor|major|off]`, off by
  default), firing the **same guarded deploy path**; a tag above the level surfaces as an available upgrade.
  Outbound-only. Also lands an app-history deploy timeline in both client binaries.
- **Multi-version forward upgrades** ([ADR-0055](adr/0055-multi-version-upgrades.md)) — the database may jump
  across any number of minors in one step; the startup gate still refuses downgrades and cross-major in-place moves.
- **Postgres always exports metrics** ([ADR-0051](adr/0051-postgres-always-exports-metrics.md)) — the Postgres
  add-on always ships its metrics exporter and the scraper discovers the add-on namespace, so `addon attach`
  observability needs no separate wiring.

## Next — the front line after v0.13

**Next — candidates for the theme after v0.13 ships** (unsequenced): **self-hoster day-2
hardening** (scheduled Postgres backups + retention — the [ADR-0032](adr/0032-postgres-backups.md) follow-on —
richer blast-radius guardrails, cost visibility); **more building-block add-ons**
([ADR-0025](adr/0025-building-block-addons.md)) beyond Postgres / cache / logs / metrics; **database-provisioning
depth** (managed Postgres as a first-class deploy dependency).

**Teed up but parked:** per-principal identity + an auth ADR — the natural continuation of the v0.10
transport seam and the ADR-0039 audit trail (which now records a real slot for the principal), but it
serves the managed/enterprise direction more than the current self-hoster, so it waits until that
product needs it.

**Deferred until requested:** registry onboarding (ADR-0046, Proposed, held pending a user signal that
onboarding is painful); managed server-side build ([ADR-0008](adr/0008-two-build-paths.md)'s second path —
the OSS in-cluster build from a git source now exists via ADR-0053, above).

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
