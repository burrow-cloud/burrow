# ADR-0035: Environments — context-routed clusters and namespace-scoped environments

## Status

Accepted. Adds **environments** so an agent can be given free rein in staging and held back in
prod. Extends the configurable-guardrail policy ([ADR-0020](0020-guardrails-as-configurable-policy.md))
to be per-environment, and builds on the per-cluster control plane ([ADR-0002](0002-four-layer-architecture.md))
and the kubeconfig-proxy connection ([ADR-0014](0014-self-host-connectivity-via-kubeconfig.md)).

## Context

The single loudest reason self-hosters resist letting an agent operate a cluster is "don't let it
touch prod" (`research/competitive-icp-positioning.md`). The answer is **per-environment
guardrails**: let the agent deploy and iterate freely in `staging`, and gate or deny the same
operations in `prod`. That requires a first-class notion of an *environment*.

An **environment = a kubeconfig context (a cluster) + a namespace + a guardrail policy.** Real
setups sit at two ends of that line:

- **Cluster-per-environment** — `prod` is a separate cluster, a different kubeconfig context (common
  in larger orgs; the maintainer's own setup).
- **Namespace-per-environment** — one cluster, `dev`/`stage`/`prod` as namespaces (the common
  single-cluster self-hoster case).

The mechanism differs sharply between them, so they ship in two phases.

Two facts shape it:
- **burrowd is per-cluster** ([ADR-0002](0002-four-layer-architecture.md)) — there is no central
  multi-cluster control plane. A multi-cluster setup is several burrowds, one per cluster.
- **The kubeconfig already holds every cluster** as a context, and the `connect` package already
  reaches a burrowd through the chosen context's API-server proxy. Today it uses the *current*
  context only (empty `ConfigOverrides`); there is no context selector.

## Decision

**Model an environment as a (context, namespace) target the CLI and the agent select per operation,
with its own guardrail policy. Ship cluster-per-env (context routing) first, namespace-per-env
second.**

### Phase 1 — cluster-per-env via context routing

The kubeconfig contexts *are* the environments; no new registry or config file is introduced (the
kubeconfig is the registry — agent-driven, nothing to manage).

- **`connect.Options` gains a `Context` field**, wired into the kubeconfig load
  (`ConfigOverrides.CurrentContext`). Empty = the current context (today's behavior, no regression).
- **CLI: a global `--context` flag** selects the target cluster's burrowd: `burrow --context
  prod-cluster app status web`. A read-only **`burrow context list`** lists the kubeconfig contexts
  and marks the current one (the name `context` avoids colliding with the existing `app env`, which is
  env vars; phase-1 environments *are* contexts).
- **MCP: per-call routing.** The mutating tools take an optional `context` argument; burrow-mcp
  resolves it to a client for that context (built and cached per context, each reading its own
  cluster's token), defaulting to the current context. A read-only **`burrow_environments`** tool
  lists the contexts so the agent knows what it can target.
- **Per-environment guardrails come for free:** each cluster's burrowd already holds its own
  guardrail policy ([ADR-0020](0020-guardrails-as-configurable-policy.md)), so the operator locks
  down prod-cluster's burrowd and leaves staging-cluster's permissive. No new mechanism.

### Phase 2 — namespace-per-env within one cluster

One burrowd owns several app namespaces, one per environment.

- **An environment registry in burrowd** (registry-state-in-Postgres): a named environment maps to a
  namespace and carries its own guardrail policy. **`burrow env add <name>`** is a kubeconfig-side
  setup operation (like `install`): it creates the env's namespace and grants burrowd a Role there
  (today burrowd has a Role in exactly one app namespace). **`burrow env list`** shows them.
- **Apps are per-environment:** an app deploys into its environment's namespace, and its per-app
  Secret is per-environment (`staging`'s `DATABASE_URL` differs from `prod`'s). Operations take the
  environment (`--env`, and an `env` argument on the agent's tools).
- **Per-environment guardrails reuse the dotted codes** ([ADR-0034 dotted rename]): the code gains
  the environment as a prefix (`prod.app.delete`, `staging.app.delete`), stored env-scoped in the
  policy table. The default disposition still applies when an env has no override.
- **Default when no environment is created:** a single implicit `default` environment behaving
  exactly like today (the current context, the `burrow-apps` namespace, the current guardrail
  defaults). Multi-environment is opt-in; no regression.

### Common to both: explicit, safe targeting

The headline safety property is the same: the agent must **explicitly name** the environment to act
on it, so a prod change is never accidental, and that environment's guardrails gate it. The
environment posture is set explicitly (presets like prod-strict, dev-permissive); it is **never
inferred from the name** — a guardrail is policy, not magic ([ADR-0020](0020-guardrails-as-configurable-policy.md)).

## Consequences

- **"Let the agent rip in staging, gate prod"** becomes real — the direct answer to the top adoption
  objection, and a differentiation a dashboard-only or single-policy tool does not have.
- **Phase 1 is mostly wiring** the context selection that half-exists, plus the MCP client-per-context
  factory; it adds no burrowd RBAC and serves multi-cluster orgs immediately. **Phase 2** adds the
  registry, the per-env-namespace Role grant, per-env secrets, and env-prefixed guardrail keys.
- **The MCP server moves from one client to a client-per-context factory** — the one structural change
  in phase 1. Each context's client reads that cluster's own token.
- **No new files to manage** in phase 1 (kubeconfig is the source of truth); phase 2's env registry
  lives in burrowd's Postgres, queried by the agent — consistent with keeping control-plane state in
  the database, not local config.

## Rejected alternatives

- **A central multi-cluster control plane.** Rejected: burrowd is deliberately per-cluster with only
  its own cluster's credentials ([ADR-0002](0002-four-layer-architecture.md)); a central plane that
  holds many clusters' credentials is a far larger blast radius. Client-side context routing keeps
  each cluster's burrowd independent.
- **A client-side env→context alias file** (so envs get friendly names in phase 1). Rejected for now:
  it is a file to manage, against the agent-driven premise; the kubeconfig context name is the env
  identifier, and `kubectl config rename-context` gives friendly names. Phase 2's named environments
  cover the friendly-name case within a cluster.
- **Inferring strict guardrails from an environment named "prod".** Rejected: name-magic is fragile;
  guardrail posture is explicit policy ([ADR-0020](0020-guardrails-as-configurable-policy.md)).
- **One global guardrail policy across environments.** Rejected: that is exactly what blocks "free in
  staging, gated in prod"; per-environment policy is the point.
