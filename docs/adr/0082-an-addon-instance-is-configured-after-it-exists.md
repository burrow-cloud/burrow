# ADR-0082: An add-on instance is configured after it exists, not at install

## Status

🟡 Proposed

## TL;DR

An add-on's shape is fixed when it is installed and can never change. A Postgres instance created
without a standby stays without one; a cache installed as a single node stays a single node. The only
way to change either is to remove it and install it again, which for a database means restoring from
a backup.

- **`burrow addon config <type> <setting> <value>`** changes an instance that already exists, with
  a subcommand per setting rather than a flag, because each one has consequences worth explaining.
- **Install stays shapeless.** It creates an instance and asks nothing, because the install is the
  one moment nobody can answer these questions.
- **Growing is not shrinking.** Adding capacity is additive; taking it away can break an app using
  what is removed, so shrinking asks and growing does not.
- **It is an operator's call**, because it provisions hardware.

Extends [ADR-0025](0025-building-block-addons.md)'s add-on model with a lifecycle verb it lacked, and
decides what [ADR-0081](0081-a-postgres-instance-may-have-a-standby.md) §1 deliberately left out.
Supersedes nothing.

## Context

Every property that determines an add-on instance's shape is set at install and never again. Postgres
gets its standby count ([ADR-0081](0081-a-postgres-instance-may-have-a-standby.md) §1); the cache
gets a single node; storage size is fixed at creation.

**The install is the worst possible moment to ask.** Somebody installing Postgres for the first time
is standing up a database for an app that does not exist yet, with no traffic and no idea whether it
will ever matter. They will say no to a standby, correctly. The day they need one is the day the app
became important — months later, in production, with data in it.

Today the only route from there to a standby is `addon remove --delete-data` and a fresh install,
followed by a restore. That is an outage, a restore that
[#428](https://github.com/burrow-cloud/burrow/issues/428) says has never been exercised against a real
object store, and a genuine risk of data loss — to change a number the operator underneath supports
changing live.

**The underlying platforms already do this.** CloudNativePG scales a `Cluster`'s instance count up and
down and handles the replication; Valkey supports sentinel and cluster topologies. What is missing is
Burrow's word for asking.

**And it will not stay Postgres-only.** The cache add-on has the same shape of question — a single
node, or sentinel, or a cluster — arriving the same way, when somebody's traffic grew. A verb invented
for Postgres standbys alone would be renamed the first time the cache needed one.

## Decision

### 1. Each setting is a subcommand under its add-on type

```
burrow addon config postgres                    # what can be set, and what it is set to
burrow addon config postgres standbys 1
burrow addon config postgres storage 50Gi
burrow addon config cache topology sentinel
```

**Not flags on one command.** The settings an add-on has are specific to it — Postgres has standbys
and storage, the cache has a topology, a future add-on has something else — and a shared flag set
would either mean different things per type or collapse into a `--set key=value` that validates
nothing.

**A subcommand earns its place because these settings carry consequences that have to be explained
where they are used.** Adding a standby restarts every attached app. Removing the last one withdraws
the read address ([ADR-0081](0081-a-postgres-instance-may-have-a-standby.md) §2). Growing a volume
cannot be undone. A flag gets one line of `--help` and no room to say any of that; a subcommand gets
a paragraph, its own validation, and a refusal written for the specific thing being refused.

**Bare `burrow addon config <type>` lists what is configurable and its current value.** An operator
who does not know what an add-on can be told is one command away from finding out, and the same
output is where a person confirms a change landed.

**Named `config` rather than `scale`, because shape is not only size.** A standby count and a volume
size are quantities; a cache's topology is not, and neither is whatever the next add-on exposes.
`scale` would have been the wrong word the first time something changed that has no bigger and
smaller. It also matches what the CLI already says: `app config` is an app's settings,
`cluster config` is the cluster's, and this is an instance's.

**An instance is identified by type and environment**, as everywhere else —
[ADR-0067](0067-one-database-instance-per-environment.md) puts one per environment, which is what
`AddonInstanceName(type, env)` already encodes. So `--env` scopes it and there is nothing else to
name.

**When an environment can hold several** ([#432](https://github.com/burrow-cloud/burrow/issues/432)),
the instance is selected by a **flag** rather than a positional. With settings as subcommands a
positional would be ambiguous against them — `burrow addon config postgres <name-or-setting?>` — so
the shape of the setting decides the shape of the selector. It is not added now: a flag that could
only ever take one value teaches nothing.

### 2. Growing is not shrinking, and only one of them asks

**Growing is additive and proceeds.** Adding a standby, enlarging a volume, moving a cache from one
node to sentinel — nothing that exists stops working, and the cost is the operator's to accept by
having typed the command.

**Shrinking can break something that is using what is removed**, so it is held for confirmation and
says what it will take away. Removing a Postgres standby removes the endpoint ADR-0081 §2's read
address resolves to, so an app configured to read from it breaks — **not at the moment of the
scale-down, but at its next query down that connection**. The confirmation names the apps affected,
not a count, for the same reason [ADR-0064](0064-addon-removal-keeps-its-data.md) §2 does: a person
about to break something should see what.

**Some shrinks are refusals rather than confirmations.** A volume cannot shrink, because the data
does not fit and the operator will not do it — so Burrow says so at the point of asking rather than
letting a `Cluster` sit in a failed state explaining it in a status field.

### 3. Withdrawing the read address is part of removing the standby

When a scale-down removes the last standby, the read address ADR-0081 §2 wrote is **removed from the
attached apps, and they are restarted** — the exact inverse of the operation that added it.

The alternative is leaving a variable pointing at an endpoint that resolves to nothing, which fails at
the app's next read rather than at the operation that caused it. **A failure at the moment of the
change is one somebody can connect to the change.**

This is why §2 holds the shrink for confirmation even though the platform underneath does it happily.

### 4. It is operator-only, and the agent says what to run

`addon config` is an operator command, absent from `burrow-agent` and reported by `guard`
([ADR-0065](0065-what-belongs-on-the-agent-surface.md) tier 1).

It **provisions hardware**. An agent deploying an image spends nothing; an agent adding a standby or
doubling a volume spends money on infrastructure nobody approved, and the ease of reversing the
change does not make the spend reversible.

The refusal names the verb and that a human runs it, per ADR-0065 §7 — an agent that can say *"this
instance has no standby, and a person can add one with `burrow addon config postgres standbys 1`"*
is doing the useful half of the work.

### 5. It is not a scheduler, and never runs by itself

Nothing scales an add-on automatically. No usage threshold, no autoscaler, no growing a volume
because it filled.

Application autoscaling exists ([ADR-0056](0056-autoscaling.md)) and is a different thing: adding a
replica of a stateless app is cheap and reversible, and adding one to a database is neither. An
add-on changing shape without anybody asking is a cost event and a topology change that nobody
decided, arriving at whatever hour the threshold was crossed.

## Consequences

**The install stops being a decision people have to get right in advance.** Install the simple thing,
change it when the need is real. That is the correct order, and today's model has it backwards.

**A shape change is a real operation with real risk**, and the record should not imply otherwise.
Adding a standby means CloudNativePG cloning a database — I/O against a live primary, for however
long the data takes. It is safe, and it is not free, and doing it during an incident is worse than
having done it beforehand.

**Growing storage is one-way.** A volume that grew cannot shrink, so an operator who over-provisions
is paying for it until the instance is rebuilt. Worth stating in the command's own output rather than
letting somebody find out by trying.

**There is exactly one way to reach a given shape**, because install takes no shape flags. An
instance is created plain and configured afterwards, so there is no second route to keep in step with
the first — a simplification worth having, and the reason `--standbys` was dropped from install
rather than kept beside this.

**The cache gets this verb for free when its topologies land**, which is the point of deciding it
here rather than adding a Postgres-shaped flag to a Postgres-shaped command.

## Rejected alternatives

**Remove and reinstall.** The status quo. It is an outage, it discards data unless a restore works,
and it routes a routine change through the single most dangerous operation Burrow has — the one that
[#428](https://github.com/burrow-cloud/burrow/issues/428) says has never been proven against a real
object store. Nobody should have to gamble a database to add a standby to it.

**Only allow growing.** Removes §2's and §3's hard cases entirely, and leaves an operator who added a
standby they no longer need paying for it forever, with removal only via the reinstall above. The
asymmetry is real and belongs in the confirmation, not in the feature set.

**Keeping a `--standbys` flag on install as well.** Two routes to one shape, which must then be kept
in agreement forever, in exchange for saving one command in the case where somebody already knows.
That case is rare — the install is precisely when they do not — and ADR-0081 §1 declines it for the
sharper reason: a flag asked at a moment nobody can answer is answered wrong by the people it exists
for.

**A generic `--set key=value`.** Fewer commands to maintain, and it gives up every check worth
having: no validation at the point of asking, no help text naming what an add-on can be, and no way
to refuse a volume shrink before it fails.

**Flags on one `config` command** rather than a subcommand per setting — `--standbys 1
--storage 50Gi`. Shorter to type and it flattens settings that are not alike: a flag gets one line of
help, which is not enough to say that adding a standby restarts every attached app or that a volume
cannot shrink back. It also grows one command's flag surface with every add-on that gains a setting,
so `postgres` flags and `cache` flags end up on the same command meaning nothing to each other.

**Making it an [ADR-0068](0068-operational-limits-are-configuration.md) operational limit.** Those are
bounds a human sets — ceilings on what may be asked for. A standby count is a property of one
instance, not a bound on all of them, and modelling it as a limit would put it at cluster or
environment scope and force one answer on every instance in reach.

**Letting the agent scale when a threshold is crossed.** The autoscaling instinct, and it is §5's
refusal for the reason above: a database changing shape unattended is a cost event and a topology
change nobody decided, at an hour nobody chose.
