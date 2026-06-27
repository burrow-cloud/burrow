# ADR-0026: Observability add-ons — query adapters over installed *or* existing backends

## Status

Accepted. Refines [ADR-0025](0025-building-block-addons.md) (the add-on model) for observability,
and reuses the adapter seam of [ADR-0018](0018-reaching-an-app-at-a-url.md) (DNS providers) and
the credential registry of [ADR-0023](0023-provider-credentials.md).

## Context

ADR-0025 framed add-ons as backing services Burrow installs. But the research that set the v0.4
direction is blunt: **most users already run a cluster, often already with logging and metrics
(Loki, Prometheus, Datadog, …), and their #1 Day-2 pain is troubleshooting what they already
have** — not greenfield deploy. Requiring them to install Burrow's stack to get value is too high
a bar.

Two facts make a "connect to what you already have" path clean:

- The **license bar only constrains what Burrow distributes.** *Connecting to* a user's existing
  Loki or Datadog is fine regardless of that tool's license — we query it, we don't ship it.
- **Capability is separable from provisioning.** "Query my logs" is the value; whether the log
  store is one Burrow installed or one the user already runs is an implementation detail behind a
  seam.

## Decision

Model observability as a **capability behind a query seam with adapters**, decoupled from how the
backend is provisioned.

- **The query seam is the value.** A logs (and later metrics) query interface; the agent queries
  through it via MCP tools, regardless of backend. Adapters: VictoriaLogs, Loki, Prometheus,
  Datadog, … The agent answering "how is my app doing? / why is it slow?" is identical across
  backends.
- **Two ways to register a backend, one `addon` group** ([ADR-0024](0024-cli-command-taxonomy.md)):
  - **`addon install <capability>`** — Burrow deploys a vetted, permissively-licensed default
    (logs → **VictoriaLogs**, Apache-2.0) **and registers it as a queryable capability in one
    step.** Install implies connect; the user does not run a second command.
  - **`addon connect <backend>`** — register an adapter to an *existing* backend the user already
    runs (endpoint + optional credential). The license bar does **not** apply — we connect, not
    distribute — so this works against AGPL (Loki, Grafana) and proprietary (Datadog) backends.
- **Capabilities are derived, not declared.** A single-capability backend implies its own (Loki →
  logs, Prometheus → metrics), so `addon connect loki --endpoint …` needs no capability flag. A
  multi-capability platform (Datadog, Grafana Cloud) is **probed with the provided key** to detect
  which capabilities are accessible. Each adapter implements capability detection — a constant for
  simple tools, an API probe for the big platforms.
- **A registry of registrations** (the provider-registry pattern, ADR-0023): each installed or
  connected add-on is recorded with its backend type, endpoint, capabilities, and a
  credential-Secret reference — credentials never cross MCP (ADR-0004/0023). `addon list` shows
  each entry, its **mode** (installed vs connected), and its **capabilities**, uniformly for both.
  Installed add-ons also own cluster resources; connected ones are registration-only.
- **VictoriaLogs install is the reference backend.** It is the always-available, testable instance
  Burrow develops and verifies the query seam against; without an installed backend there is
  nothing to exercise the query path — or a new connect-adapter — against. So `addon install logs`
  (store + a log collector so logs actually flow + the query adapter) is the first slice, and the
  test bed every later adapter is checked against.

**Surface.** `addon install <capability>`, `addon connect <backend>`, `addon list`,
`addon remove <name>` — install/remove guarded by `addon_install` / `addon_remove`
([ADR-0025](0025-building-block-addons.md)). Agent MCP: a query tool per capability (e.g.
`burrow_logs_query`) plus a read-only `burrow_addons` listing.

## Consequences

- Burrow's funnel widens from "deploy and operate on your own cluster" to "**point it at any
  cluster and let the agent troubleshoot your existing observability**," with install as the
  convenience for those who lack a backend — aimed squarely at the Day-2 pain the research found.
- The license bar governs only `install`; `connect` works against any backend.
- Reuses the adapter-seam and credential-registry patterns, so little new architecture; the agent
  value is identical across installed and connected backends.
- The first build is install-VictoriaLogs + the logs query seam (the testable core); connect
  adapters and metrics follow against that proven seam.

## Alternatives considered

- **Install-only (ADR-0025 as written).** Misses the larger existing-cluster, Day-2-troubleshooting
  market and forces a redeploy to get any value.
- **User-declared capabilities.** More boilerplate and error-prone; deriving (and probing for
  multi-capability platforms) is cleaner and harder to get wrong.
- **A separate `connect` top-level command.** Rejected — both modes end in "a capability the agent
  can query," so they belong in one `addon` group.
