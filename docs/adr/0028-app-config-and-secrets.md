# ADR-0028: App config and secrets — a lifecycle store, secrets off MCP

## Status

Accepted. **Its secret-transport decision (`app secret set` is kubeconfig-direct; the value never
crosses the control-plane API) is superseded by [ADR-0029](0029-secrets-through-the-control-plane.md)**
— secret values may traverse burrowd's authenticated API (for the web UI and the managed product),
still never over MCP. Everything else in this ADR stands. Builds on
[ADR-0004](0004-code-never-over-mcp.md) (code and secrets never travel over
MCP), [ADR-0023](0023-provider-credentials.md) (credentials live in a scoped Secret; only the key
crosses the API), [ADR-0007](0007-explicit-deploy-by-image-reference.md) (deploy is an explicit
call), [ADR-0020](0020-guardrails-as-configurable-policy.md) (guardrails), and
[ADR-0024](0024-cli-command-taxonomy.md) / [ADR-0019](0019-cli-framework-cobra.md) (CLI taxonomy
on Cobra). Prerequisite for the app-backend building blocks (a Postgres add-on writes
`DATABASE_URL` into the per-app Secret defined here).

## Context

A real app needs configuration and secrets to run: a `LOG_LEVEL`, a `DATABASE_URL`, a
`STRIPE_SECRET_KEY`. Today the only way to set environment is `app deploy --env KEY=VALUE`, which
has two problems:

- **It is a declaration, not a lifecycle.** You cannot add, change, or remove a single variable
  on a running app without a full redeploy that re-states everything. Operating an app means
  `config:set`-style mutation independent of releases.
- **It mishandles secrets.** `--env` renders values **inline and in plaintext** into the
  Deployment spec *and* persists them in the release record (Postgres) — and the MCP deploy tool
  carries `Env`, so a secret value would cross MCP, violating [ADR-0004](0004-code-never-over-mcp.md).
  A Stripe key or a database URL must never be inlined, persisted in the control-plane DB, or sent
  over MCP.

The app also needs to reach backends it does not host: a brought-your-own database (Neon,
Supabase) is a connection string the user holds — a secret — and a Burrow-provisioned database
(a future add-on) is a connection string Burrow generates. Both should reach the app the same way.

## Decision

Introduce an **app-scoped config/secret store**, managed independently of deploy, with two parallel
command groups under `app`. **Deploy no longer takes environment** — the store is the single source
of truth.

### Surface (CLI; ADR-0024 verb-first taxonomy)

```
burrow app env    set   <app> KEY=VALUE [--no-restart]   # upsert (add or replace)
burrow app env    list  <app>                            # shows KEY=VALUE
burrow app env    unset <app> KEY        [--no-restart]

burrow app secret set   <app> KEY=VALUE [--no-restart]   # value never crosses MCP or the API
burrow app secret list  <app>                            # KEYS ONLY — never the values
burrow app secret unset <app> KEY        [--no-restart]
```

`set` is an upsert. The app name is always a positional **argument**, never a command token, so an
app may legally be named `env`, `secret`, `list`, etc. (An `-a/--app` flag with a current-app
context is a deferred ergonomic follow-on, not in scope.)

### Env (non-secret)

Env values are **non-secret config**. They are stored in the control plane's Postgres (per-app,
the registry-in-Postgres principle) and **rendered inline into the Deployment pod template**
(`env: [{name, value}]`). Because the values live in the pod template, changing them mutates the
template and Kubernetes performs a **rolling update automatically** — no ConfigMap and no checksum
trick needed. `env list` shows values; the agent may read and set them (they are not secret).

### Secrets

Secret values live **only in a per-app Kubernetes Secret** in the app namespace. They are **never**
inlined into the Deployment, never written to Postgres, and never sent over MCP or the burrowd API
([ADR-0004](0004-code-never-over-mcp.md)). Postgres records only the **keys** (references), as
[ADR-0023](0023-provider-credentials.md) does for vendor credentials. The pod template consumes
them via `secretKeyRef` / `envFrom`. `secret list` returns **keys only**; no command ever echoes a
secret value back.

Because changing a secret *value* under an existing key does not mutate the pod template,
Kubernetes would not roll the Deployment. Burrow forces it with a **checksum annotation** (a hash
of the Secret's contents) on the pod template — the standard Helm `checksum/secret` pattern — so a
value change triggers a rolling update. Adding or removing a key changes the template and rolls on
its own.

### Who may set what (the MCP boundary)

- **`app env` set/list/unset** are also MCP tools — env is non-secret, so the agent manages it.
- **`app secret set` is CLI-only** (the kubeconfig/human path): the value is written directly to
  the Kubernetes Secret and never reaches MCP or the burrowd API. **`app secret list` (keys) and
  `app secret unset` (by key) may be MCP tools** — no value crosses.

The agent's flow for "a new release needs a secret" is therefore: ask the **user** to run
`burrow app secret set <app> KEY=VALUE`, confirm the key is present with `secret list`, then deploy.
The agent **must not ask the user to paste a secret value into the prompt** — anything in the prompt
is retained in the conversation and re-sent on every later tool call. The tool descriptions say so
explicitly.

### Rollout semantics and `--no-restart`

By default a change applies immediately (env: inline change rolls the Deployment; secret: checksum
annotation rolls it), so a running app picks it up — `config:set`-style. `--no-restart` updates the
store/Secret **without** the immediate rollout; the change lands on the next deploy. This collapses
the common "new release needs new config" case from two restarts (set rolls the old release, deploy
rolls again) to one. `--no-restart` is available over MCP for env, so the agent can self-optimize:
set env `--no-restart`, then deploy, for a single restart that boots with the new config present.

The deploy/env tools tell the agent to set config and secrets **before** deploying a release that
needs them, so the new release boots with them on first start rather than crash-looping and being
fixed on a second restart.

### Backend convergence

The per-app Secret is the single seam for database (and other backend) credentials:

- **BYO external DB** (Neon, Supabase): `burrow app secret set myapp DATABASE_URL=postgres://…`.
- **Burrow-provisioned DB** (a future Postgres add-on): Burrow generates the credentials and writes
  the same `DATABASE_URL` into the app's Secret automatically.

Either way the app just reads `DATABASE_URL` from its environment; whether the database is external
or Burrow-operated is a swappable detail the agent never has to special-case.

## Consequences

- Real apps can ship: config and secrets (DB URL, Stripe key) are first-class and managed over the
  app's life, not frozen at deploy time.
- Secrets stay off MCP, out of Postgres, and out of the Deployment spec — the [ADR-0004](0004-code-never-over-mcp.md)
  boundary now covers app secrets, not just Burrow's own credentials.
- **Breaking change:** `app deploy` (and the MCP deploy tool) drop `--env`/`Env`. The surface is
  early and pre-1.0; a one-time break is cheaper than two sources of truth racing. A first deploy
  with no env is fine — first deploys are not yet live/production.
- Double-restart churn on "config + new release together" is mitigated by `--no-restart`.
- Env values are visible in the Deployment spec (`kubectl get deploy -o yaml`). Acceptable — they
  are non-secret by definition; anything sensitive goes through `app secret`.
- Each mutation is a guardable, auditable control-plane action and feeds the audit log
  ([ADR-0027](0027-audit-log.md)).

## Rejected alternatives

- **Keep env on deploy.** No lifecycle, and it leaks secrets (plaintext in the DB, value over MCP).
- **Env via a ConfigMap + `envFrom`.** A ConfigMap consumed as env does **not** auto-roll the
  Deployment on change (env is injected only at pod start), so it would need the same checksum
  trick anyway — inlining the values into the pod template is simpler and rolls for free.
- **Secret values through the API/Postgres.** Violates [ADR-0004](0004-code-never-over-mcp.md);
  the value must live only in the Kubernetes Secret, set on the kubeconfig path.
- **One `app env --secret` flag instead of two groups.** Mixes two different exposure rules (values
  shown vs keys-only) and two different set paths (MCP-allowed vs CLI-only) under one verb —
  error-prone. Separate groups make the boundary obvious.
- **App name as the command token (`burrow app <app> secret set`).** Reads nicely but collides with
  reserved verbs (an app named `secret`/`deploy`/`list` becomes unreachable) and fights Cobra's
  static subcommand model ([ADR-0019](0019-cli-framework-cobra.md)). The app stays a positional
  argument.
