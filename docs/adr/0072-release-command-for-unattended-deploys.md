# ADR-0072: A release command for deploys nobody is driving

## Status

🟡 Proposed

## TL;DR

`burrow run` ([ADR-0048](0048-one-off-command-runner.md)) is how migrations happen, and it is the
right primitive: it runs a command in the app's own image with the app's own environment, and the
caller sequences it — run, deploy, run — which is what a real expand/contract migration needs.

**It assumes a caller.** Auto-deploy ([ADR-0052](0052-pull-based-passive-deploy.md)) and push-deploys
ship a new image with nobody present. Neither record mentions migrations, and there is no hook, so a
user who turns on auto-deploy and changes their schema has no supported way to migrate. They fall
back to the container entrypoint, which runs once per replica per restart and races itself.

This adds a **release command**: a command configured once on the app, run automatically as a Job
**before** an unattended deploy switches traffic, from the **new** image. **If it fails, the deploy
does not happen** and the running version keeps serving.

Rollback gets a **separate, optional command that defaults to nothing**, because whether a rollback
should touch the schema depends on the application — forward-only teams want nothing to happen, and
teams with reversible pairs may want the schema stepped back. When set, it runs from the image being
rolled back *from*, since that is where the code that knows how to undo its own migration lives.

It does not replace `burrow run`. ADR-0048 rejected a hook that would have *replaced* the explicit
call; this is one that covers the case the explicit call cannot reach, and the two are for different
situations — a sequenced release with a human or agent driving, versus a push at 3am.

Extends [ADR-0052](0052-pull-based-passive-deploy.md) and the cloud's push-deploy path. Complements
[ADR-0048](0048-one-off-command-runner.md). Reuses [ADR-0028](0028-app-config-and-secrets.md)'s
per-app config and [ADR-0032](0032-postgres-backups.md)'s Job machinery. Supersedes nothing.

## Context

### What exists today

- **`burrow app run <app> -- <cmd>`** runs a command synchronously in the app's current image with
  its config and Secret injected, gated by `app.run` (default `confirm`) and audited.
- **Auto-deploy** stores a per-`(app, environment)` level and deploys matching new tags without a
  caller. The cloud's push-deploy does the same from a webhook.
- **No release hook of any kind.** Searching the tree for a pre-deploy or release command finds
  nothing, and neither ADR-0052 nor the cloud's push-deploy record mentions migrations.

### What breaks

A user enables auto-deploy, pushes a commit that adds a column, and the new image ships. Nothing runs
the migration. The app starts, queries a column that does not exist, and fails — and the failure
looks like a bad release rather than a missing step.

Their options today are all poor:

- **Entrypoint migration** — runs on every pod start, so replicas race, and a restart re-runs it.
- **Hand-written Job** — needs `kubectl` and the app's Secret, routing around the control plane and
  the scoped credential that makes an agent safe to point at production.
- **Turn auto-deploy off** — which is to say, do not use the feature.

### Why `burrow run` cannot cover it, and why that is not a criticism of it

ADR-0048's reasoning is sound and unchanged:

> The explicit call is the spine… it is what lets the agent sequence and react — run then deploy, or
> express expand/contract as run → deploy → run — which a pre-deploy hook structurally cannot.

That is exactly right when someone is sequencing. The gap is that auto-deploy has **no such someone**
— by design, since the point is that a push ships. A primitive that requires a caller cannot serve a
path defined by its absence.

### What this record resolves

How an unattended deploy runs a migration, and what happens when it fails.

## Decision

### 1. A release command is app configuration, not a per-deploy argument

`burrow app release-command set <app> -- <cmd>` stores one command per app, per environment,
alongside the app's config ([ADR-0028](0028-app-config-and-secrets.md)). Unset means no release
command and today's behaviour exactly.

It has to be configuration because there is no caller to supply it — that is the whole premise.

### 2. It runs from the new image, before traffic moves

The Job runs the **image being deployed**, not the running one, so the migration ships with the code
that needs it. It runs against current state, with the app's config and Secret injected exactly as
`burrow run` does, and the deploy proceeds only after it exits zero.

### 3. A failed release command aborts the deploy

The new image does not roll out. The running version keeps serving, on the old schema, unchanged.

This is the property that justifies the feature: a migration failure becomes a **deploy that did not
happen** rather than a deploy that half-happened. The Job is left for diagnosis, and the failure is
reported as the deploy's failure, with the command's output, rather than as a mysterious crashloop
afterwards.

### 4. Rollback runs a separate command, or nothing, and the user decides which

Whether a rollback should touch the schema is a property of the application, not of Burrow. Teams
practising expand/contract migrate **forward only** and would be harmed by anything running on
rollback. Teams whose tool maintains reversible pairs — `goose`, `migrate`, `alembic`, Rails all
support a `down` — may legitimately want the schema stepped back.

So there is a **separate, optional rollback command**, and **unset means nothing runs**. The safe
default is the forward-only one, because a user who has not thought about it is likelier to be
forward-only than to have reversible migrations they trust.

**It runs from the image being rolled back FROM, not the one being rolled back to**, and this is the
part that is easy to get wrong. Rolling back from B to A, the code that knows how to undo B's
migration is in **B**. Running A's migration tool would not undo B's change — A does not know it
exists — it would undo *A's* last migration instead, which is worse than doing nothing.

**It runs before traffic moves back**, so the schema is stepped back before the older code begins
serving. Where B's migration was additive this ordering does not matter; where it was destructive it
is the difference between a restored app and a broken one.

The reuse of the release-command machinery is deliberate — same Job, same image resolution, same
config and Secret injection, same gating on exit code. What differs is which image it comes from and
that it is opt-in.

### 5. It runs on every deploy path, not only unattended ones

An explicit `burrow deploy` runs it too. A release command that fires only sometimes is worse than
one that always fires: the whole point is that the schema and the code move together, and an operator
who deploys by hand should not silently skip the step their auto-deploys depend on.

`burrow run` remains available for the sequenced case, and a release command does not prevent it.

### 6. One at a time, per app and environment

Two pushes in quick succession must not run two migration Jobs concurrently against one database. A
release command for an app and environment is serialized; a deploy waits for the previous one's
release command rather than racing it.

### 7. Burrow does not understand migrations

It runs a command. The migration tool — `goose`, `migrate`, `alembic`, `rails db:migrate` — is the
user's, and versioning, ordering and idempotency are that tool's job.

This is worth stating because the temptation to add "migration support" is real, and would mean
Burrow taking a position on a problem every language ecosystem has already solved differently.
ADR-0048 already declined a Postgres-only `burrow_run_sql` for the same reason.

## Consequences

- **Auto-deploy becomes usable by an app with a database**, which is most of them.
- **Expand/contract is still not expressible unattended.** A release command fires once per deploy,
  so a migration needing run → deploy → run still requires the sequenced path. An operator doing one
  of those turns auto-deploy off for that release, or accepts two deploys. This is a real limit and
  the honest framing is that the hook covers the common case — additive, backward-compatible
  migrations — and not the hard one.
- **With no rollback command set, a rollback returns the code and leaves the schema ahead of it.**
  If the forward migration was not backward-compatible, the rollback will not restore a working app.
  That is inherent to schema changes rather than introduced here, but it becomes reachable
  automatically now, so the rollback path should say what it did and did not do rather than reporting
  a bare success.
- **A rollback command that is set and wrong is worse than none.** It runs a schema change during an
  incident, from an image being abandoned, against a database whose state is already the reason
  someone is rolling back. It should be set only by a user whose migrations are genuinely reversible
  and who has exercised the down path — and the documentation should say that rather than presenting
  it as the tidier option.
- **The blast radius of a bad release command is every deploy of that app.** It is configuration set
  once and then forgotten, and a command that starts failing blocks deploys until someone notices.
  That is the correct failure direction — better than shipping code whose schema is missing — but it
  is a new way for an app to become undeployable.
- **`app.run`'s guardrail does not gate it.** The release command runs as part of a deploy, which is
  gated by `app.deploy`. Whether it deserves its own disposition is a real question this record does
  not settle: it executes arbitrary code, but so does the image being deployed.
- **A cloud tenant's release command runs their code**, so it inherits every isolation requirement
  the app itself has. On the managed product it must carry the same sandbox and node placement as any
  other tenant workload — which is not currently true of `burrow run` either, and is tracked
  separately.

## Rejected alternatives

- **Do nothing; tell users to run `burrow run` before deploying.** Correct for anyone driving a
  deploy by hand, and no new surface. Rejected because it is not available on the path that needs it:
  auto-deploy exists precisely so nobody has to be there, and "be there" is not an answer.
- **Replace `burrow run` with a release hook** — the Heroku shape, one mechanism instead of two.
  Rejected in ADR-0048 and still rejected: expand/contract needs sequencing, and a hook cannot stop
  between steps. The two mechanisms are not redundant; they serve attended and unattended releases.
- **An init container on the app's Deployment.** Kubernetes-native and needs no new concept.
  Rejected because it runs per pod: two replicas race, and every restart re-runs it. It is the
  entrypoint problem with more YAML.
- **Run the migration after the deploy**, so a failure does not block shipping. Rejected because the
  window between the new code starting and the migration finishing is exactly when the app queries a
  schema that does not exist — turning a blocked deploy into a live outage.
- **Have Burrow detect and run migrations** by recognising common tools. Rejected per §7: it would
  make Burrow responsible for every ecosystem's migration semantics, and be wrong for somebody
  immediately.
- **A single command used for both deploy and rollback**, with the tool told which direction to go.
  Fewer concepts, and it matches how migration tools are actually invoked (`goose up` / `goose down`).
  Rejected because the two differ in more than an argument: they run from **different images**, and
  one must default to doing nothing while the other defaults to running. Collapsing them would make
  the safe default impossible to express — a user who set a release command would silently acquire
  rollback behaviour they never chose.
- **Run the rollback command from the image being rolled back TO.** The intuitive reading of
  "re-deploy A, run A's release command". Rejected in §4: A's migration tool does not know B's
  migration exists, so it would step back one of A's own migrations instead. This is the failure that
  looks correct in a diagram and corrupts a schema in practice.
- **A separate `burrow migrate` verb**, distinct from both. Rejected as a third mechanism for the
  same job: it would still need a caller, which is the problem, and it would compete with `run`
  rather than complete it.
