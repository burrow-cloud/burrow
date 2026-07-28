# ADR-0067: One database instance per environment, and the first environment is called `prod`

## Status

✅ Accepted

## TL;DR

Attaching an app to Postgres creates a database named after the app and a role named `app_<app>`.
Neither name contains the environment:

```go
EnsureAppDatabase(ctx context.Context, app string) (databaseURL string, err error)
```

So the moment [ADR-0035](0035-environments.md)'s namespace-per-environment is actually used, an app
called `web` in staging and an app called `web` in production resolve to **the same database, owned
by the same role, on the same instance** — and because provisioning is idempotent, the second attach
silently adopts the first one's database rather than failing. Staging would write to production data
and nothing would report a problem.

Two decisions fix it and settle a question ADR-0035 left thin:

**One add-on instance per environment.** Not one instance with environment-qualified database names —
a separate instance. Qualified names would fix the collision, and the *data* would then be properly
isolated; what they cannot isolate is the server. The decisive case is upgrades: a Postgres
major-version upgrade applies to the whole server, so on a shared instance there is no way to run
staging on the new version and production on the old. The rehearsal a staging environment exists to
provide is not risky — it is impossible.

**Install creates one environment, and it is named `prod`.** ADR-0035 called the implicit one
`default`. A self-hoster's single environment *is* production, and naming it honestly is what makes
[ADR-0065](0065-what-belongs-on-the-agent-surface.md)'s safe defaults engage on day one instead of
sitting inert behind an environment nobody named.

Refines [ADR-0035](0035-environments.md) §Phase 2 (the implicit environment's name and shape) and
[ADR-0031](0031-postgres-addon.md) (one shared instance becomes one per environment). Interacts with
[ADR-0066](0066-postgres-on-cloudnativepg.md), which should not be accepted until this is settled —
its §1 restates the single-instance contract. Supersedes nothing.

## Context

### What exists today

- **One Postgres instance for the whole cluster**, in `burrow-addons` (ADR-0031). Not one per
  environment — environments did not exist when it was chosen.
- **`addon attach postgres <app>`** creates a database named `<app>` owned by a role named
  `app_<app>` on that instance, and writes the app's `DATABASE_URL` into its Secret.
- **The provisioner never sees an environment**: `EnsureAppDatabase(ctx, app)`.
- **Environments are namespaces** (ADR-0035 phase 2). An app's *Secret* is per-environment; its
  *database* is not.
- **Nothing is broken right now**, because nobody has created a second environment.

### What breaks the moment someone does

`env add staging`, then deploy `web` into it and attach Postgres. The provisioner is asked for a
database for `web` — with no environment to distinguish it — and finds `web` already exists, owned by
`app_web`. Provisioning is **idempotent**, so it does not fail. It rotates the role password and
hands back a `DATABASE_URL` pointing at production's database.

Staging is now writing to production data. Nothing errors, nothing warns, and the two apps look
correctly configured from every angle Burrow can show you.

**A collision that errors is a bug report. A collision that silently succeeds is a data incident
found later, by its consequences.** That is what makes this worth fixing before the feature that
triggers it is used, rather than after.

### What this record resolves

The collision, and the question ADR-0035 left open about what a shared add-on means across
environments. Two candidate fixes exist and they are not equivalent:

| | Fixes the collision | Isolates data | Isolates the server | Upgrade rehearsal |
| --- | --- | --- | --- | --- |
| **Qualified names** (`web`, `web_staging`) on one instance | yes | yes | **no** | **impossible** |
| **One instance per environment** | yes | yes | yes | yes |

Both isolate the *data* — no query in `web_staging` could read `web`. They differ in what happens
around the data.

**Server resources stay shared under qualified names.** One volume, one `max_connections`, one
autovacuum. A staging table that grows without bound fills the volume and production stops accepting
writes; a staging load test exhausts the connection limit for everyone; a long staging transaction
blocks autovacuum server-wide. None of these is a data leak — all of them are production outages
caused by the environment that exists to be where things break.

**And a major-version upgrade cannot be rehearsed on one instance at all.** An upgrade applies to the
*server*: one binary, one data directory, one catalog version, and `initdb` refuses a directory
written by a different major version. There is no arrangement in which staging runs Postgres 18 while
production stays on 17 — upgrading the shared instance moves production in the same operation. The
rehearsal a staging environment exists to provide is not risky; it does not exist.

With an instance per environment it is ordinary: upgrade staging, run the app against it, upgrade
production when satisfied.

## Decision

### 1. One add-on instance per environment

Each environment gets its own instance of a stateful add-on. Databases keep their simple names —
`web` in staging is a database on staging's instance, `web` in production is a database on
production's — so **isolation comes from the instance, not from a naming convention.**

That is deliberate, and it is the more expensive of the two options in §Context — qualified names on
one instance would cost nothing extra. The instance is what buys the server-level isolation and the
upgrade rehearsal that qualified names cannot provide at any price.

The provisioning seam therefore takes the environment as well as the app, and the environment is not
optional. A signature that can omit it is a signature that will omit it.

### 2. Install creates one environment, named `prod`

ADR-0035 phase 2 specified an implicit `default` environment. This names it `prod` instead, and
creates it explicitly at install.

**The decisive reason is guardrails, not naming taste.** ADR-0065 makes `app.delete` and
`dns.delete` deny-by-default and expects operators to relax them per environment — allow in
development, confirm in staging, deny in production. That gradient needs an environment with a
meaningful name to hang on. An environment called `default` invites `guard set --env default
app.delete allow` as the obvious way to make the friction stop, and the operator has then quietly
relaxed production without ever typing the word.

Calling it `prod` makes the same command read as what it is.

**A self-hoster's single environment is production.** They are running something real on it; the
absence of a staging environment does not make it not production. `default` describes Burrow's
internals; `prod` describes the user's situation.

**And it makes the second environment cheap.** Adding staging later is `env add staging`, with no
rename of the environment that holds live data and no re-pointing of apps.

### 3. The first environment maps to the existing namespace

`prod` maps to the app namespace the install already uses (`burrow-apps`), not to
`burrow-apps-prod`. The environment's *name* and its *namespace* are separate values, which ADR-0035
already allows via `--namespace`.

This is what keeps existing installs from needing a migration: they gain an environment named `prod`
pointing at the namespace their apps are already in. Nothing moves.

### 4. An app belongs to an environment, and operations default to it

Every app exists within an environment. A command with no `--env` targets the pinned or default
environment, so a single-environment user never types `--env` and a multi-environment user is always
explicit about production — which is [ADR-0047](0047-agent-environment-safety.md)'s existing
sticky-targeting property, inherited rather than reinvented.

### 5. Sharing one instance across environments is not supported

Not "discouraged" — unsupported, so that nothing has to defend against it. A user who wants one
server for cost reasons runs one environment.

The reason to close it explicitly: a shared instance is only safe with environment-qualified database
names, so permitting it would mean carrying *both* isolation models and choosing between them at
runtime. Two mechanisms for the same invariant is how the collision in §Context happened in the first
place.

## Consequences

- **A latent data-corruption bug is closed before the feature that triggers it ships**, which is the
  only cheap moment to close it.
- **Each environment costs a database instance** — a pod and a volume. Two environments is two
  Postgres servers, and for a small self-hoster that is a real number. It is the cost of the blast
  radius being real, and ADR-0031's density argument does not transfer: apps are many and
  environments are few.
- **Major-version upgrades become rehearsable**, which is the largest operational gain and does not
  show up until the first upgrade.
- **`default` disappears as a name**, and any documentation or tooling that assumed it needs
  updating. Installs predating this gain `prod` pointing at their existing namespace, so no data
  moves — but the name they see changes.
- **Someone whose only cluster is genuinely a sandbox now has an environment called `prod` with
  production-grade guardrail defaults**, and will find them stricter than they want. That is the
  intended direction of error: over-strict on a sandbox is an annoyance a `guard set` fixes;
  under-strict on real production is not recoverable. It should still be said plainly in the install
  output, since being told is the difference between a considered default and a surprise.
- **ADR-0066 must be revised before acceptance**, since its §1 restates ADR-0031's single-instance
  contract as unchanged.
- **The provisioning seam changes shape**, and every implementation and fake with it. That is a small
  cost paid once, and it is the change that makes the collision unrepresentable rather than merely
  avoided.

## Rejected alternatives

- **Environment-qualified database names on one shared instance** (`web_staging`). Much cheaper: no
  extra pod, no extra volume, one server to operate, and it does fix the collision. Rejected because
  it fixes only the collision. The **data** is then properly isolated — no staging query can read
  production — but the *server* is not: a staging table that fills the volume stops production
  accepting writes, a staging load test exhausts `max_connections` for everyone, and a major-version
  upgrade cannot be rehearsed at all, because it applies to the server and would move production to
  the new version in the same operation. It is the right answer if the per-instance cost proves
  prohibitive, and it should be reconsidered on those grounds rather than assumed away now.
- **Keep the implicit environment named `default`.** Consistent with ADR-0035 as written, and one
  less thing to explain. Rejected because it makes the safe defaults inert: the gradient ADR-0065
  expects has nowhere to attach, and the operator relaxing production does not see that they are
  doing it.
- **Create no environment at install; let the user name the first one.** Most honest in one sense —
  Burrow does not presume what the cluster is for. Rejected because it puts a naming decision in
  front of a user who wants to deploy something, and because "no environment" is the state in which
  environment-scoped guardrails do nothing.
- **An instance per app rather than per environment.** The strongest isolation available, and
  ADR-0031 already rejected it on density. Nothing here changes that: apps are many, environments are
  few, and the per-app boundary is a database.
- **Defer the whole question until someone creates a second environment.** Tempting, since nothing is
  broken today. Rejected because the trigger is a user action rather than a release: whoever runs
  `env add staging` first gets the data incident, and they will have no reason to expect it.
