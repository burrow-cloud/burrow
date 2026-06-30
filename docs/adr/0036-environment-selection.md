# ADR-0036: Environment selection — one `burrow env` surface that follows the kube context

## Status

Accepted. Supersedes the user-facing `burrow context` surface introduced as a stepping stone in
[ADR-0035](0035-environments.md) (phases 1–2), folding it into a single `burrow env` concept. Builds
on the per-cluster control plane ([ADR-0002](0002-four-layer-architecture.md)) and the
kubeconfig-proxy connection ([ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)). The
guardrail policy ([ADR-0020](0020-guardrails-as-configurable-policy.md)) and the burrowd-side
environment registry from ADR-0035 are unchanged in substance — this ADR changes only how a human
*selects* an environment and removes a redundant command surface.

## Context

ADR-0035 shipped environments in two shapes (cluster-per-env via `--context`; namespace-per-env via a
burrowd registry and `--env`) plus a `burrow context list` command and a `burrow_contexts` MCP tool.
Those context surfaces were a phase-1 stepping stone — they predated `burrow env`. Shipping both as
parallel commands invites the obvious "context vs env, which do I use?" confusion.

The target user's real workflow resolves the design:

- **Multiple clusters, one per environment** (`dev`/`nonprod`/`prod`), switched with `kubectx` to
  match the task; names vary by org (`staging`==`nonprod`) and change (`dev-new`). **Names are the
  user's** — never inferred.
- **Namespace set only sometimes** (`kubens`) — on a shared, multi-team cluster the operator pins
  their team's namespace so they don't pass it each time.

So a "burrow environment" in the user's head already *is* `{kube context, namespace}`, steered with the
standard tools. burrow should ride that, expose one concept, and never silently mis-target.

## Decision

**`burrow env` is the single environment surface. An environment is a user-named handle resolving to
`{context, control-plane-namespace, app-namespace}`. The terminal follows the current kube context by
default, always shows the resolved target, and lets you pin a handle. `burrow context` is retired as a
command; `--context` survives only as a low-level per-command flag. The agent targets explicitly.**

### The handle

A handle is `name → {context, control-plane-namespace, app-namespace}`:
- **context** — the kubeconfig context (which cluster's burrowd).
- **control-plane-namespace** — where burrowd runs, **default `burrow`**. Carrying this dimension now
  (even though installs are single-instance today) means a future *multiple burrowd per cluster*
  setup (a control plane per team) is a non-default value, not a breaking change.
- **app-namespace** — where the environment's apps live; for a namespace-per-env handle this is the
  burrowd-registered environment's namespace (so its guardrails apply), for cluster-per-env it is that
  cluster's app namespace.

Handles live **client-side** in a config file (see below): a handle can span clusters and burrowd is
per-cluster ([ADR-0002](0002-four-layer-architecture.md)), so no single control plane can hold the
map. This is human selector state, like the kubeconfig — not agent configuration.

### Commands

- **`burrow env`** and **`burrow env list`** list every handle and mark the active one and its mode,
  kubectx-style:
  ```
  NAME      CONTEXT             NAMESPACE
  dev       do-nyc1-dev         burrow-apps
  nonprod   do-nyc1-nonprod     team-x         <--- current (following kubectl)
  prod      do-nyc1-prod        burrow-apps
  ```
  Pinned shows `<--- current (pinned)`; when following and the current context matches no handle, a
  line reads `following kubectl: <context> (unregistered)` so the next command's target is never
  ambiguous.
- **`burrow env use <name>`** pins a handle (decouples from kubectl context switches).
- **`burrow env follow`** returns to tracking the current kube context (a sibling subcommand, not a
  flag on `use`).
- **`burrow env add <name>`** registers a handle; on a namespace-per-env cluster it also does the
  ADR-0035 server-side setup (create the namespace + RBAC, register it in burrowd) so its guardrails
  apply — one command, the server bits happen underneath.
- **`burrow scan`** walks every kubeconfig context, probes each (namespace-aware) for burrowd
  instances (installed? version? which control-plane namespace?), prints what it finds, and offers to
  register handles. It caches a hash of the kubeconfig so it notices a new context (offer to scan) or
  a current-context change.

### Default selection: follow the kube context

With nothing pinned, a command targets the **current kube context**, its **namespace** (so `kubens`
moves burrow too; falling back to the burrowd default app namespace when the context sets none), and
the default control-plane namespace. The resolved target is **printed on every command**, so a context
switch is never silent — the failure mode that makes "follow" risky is removed by making the target
legible, not by refusing to follow.

### `burrow context` retired; `--context`/`--env` kept as flags

The `burrow context list` command and the `burrow_contexts` MCP tool are removed — their job is covered
by `burrow env list` (handles) and `burrow scan` (raw discovery + burrowd probe), with
`kubectl config get-contexts` for the pure raw list. The **`--context` and `--env` flags remain** as
low-level, per-command overrides (run one command against a raw cluster or a specific env without
changing the sticky selection). They are escape hatches; `burrow env` is the surface.

### The agent targets explicitly

Over MCP, targeting is **named input arguments** (`context`/`env` — there are no flags in MCP), made
prominent and first in each tool's schema and described as the target. burrow-mcp resolves an `env`
through the same handle config, so the agent can name an environment. The `env` argument is
**optional**, but **every tool result echoes the environment it acted in**, so even a defaulted
operation is legible to the agent and to a human reviewing the audit trail. The agent never rides a
human's pin or the ambient context implicitly; the sticky selector is a human convenience. (A future
option to *require* `env` on mutating tools is left open; per-environment guardrails gate prod
regardless.) `burrow_environments` is the agent's discovery tool; `burrow_contexts` is removed.

### Config file

- **Location `~/.burrow/config`** (mirroring `~/.kube/config`), overridable with **`$BURROW_CONFIG`**
  (mirroring `$KUBECONFIG`).
- **YAML**, with an **`apiVersion` + `kind`** header (`apiVersion: burrow.dev/v1`, `kind: Config`) so
  the format can be migrated safely across versions.

## Consequences

- One environment concept (`burrow env`) instead of an overlapping context/env pair — the selector
  matches the `kubectx`/`kubens` muscle memory with no silent mis-targeting (the target is always
  printed).
- A small client-side config is introduced — a deliberate exception to "no files to manage," justified
  because it is *human* selector state (like the kubeconfig) the agent never depends on.
- Removing `burrow context list`/`burrow_contexts` and broadening `burrow env` revises surfaces shipped
  in ADR-0035. Acceptable: pre-1.0, days old, no external users — the same "get the naming right now"
  call as `app env`→`app config`.
- The control-plane-namespace dimension is carried from day one, so multi-burrowd-per-cluster (a
  control plane per team) can arrive later without breaking single-instance users.

## Rejected alternatives

- **Keep `burrow context` and `burrow env` as parallel commands.** Rejected: the context/env duality is
  the confusion; the raw-context list is subsumed by `burrow env list` + `burrow scan`.
- **Pinned/independent as the default.** Rejected: it drifts from the kube context the operator already
  steers; kept as the opt-in `burrow env use` pin.
- **A server-side/central registry for the cross-cluster handle map.** Rejected: burrowd is per-cluster
  ([ADR-0002](0002-four-layer-architecture.md)); the map's only home is the client.
- **Inferring posture from the name** (`prod` auto-locks). Rejected: guardrails are explicit policy
  ([ADR-0020](0020-guardrails-as-configurable-policy.md)); `staging`==`nonprod` proves names are not
  semantics.
- **Auto-following silently.** Rejected: tracking without showing the target is the danger; follow is
  paired with always-print-the-target.

## Out of scope (separate threads)

- **Associated read-only namespaces** — an opt-in operator grant (`get`/`list`/`watch` on jobs+pods)
  on extra namespaces so burrow can report status (not just collector logs) for an app's Jobs in a
  separate namespace. Its own future ADR under the access-tiers family.
- **Operating on a shared, non-admin cluster** in only the team's namespace — an install/RBAC question.
- **Connecting external/cloud log aggregators** (GCP Logging, Splunk, Elasticsearch) per environment —
  its own future ADR ([ADR-0026](0026-observability-query-adapters.md) family).
