# ADR-0072: Lifecycle hooks named for when they run, and told how it went

## Status

✅ Accepted

## TL;DR

Auto-deploy ships a new image with nobody present, and there is **no hook at any phase**. A user who
enables it and changes their schema has no supported way to migrate, and no way to hear that the
deploy then crashlooped.

This adds **lifecycle hooks named for the moment they run** — `pre-deploy`, `post-deploy`,
`pre-rollback` — as one command with a phase you name.

- **Not "release".** That word means a tag and a changelog. A phase name says *when*, which is the
  only thing a reader needs.
- **`pre-deploy` runs on every deploy, automated or not**, from the new image, before traffic moves.
  Failure aborts the deploy and the running version keeps serving.
- **A rollback fires `pre-rollback`, never `pre-deploy`.** `pre` phases are direction-specific
  because they run from different images; `post` phases are not, so `post-deploy` fires after a
  rollback too, told that it was one.
- **`post-deploy` is told the outcome** — and, on failure, the reason from
  [ADR-0074](0074-burrow-observes-what-it-manages.md) §2's vocabulary (`CrashLoopBackOff`,
  `Unschedulable`, `OOMKilled`). **It runs either way**; a post hook that fires only on success cannot
  report the case it exists for.
- **Burrow never rolls back by itself.** It reports; the hook decides.
- **`pre-rollback` defaults to nothing**, and runs from the image being rolled back *from* — that is
  where the code that knows how to undo its own migration lives.

**The limit worth knowing before you read "succeeded":** Burrow has no readiness probe for user apps,
so success means *the rollout completed and no pod reported a blocking condition* — not *the app is
healthy*. §7.

Extends [ADR-0052](0052-pull-based-passive-deploy.md). Complements
[ADR-0048](0048-one-off-command-runner.md). Depends on
[ADR-0074](0074-burrow-observes-what-it-manages.md) §2 for the outcome vocabulary. Supersedes nothing.

## Context

### What exists today

- **`burrow app run <app> -- <cmd>`** runs a command synchronously in the app's current image with
  its config and Secret injected, gated by `app.run` and audited.
- **Auto-deploy** stores a per-`(app, environment)` level and deploys matching new tags without a
  caller. The cloud's push-deploy does the same from a webhook.
- **No hook of any kind, at any phase.** Neither ADR-0052 nor the cloud's push-deploy record mentions
  migrations.
- **The outcome of a deploy is now observable**, which it was not when this record was first drafted:
  ADR-0074 §2 widened `WorkloadStatus.Issue`/`IssueReason` beyond image pulls to the blocking classes.
- **`deploymentRolledOut` defines success** as observed generation current, updated replicas at
  desired, and available replicas at desired.
- **There is no readiness probe on a user application.** `ReadinessProbe` appears once in the tree, on
  the add-on path. Without one, Kubernetes marks a pod ready as soon as its container starts.

### What breaks

A user enables auto-deploy, pushes a commit that adds a column, and the new image ships. Nothing runs
the migration. The app starts, queries a column that does not exist, and fails — and the failure looks
like a bad release rather than a missing step.

Their options today are all poor: an **entrypoint migration** that runs on every pod start so replicas
race and restarts re-run it; a **hand-written Job** needing `kubectl` and the app's Secret, routing
around the control plane and the scoped credential that makes an agent safe to point at production; or
**turning auto-deploy off**, which is to say not using the feature.

**And nothing tells anyone how it went.** Even with a pre-deploy hook, an unattended deploy that ships
and then crashloops is silent. The push succeeded, the rollout was attempted, and the only party who
could act is not watching. A hook that runs *before* the deploy cannot report on it.

### Why `burrow run` cannot cover it, and why that is not a criticism of it

ADR-0048's reasoning is sound and unchanged:

> The explicit call is the spine… it is what lets the agent sequence and react — run then deploy, or
> express expand/contract as run → deploy → run — which a pre-deploy hook structurally cannot.

That is exactly right when someone is sequencing. The gap is that auto-deploy has **no such someone**
— by design, since the point is that a push ships. A primitive that requires a caller cannot serve a
path defined by its absence.

### What this record resolves

What hooks exist, when each runs, what they are told, and what Burrow does with a failure.

## Decision

### 1. Hooks are named for when they run

`pre-deploy`, `post-deploy`, `pre-rollback`.

**Not "release".** In every other tool a release is an artifact — a tag, a changelog, the thing that
records what shipped. Borrowing the word for a command that happens to run during a deploy asks the
reader to learn a second meaning, and it does not answer the only question they have, which is *when
does my command run*. A phase name answers it in the name.

It is **one mechanism**, configured per app and environment alongside the app's config
([ADR-0028](0028-app-config-and-secrets.md)), with the phase named rather than a command per phase:
`burrow app hook set <app> --on pre-deploy -- <cmd>`, with `hook list` and `hook unset` beside it.
Unset means no hook and today's behaviour exactly.

It has to be configuration because there is no caller to supply it — that is the whole premise.

### 2. `pre-deploy` runs on every deploy path, from the new image, before traffic moves

**Every path**, including an explicit `burrow deploy` and including automated ones. A hook that fires
only sometimes is worse than one that always fires: the point is that schema and code move together,
and an operator deploying by hand should not silently skip the step their auto-deploys depend on.

It runs the **image being deployed**, not the running one, so the migration ships with the code that
needs it, with the app's config and Secret injected exactly as `burrow run` does.

### 3. A failed `pre-deploy` aborts the deploy

The new image does not roll out. The running version keeps serving, on the old schema, unchanged.

This is the property that justifies the pre phase: a migration failure becomes a **deploy that did not
happen** rather than a deploy that half-happened. The Job is left for diagnosis, and the failure is
reported as the deploy's failure, with the command's output, rather than as a mysterious crashloop
afterwards.

### 4. `post-deploy` receives the outcome, and runs either way

It runs **after the rollout settles**, and it runs **whether it succeeded or failed**. A post hook
that fires only on success cannot report a failure, which is the case it exists for.

**It fires after a rollback too**, told that the deploy it is reporting on was one. "Did this settle
and is it serving?" is the same question whichever direction the image moved, and a separate
`post-rollback` phase would be a fourth name for an identical answer.

What it receives:

- **the outcome** — succeeded or failed;
- **the reason**, when it failed, from ADR-0074 §2's closed vocabulary, so a hook can branch on the
  cause without parsing prose;
- **the app, environment and image**, so a hook can report what it is talking about.

**This depends on ADR-0074 §2 and could not have been specified before it.** Until the Issue
vocabulary was widened, an unavailable workload reported `Available: false` and an empty reason, so a
post-deploy hook could have been told *that* a deploy failed and never *why*. A hook that knows only
"something went wrong" is a notification, not an integration.

### 5. Settling needs a deadline, and the deadline is a decision

A rollout does not finish; it either completes, fails, or hangs. `post-deploy` therefore waits with a
bound, and when the bound expires the outcome is **failed, with the reason Burrow observed** — not a
bare timeout.

This is issue [#352](https://github.com/burrow-cloud/burrow/issues/352)'s shape and it must not be
repeated: a waiter that burns its full deadline and reports elapsed time, when the cluster was saying
"unschedulable" the entire time, has converted a diagnosis into a shrug. The bound is an operational
limit and belongs in [ADR-0068](0068-operational-limits-are-configuration.md)'s configuration rather
than in a constant.

### 6. Burrow does not roll back by itself

A failing `post-deploy` hook does not undo the deploy, and Burrow does not roll back automatically on
a failed rollout.

The hook is told what happened and decides — including by calling `burrow rollback` itself, which it
is free to do. That is ADR-0074 §9's restraint in a second place: the remedy for a failed deploy is
usually a judgement about blast radius and data, not a reflex, and an automatic rollback of a deploy
whose `pre-deploy` migration already ran can leave the schema ahead of the code it just restored.

An automatic-rollback policy is defensible and is **its own record**, with its own guardrail.

### 7. What "succeeded" means, and what it does not

Burrow reports the outcome it can observe: the rollout completed, and no pod reported a blocking
condition. **That is not the same as the application being healthy.**

**There is no readiness probe on a user application.** Kubernetes marks a pod ready as soon as its
container starts, so an app that boots, listens, and returns 500 to every request is a *successful*
deploy by this definition. `deploymentRolledOut` counts available replicas; without a probe,
"available" means "running".

So a `post-deploy` hook that wants to know the app actually works has to check that itself — which it
is well placed to do, being a command in the app's own image with the app's own environment. A smoke
test is the natural `post-deploy` hook, and the honest framing is that Burrow tells the hook the
deploy *happened*, and the hook decides whether it *worked*.

**Supporting a readiness probe on user apps would close this**, and it is deliberately not decided
here: it changes what a deploy waits for, it needs a per-app configuration surface, and a
badly-configured probe turns a working deploy into a failed one. It is the obvious next record, and
this one should not pre-empt it by pretending the gap is smaller than it is.

### 8. `pre-rollback` runs from the image being left, or nothing runs

Whether a rollback should touch the schema is a property of the application, not of Burrow. Teams
practising expand/contract migrate **forward only** and would be harmed by anything running on
rollback. Teams whose tool maintains reversible pairs — `goose`, `migrate`, `alembic`, Rails — may
legitimately want the schema stepped back.

So `pre-rollback` is separate and **unset means nothing runs**. The safe default is the forward-only
one, because a user who has not thought about it is likelier to be forward-only than to have
reversible migrations they trust.

**It runs from the image being rolled back FROM, not the one being rolled back to.** Rolling back from
B to A, the code that knows how to undo B's migration is in **B**. Running A's migration tool would
not undo B's change — A does not know it exists — it would undo *A's* last migration instead, which is
worse than doing nothing.

**It runs before traffic moves back**, so the schema is stepped back before the older code serves.

**A rollback fires this and never `pre-deploy`**, which §2's "every deploy path" would otherwise imply,
since a rollback is mechanically a deploy of an older image. Firing `pre-deploy` on a rollback would
run **A's** migration tool while returning to A — and A does not know B's migration exists, so it would
step back one of *A's own* migrations instead. That is the failure this section already warns about,
arriving through a different door, which is why the exclusion is stated rather than left to inference.

### 9. One at a time, per app and environment

Two pushes in quick succession must not run two migration Jobs concurrently against one database. A
hook for an app and environment is serialized, and a deploy waits for the previous one's hooks rather
than racing them.

This also covers a case that is not a deploy: a database promotion or a maintenance operation that
must not overlap a migration needs the same lock, so serialization is per app and environment rather
than per deploy.

### 10. Burrow does not understand migrations

It runs a command. The migration tool — `goose`, `migrate`, `alembic`, `rails db:migrate` — is the
user's, and versioning, ordering and idempotency are that tool's job.

This is worth stating because the temptation to add "migration support" is real, and would mean Burrow
taking a position on a problem every language ecosystem has already solved differently. ADR-0048
already declined a Postgres-only `burrow_run_sql` for the same reason.

## Consequences

- **Auto-deploy becomes usable by an app with a database**, which is most of them.
- **An unattended deploy stops being silent.** `post-deploy` is the first mechanism by which a push at
  3am can tell anyone it went wrong, and it is fed a reason rather than a boolean.
- **Three phases is modest surface**, mitigated further by their being one command with a phase
  argument rather than three commands. An earlier draft carried `pre-run` and `post-run` as well; they
  were dropped because this record's premise is *there is no caller*, and `burrow app run` is
  synchronous and always has one — the caller already sees the exit code. Symmetry is not a
  justification when the argument for the feature does not apply.
- **Expand/contract is still not expressible unattended.** A pre-deploy hook fires once per deploy, so
  a migration needing run → deploy → run still requires the sequenced path. Hooks cover the common case
  — additive, backward-compatible migrations — and not the hard one.
- **With no `pre-rollback` set, a rollback returns the code and leaves the schema ahead of it.** That
  is inherent to schema changes rather than introduced here, but it becomes reachable automatically
  now, so the rollback path should say what it did and did not do rather than reporting a bare success.
- **A `pre-rollback` that is set and wrong is worse than none.** It runs a schema change during an
  incident, from an image being abandoned, against a database whose state is already the reason
  someone is rolling back.
- **The blast radius of a bad `pre-deploy` is every deploy of that app.** It is configuration set once
  and forgotten, and a command that starts failing blocks deploys until someone notices. That is the
  correct failure direction, and it is a new way for an app to become undeployable.
- **`app.run`'s guardrail does not gate hooks.** They run as part of a deploy, gated by `app.deploy`.
  Whether they deserve their own disposition is a real question this record does not settle.
- **A cloud tenant's hooks run their code**, so they inherit every isolation requirement the app itself
  has — the same sandbox and node placement as any other tenant workload.
- **"Succeeded" is weaker than users will read it as**, per §7, until user apps can carry a readiness
  probe.

## Rejected alternatives

- **Call it a release command**, as Heroku does. Familiar, and one word instead of a phase vocabulary.
  Rejected in §1: "release" already means the artifact that records what shipped, and reusing it tells
  the reader nothing about when the command runs — the only thing they need to know, and the thing a
  phase name answers for free.
- **Do nothing; tell users to run `burrow run` before deploying.** Correct for anyone driving a deploy
  by hand. Rejected because it is not available on the path that needs it: auto-deploy exists precisely
  so nobody has to be there, and "be there" is not an answer.
- **Replace `burrow run` with a hook** — the Heroku shape, one mechanism instead of two. Rejected in
  ADR-0048 and still rejected: expand/contract needs sequencing, and a hook cannot stop between steps.
- **Pre-deploy only, no post phase.** Smaller, and it covers the migration case that motivated this.
  Rejected because it leaves the unattended path as silent as it is today for everything after the
  deploy starts — the failure a push at 3am actually produces is a crashloop, not a failed migration.
- **`pre-run` and `post-run` hooks**, for symmetry with the deploy phases. Rejected because the whole
  argument for hooks is that auto-deploy has no caller, and `burrow app run` is a synchronous call that
  always does — the caller sees the exit code and can sequence whatever comes next, which is precisely
  what ADR-0048 says the explicit call is for. A hook there would duplicate the caller.
- **A post hook that runs only on success.** Simpler, and it matches how many CI "on success" steps
  work. Rejected in §4 as self-defeating: the case a post hook is most needed for is the one it would
  not fire in.
- **Roll back automatically when a deploy fails**, rather than telling a hook. What a user might
  expect. Rejected in §6: the remedy is a judgement about blast radius and data, and an automatic
  rollback after a `pre-deploy` migration has run can leave the schema ahead of the code it restored.
- **An init container on the app's Deployment.** Kubernetes-native, no new concept. Rejected because it
  runs per pod: two replicas race, and every restart re-runs it. The entrypoint problem with more YAML.
- **Run the migration after the deploy**, so a failure does not block shipping. Rejected because the
  window between the new code starting and the migration finishing is exactly when the app queries a
  schema that does not exist — turning a blocked deploy into a live outage.
- **Have Burrow detect and run migrations** by recognising common tools. Rejected per §10.
- **A single command for both deploy and rollback**, with the tool told which direction to go. Fewer
  concepts. Rejected because the two run from **different images** and must default oppositely, so
  collapsing them makes the safe default impossible to express.
- **Add a readiness probe in this record** so "succeeded" means healthy. §7 admits it is the real gap.
  Rejected as a different decision with its own failure mode — a badly-configured probe turns a working
  deploy into a failed one — and folding it in would bury a change to what every deploy waits for
  inside a record about hooks.
