# ADR-0085: A guardrail can name the app it guards

## Status

📝 Proposed

## TL;DR

Guardrails scope to an environment, not to an app. So "the agent must not redeploy *this one thing*"
has no way to be said, and the only way to say it is to put that thing in an environment of its own.

- **`guard set --app <name>`** — a disposition for one app, or one add-on.
- **Three tiers, most specific wins**: app, then environment, then the built-in default.
- **No migration.** The policy key is a string and lookup never parses it.
- **Each guardrail declares whether it is app-scopable**, the same way `envScoped` already works.

Extends [ADR-0035](0035-guardrail-policy.md) phase 2c's environment scoping and
[ADR-0068](0068-operational-limits-are-configuration.md) §5's declaration rule. Removes the reason
cloud ADR-0021 §3 gives the control plane an environment of its own. Supersedes nothing.

## Context

`guardrail_policy` is two columns:

```sql
CREATE TABLE guardrail_policy (
    code        TEXT PRIMARY KEY,
    disposition TEXT NOT NULL
);
```

The environment is a **prefix on the code string**, not a column.
[`controlplane/guardrails.go`](../../controlplane/guardrails.go) resolves it by trying the prefixed
key and falling back:

```go
if d, ok := p.Dispositions[GuardrailCode(env+"."+string(code))]; ok && d.Valid() { ... }
if d, ok := p.Dispositions[code]; ok && d.Valid() { ... }
```

So a disposition applies to **every app in an environment**. There is no way to say "deny this for
that one app".

**What that costs, concretely.** The managed product is about to deploy its own control plane as an
ordinary Burrow application (cloud ADR-0021 §1), onto the same install that hosts the marketing site.
The control plane must not be redeployable, restartable or reconfigurable by the agent — a deploy
call repointing it at another image would hand over the database DSN, the platform proxy token and
the GitHub App private key. But the marketing site in the same environment *must* stay
agent-operable, because operating it is the dogfooding the whole arrangement exists for.

Those two requirements cannot both be expressed. So cloud ADR-0021 §3 gives the control plane an
environment of its own — which is a workaround for this record's absence, not a decision anybody
wanted. It was pushed back on the first time it met an operator:

> we already have a "prod" environment which includes the website. I would push back on creating an
> environment specifically for burrow-cloud.

**And the workaround is one-way.** Moving an app between environments is not a setting; it is
recreating the app, which for a control plane means downtime and re-establishing its config and
secrets. Deciding this after go-live means paying that, so it is cheaper to decide it now.

**The general want is not about control planes.** *"Don't let the agent touch this one app"* is an
ordinary thing to want — a payments service, a database migration runner, anything whose blast radius
is different from its neighbours'. Environment-scoped guardrails answer *"don't let the agent touch
production"*, which is a real question and a different one.

## Decision

### 1. A disposition can name an app

```
burrow guard set --app burrowd-cloud app.deploy deny
burrow guard set --env prod --app burrowd-cloud app.deploy deny
```

`--app` names an application, or an add-on for the `addon.*` codes. The guardrail code already says
which kind of thing it targets, so one flag covers both without ambiguity.

### 2. Resolution is three tiers, most specific first

| Key tried | Meaning |
| --- | --- |
| `prod.burrowd-cloud.app.deploy` | this app, in this environment |
| `prod.app.deploy` | every app in this environment |
| `app.deploy` | every app everywhere |
| — | the built-in default in code |

`Source` on `GuardrailInfo` already reports `env`, `global` or `default` so `guard list` can show
where an effective disposition came from. It gains `app`.

**No migration.** The key is a `TEXT PRIMARY KEY` and resolution only ever *constructs* a key and
looks it up — it never parses one. App names are DNS labels and cannot contain a dot, so the
composed key is unambiguous without anything having to split it.

### 3. Each guardrail declares whether it is app-scopable

`envScoped` is already a declaration on `guardrailDef` rather than something inferred from the code's
name ([ADR-0068](0068-operational-limits-are-configuration.md) §5). App scoping is declared the same
way.

Not everything should be. `dns.write` and `bucket.create` act on things outside the cluster that no
single app owns; `addon.restore_instance` takes **every** app on a Postgres instance back together,
which is exactly why it is dangerous, and scoping it to one app would describe its blast radius
falsely. A guardrail whose effect is wider than one app must not be settable as though it were
narrower.

### 4. `guard list` shows which tier answered

Listing with `--app` shows the effective disposition for that app and the tier it came from, so
"why is this denied" is answerable without reconstructing the fallback chain by hand.

## Consequences

**The control plane goes in `prod` with everything else.** Cloud ADR-0021 §3's separate environment
stops being necessary, and the denials that protect it name it directly — which is also more legible
than a policy that protects it by being alone in a room.

**A narrower thing to get wrong.** An operator who means "deny for the control plane" and types it
without `--app` denies it for the whole environment, including apps the agent is supposed to run.
That failure exists today too, and is the *only* thing available today; §4's listing is what makes it
visible.

**More keys in one table.** A per-app disposition for many apps is many rows where there was one.
The table holds overrides only, so it stays small in practice, but it is a table that now grows with
apps rather than with environments.

**The refusal message gets better.** *"`app.deploy` is denied for `burrowd-cloud`"* is a sentence
someone can act on. *"`app.deploy` is denied"* on an install where it is allowed for everything else
reads like a bug.

## Alternatives rejected

**Keep the separate environment.** Ships today with no code, and it is what cloud ADR-0021 §3 already
says. Rejected because the workaround is one-way — moving the app later means recreating it — and
because it answers only this instance of a general problem. The next app that needs protecting gets
its own environment too, until environments are an access-control mechanism wearing an environment's
name.

**Add an `app` column to `guardrail_policy`.** The obvious schema, and it is a migration, a composite
key, and a change to every read path — for a lookup that is already a string key nothing parses. The
prefix scheme extends to a third tier by constructing one more candidate key.

**Infer app-scopability from the code prefix** — `app.*` and `addon.*` are scopable, the rest are
not. Reads as a tidy rule and is wrong in the case that matters: `addon.restore_instance` starts with
`addon.` and takes every app on the instance down together. ADR-0068 §5 already decided this axis
should be declared rather than inferred, for this reason.

**Scope to an app *instead of* an environment.** Simpler, one tier. It loses "deny in production,
allow in staging", which is the thing environment scoping was built for and the more common want.

**Wait until after go-live.** The cost of waiting is not the code, it is that the workaround has to
be un-done afterwards, and un-doing it means recreating the control plane.
