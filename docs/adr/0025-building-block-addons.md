# ADR-0025: Building-block add-ons — vetted, self-hostable backing services

## Status

Accepted. Builds on [ADR-0023](0023-provider-credentials.md) (the registry + credential-Secret
pattern), [ADR-0006](0006-guardrails-in-the-control-plane.md) /
[ADR-0020](0020-guardrails-as-configurable-policy.md) (guardrails),
[ADR-0004](0004-code-never-over-mcp.md) (code/credentials never over MCP), and
[ADR-0024](0024-cli-command-taxonomy.md) (CLI taxonomy).

## Context

Burrow's v0.4 direction is the differentiator: an agent that stands up and operates a whole
stack on the user's own cluster, not just an app. The user asks for a capability — "my site is
slow, add a cache"; "set up metrics" — the **agent writes the app-side integration code**, and
**Burrow deploys and operates the backing service**. This needs a model: what Burrow knows how
to deploy, how it deploys it, how the app gets connection details, how credentials stay off MCP,
and how each mutation is guarded.

The pieces already exist in spirit. The provider registry ([ADR-0023](0023-provider-credentials.md))
curates vendor credentials as a DB record plus a scoped Secret; `ingress install`
([ADR-0018](0018-reaching-an-app-at-a-url.md)) installs cluster infrastructure from pinned
upstream manifests; guardrails ([ADR-0006](0006-guardrails-in-the-control-plane.md)) gate every
mutation. Add-ons compose the same patterns rather than inventing new ones.

Two constraints shape the model:

- **A firm license bar.** Burrow only bundles backing services it can recommend and ship without
  copyleft friction: **Apache / MIT / BSD**. AGPL is out — Loki and recent Grafana are excluded.
  The set is **curated**, not arbitrary.
- **The invariants hold.** Credentials never cross MCP ([ADR-0004](0004-code-never-over-mcp.md),
  [ADR-0005](0005-mcp-server-holds-no-cluster-credentials.md)); every install/remove is a guarded
  control-plane operation ([ADR-0006](0006-guardrails-in-the-control-plane.md)).

## Decision

Model a backing service as an **add-on**, in two layers that mirror the provider registry:

- **Catalog — compiled-in and curated.** A vetted set of add-on types — `logs`
  ([VictoriaLogs](https://docs.victoriametrics.com/victorialogs/), Apache-2.0), `metrics`
  ([VictoriaMetrics](https://victoriametrics.com), Apache-2.0), `cache`
  ([ValKey](https://valkey.io), BSD-3), … — each an `AddonSpec`: the
  workload to create (a Deployment or StatefulSet + a ClusterIP Service, a PVC when stateful, a
  generated-credential Secret when the service needs auth), a **pinned image**, sane defaults,
  and the **connection contract** (the host, port, and Secret keys an app uses). Only
  Apache/MIT/BSD services enter the catalog; adding one is a deliberate vetting decision.
- **Instances — a DB-backed registry.** What is actually installed on this cluster: name, type,
  non-secret connection info, and a pointer to its credential Secret — the same shape as the
  provider registry ([ADR-0023](0023-provider-credentials.md)).

The control plane deploys add-ons through the **existing Kubernetes seam** apps already use, not
Helm — keeping full control, the guardrail gate, and the in-memory fakes the tests rely on
([ADR-0010](0010-testing-strategy.md)). A rare add-on that genuinely needs upstream manifests
uses the pinned-manifest path `ingress install` established; the catalog entry chooses per
add-on.

**Connection and credentials.** On install the control plane creates the workload and a ClusterIP
Service, and — when the service needs auth — writes a generated password into a **Secret in the
cluster**. That Secret never crosses MCP. The install result returns the **non-secret connection
details**: the in-cluster host, the port, and the **Secret name and keys**. The agent wires the
app to them (env-from-secret), exactly as it would any dependency — it receives the Secret's
*name*, never its value.

**Surface** (the noun grouping of [ADR-0024](0024-cli-command-taxonomy.md)): a new `addon` group
— `burrow addon add <type>`, `addon list`, `addon remove <name>` — all guarded. MCP gets a
read-only `burrow_addons` plus `burrow_addon_add` / `burrow_addon_remove`, so an agent can list
the catalog and what is installed, then install or remove a block. (`addon` is the working name;
refinable.)

**Guardrails.** Installing is `addon_install` (confirm by default); removing is `addon_remove`
(confirm by default — removing an observability store or a cache can break dependent apps). The
confirm-by-default posture is also the *adoption* mechanism, not only a safety one: operators say
plainly they will not let an agent change production without a human in the loop, so "the agent
proposes, the human approves, the agent executes" — backed by the deploy record as an audit
trail — is what makes an agent-operated cluster acceptable at all.

An add-on is consumed in one of two ways, and the catalog entry says which:

- **App-facing** (e.g. a cache): the app connects to it. Install returns the connection details +
  Secret name; the agent writes the app integration, as above.
- **Agent-facing** (observability): the *agent* queries it to answer questions. The add-on
  exposes its store to the agent as **MCP query tools** (e.g. a logs/metrics query), so "how is my
  app doing? / why is it slow?" is answered in plain language with query options. Burrow does
  **not** bundle a dashboard UI — Grafana is AGPL, and the evidence is that users want *answers,
  not dashboards*; the agent is the query layer.

First slices target **observability** — the universal day-two need (Kubernetes has no native
cluster-level logging; pod logs vanish when a pod is evicted) and the precondition for the agent
to operate competently at all: **logs ([VictoriaLogs](https://docs.victoriametrics.com/victorialogs/),
Apache-2.0)** first, then **metrics ([VictoriaMetrics](https://victoriametrics.com) /
Prometheus, Apache-2.0)**. The license bar bites hardest here and steers the picks: **Loki,
Grafana, and Tempo are all AGPL, and Elasticsearch is SSPL — excluded**; the Victoria stack is
Apache-2.0 and bundle-safe. A **cache (ValKey, BSD-3)** is a later, conditional slice — useful,
but a backing service only some apps need and orthogonal to "how is my app doing?".

## Consequences

- The differentiator ships as thin, reviewable slices: each add-on is one catalog entry plus its
  workload, behind one guardrail, recorded in one registry.
- It reuses the provider-registry, guardrail, and credential-Secret patterns, so there is little
  new architecture and the invariants hold by construction.
- The catalog is a **curated** surface: an add-on is a vetting decision (license, defaults,
  operational shape), not a config knob — AGPL services are excluded by policy, in code.
- The agent's contract is uniform: ask Burrow to install a block, receive connection details and
  a Secret name, write the integration. Credentials stay in the cluster.
- A future managed product can extend the catalog without changing the model.

## Alternatives considered

- **Helm-based install.** A heavy dependency and an opaque seam; Burrow prefers the Kubernetes
  seam it can fake and guard, with pinned manifests for the rare complex case (as `ingress
  install` does). Rejected as the default.
- **A generic "deploy any image as a service."** That is just `app deploy`; the value here is
  *vetted* defaults, operation, and a connection contract, which a generic deploy does not give.
- **Agent-chosen backing services (no catalog).** Defeats the license bar and the
  sane-vetted-defaults promise, and would put un-vetted, possibly-copyleft software on the user's
  cluster. The catalog is curated on purpose.
- **Per-type verbs (`burrow cache add`, `burrow metrics setup`).** More discoverable one at a
  time, but it fragments the surface and does not scale; one `addon` group with a type argument
  matches the catalog and ADR-0024's grouping.
