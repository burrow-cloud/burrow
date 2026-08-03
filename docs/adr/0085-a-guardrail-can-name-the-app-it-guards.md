# ADR-0085: A guardrail can name the app it guards

## Status

✅ Accepted

## TL;DR

Guardrails scope to an environment, not to an app. So "the agent must not redeploy *this one thing*"
has no way to be said, and the only way to say it is to put that thing in an environment of its own.

- **`guard set <code> --env <env> --name <thing>`** — one flag for both, because the guardrail code
  already says whether it means an app or an add-on instance.
- **`--name` requires `--env`.** Without it the key is indistinguishable from an environment of the
  same name.
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

### 1. A disposition can name one app or one add-on instance

```
burrow guard set app.run --env prod --name website deny
burrow guard set addon.restore_instance --env prod --name postgres-prod deny
```

**One flag, `--name`.** Not `--app` and `--addon`. The guardrail code already says what kind of thing
it targets, so a second flag repeating it produces `guard set app.run --app website`, where the
reader has to ask why `app.run` needs telling that it concerns an app. `--name` also matches
`burrow addon config postgres --name <instance>`
([ADR-0082](0082-an-addon-instance-is-configured-after-it-exists.md)).

The code resolves what `--name` refers to: an app for `app.*`, an add-on instance for `addon.*`. A
guardrail that names nothing refuses `--name` with a message saying why its effect is wider than one
thing.

**`--name` requires `--env`, and this is a refusal rather than a default.** The key is
`<env>.<name>.<code>`. Without the environment there is no slot for the name, and the obvious
fallback `<name>.<code>` is byte-identical to the key an *environment* of that name produces. App
names and environment names are both DNS labels, so nothing tells them apart: `website.app.run` would
mean "the `website` app" or "everything in the `website` environment" depending on facts the lookup
cannot see.

**Add-ons need no new identity for this.** They are already addressed by name — `addon remove
<name>`, `restore-instance <addon>`, `attach`/`detach <addon> <app>` — and
`AddonInstanceName(type, env)` is what produces it. So this is **instance-level from the start**, and
stays correct when an environment can hold several instances
([#432](https://github.com/burrow-cloud/burrow/issues/432)) rather than needing a fourth tier later.

**Some guardrails name two things.** `addon.detach` is `detach <addon> <app>`: detaching `web` from
`postgres-prod` destroys `web`'s database, and either target could plausibly scope it. **It scopes by
the add-on**, because that is where the data lives and where the blast radius is bounded. Scoping by
app would let an operator protect `web` while the identical verb wipes `api` on the same instance —
which reads as protection and is not.

`addon.restore` has the same two-target shape and resolves the same way, for the same reason: every
database either verb can reach lives on one instance. `addon.attach` is not a guardrail at all —
attaching provisions and destroys nothing.

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

Not everything should be. `dns.write`, `dns.delete` and `bucket.create` act on things outside the
cluster that no app or add-on owns, so there is no name to give them. A guardrail whose effect is
wider than the thing named must not be settable as though it were narrower.

**Correction, 2026-08-02.** This section originally named `addon.restore_instance` as the example of
something unscopable, because it takes every app on a Postgres instance back together. That argument
is against scoping it to an **app**, and it holds. It does not survive `--name` meaning the *add-on
instance* for `addon.*` codes: the instance is precisely that verb's blast radius, so naming it
describes the reach honestly rather than falsely. `addon.restore_instance` is scopable, by instance.
The three genuinely unscopable guardrails are the ones above.

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

**Two flags, `--app` and `--addon`.** Explicit, and it makes every command state twice what kind of
thing it concerns. `guard set addon.remove --addon postgres-prod` is not clearer than
`--name postgres-prod`; it is longer.

**Let `--name` stand alone, meaning "this app in every environment".** A legitimate want — *never let
the agent delete the control plane, wherever it lives*. It needs a key shape of its own to avoid the
collision above, which is machinery bought for a case that does not exist while there is one
environment. An error now is cheaper to relax later than a shape is to unpick.

**Scope add-on guardrails by the app rather than the add-on.** For the two-target verbs it is the
more natural reading — `detach <addon> <app>` is something done *to* an app. Rejected because the
protection would be illusory in exactly the case it exists for: the destructive reach of a verb on a
shared Postgres instance is the instance, not the one app named on the command line.

**Scope to an app *instead of* an environment.** Simpler, one tier. It loses "deny in production,
allow in staging", which is the thing environment scoping was built for and the more common want.

**Wait until after go-live.** The cost of waiting is not the code, it is that the workaround has to
be un-done afterwards, and un-doing it means recreating the control plane.
