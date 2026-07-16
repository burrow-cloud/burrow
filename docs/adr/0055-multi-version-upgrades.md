# ADR-0055: Multi-version forward upgrades in one step, guarded by a version floor

## Status

🟡 Proposed

## TL;DR

A database may be brought forward across any number of minor versions in a single upgrade:
on startup the control plane applies **all** pending migrations in order, exactly as it
already does, and the startup gate refuses only three things — a downgrade, a cross-major
in-place move, and (once squashing begins) an upgrade that starts below a defined version
floor. The single-minor-step rule is dropped because a linear chain of self-contained SQL
migrations makes stepping and jumping the same tested surface. The same frequent-release
cadence that motivates this also makes the CLI/control-plane compatibility band too tight,
so that band is widened here in the same breath.

Supersedes the single-minor-step **upgrade policy** of
[ADR-0013](0013-database-migrations-and-upgrade-policy.md) and the one-minor **compatibility
band** of [ADR-0039](0039-cli-control-plane-version-skew.md); preserves ADR-0013's migration
*mechanism* (embedded goose, pgx-via-`database/sql`, advisory-lock serialization,
forward-only, the `burrow_meta` version stamp) unchanged. Builds on
[ADR-0012](0012-in-cluster-postgres.md); interacts with
[ADR-0052](0052-pull-based-passive-deploy.md) (frequent releases are the norm).

## Context

[ADR-0013](0013-database-migrations-and-upgrade-policy.md) decided that Burrow supports
upgrading **one minor version at a time** (`vN.M → vN.(M+1)`), enforced by a gate that
refuses any larger forward jump. The gate lives in
`controlplane/postgres/upgrade.go` (`checkUpgrade`) and runs on startup before any migration
is applied (`Migrate` in `controlplane/postgres/migrate.go`): it compares the running
binary's version to the version recorded in `burrow_meta` and refuses a downgrade, a
cross-major move, or a skip of more than one minor, each with an actionable error.

ADR-0013 gave two reasons for the single-minor-step rule: (a) it **bounds what we test** —
"avoid testing and preserving every N→M path"; and (b) it keeps a future **migration
squash** into a baseline safe — squashing 0.x migrations at a major "without stranding old
databases." Both deserve to be met head-on, because the rule has a real cost that has grown
as the project has: a user two or three minors behind must install and run each intermediate
release in sequence just to catch up, and a fresh cadence of frequent small releases
([ADR-0052](0052-pull-based-passive-deploy.md)) turns that into a long, manual ladder for
exactly the solo developer Burrow is built for.

Reason (a) does not actually hold for the migrations Burrow ships. The migrations are a
**linear chain**, not a set of independent N→M paths. goose applies them in numeric order —
`N → N+1 → … → M` — whether they run in one `provider.Up()` call or across several separate
upgrades. The tested surface is *each migration applied on top of its immediate predecessor*,
and that surface is **identical** whether a database climbs one step at a time or jumps the
whole way at once. The only thing single-stepping adds is running the intermediate *binary*
between migrations, which matters in exactly one case: a migration that depends on
application code running out of band — a Go migration, or a backfill the binary performs in
its own code — at that intermediate version. An audit of
`controlplane/postgres/migrations/*.sql` finds none: all fifteen migrations are pure,
forward-only SQL with no Go migrations (`goose.AddMigration` appears nowhere in the tree),
and the three that transform data (`00009` renames guardrail codes, `00014` and `00015`
backfill new columns with `NOT NULL DEFAULT`) do that work **in-migration SQL**,
self-contained, needing no binary between them. So no migration depends on intermediate
application state, and jumping equals stepping.

Reason (b) is real but does not require single-step. Squashing needs a **version floor** at
each squash boundary — a database below the baseline must first reach the baseline before it
can jump past it — because the squashed baseline replaces the individual pre-baseline
migrations a very old database would otherwise need. A floor plus multi-step jumps preserves
squashing exactly: refuse a jump only when it would skip *over* a baseline, not for every gap.

Separately, [ADR-0039](0039-cli-control-plane-version-skew.md) tied its client/server
compatibility band directly to this rule: burrowd of minor `N` serves clients `N` and `N-1`,
"bounded deliberately to one minor **because burrowd itself only ever moves one minor at a
time**." That coupling is the mechanism, so relaxing the upgrade rule reopens the band by the
same logic. The one-minor band is also simply too tight for the release cadence: it is why an
unversioned or slightly-old CLI could be refused a plain operation like a rollback purely on a
version difference. The two decisions move together, so they are decided together here.

## Decision

### 1. Apply all pending migrations in one step

On startup the control plane applies **every** pending migration in numeric order, under the
existing Postgres advisory session lock, and stamps `burrow_meta` with the running binary's
version. This is goose's normal `provider.Up()` behavior and is already what
`runMigrations` does; nothing about the apply path changes. A database several minors behind
is brought fully current in a single `Migrate` call — `v0.8 → v0.13` runs migrations
`00010…00013` in order, once, with no intermediate binary required.

### 2. The gate refuses only three things

The startup gate (`checkUpgrade`) refuses an upgrade in exactly these cases, each with an
actionable error, and allows everything else:

1. **Downgrade** — the binary's minor is below the database's within the same major. The
   schema only moves forward ([ADR-0013](0013-database-migrations-and-upgrade-policy.md)),
   so an older binary must not run against a newer database.
2. **Cross-major in-place upgrade** — the binary's major differs from the database's. A major
   boundary may carry a discontinuity a plain forward apply cannot honor, so it stays an
   explicit, separately-designed step, not an in-place jump.
3. **Below a defined version floor** — once a squash baseline exists (see §4), a database
   recorded below the nearest baseline is refused with an instruction to first reach that
   baseline, because the migrations that would carry it there have been squashed away.

A **forward jump across multiple minors** within the same major and at or above the current
floor — the case ADR-0013 refused — is now **allowed**. A re-run of the same version remains
a no-op stamp.

### 3. Migrations must be self-contained (a new invariant)

The multi-step guarantee rests on one property, which is now a **standing invariant** the
project commits to: **every migration is self-contained.** Any data transformation a schema
change needs happens *inside the migration's own SQL* (a backfill, a rename, a computed
default), and no migration may depend on application code running between it and the next
migration. This is what makes "run the whole chain at once" identical to "run it a step at a
time." Future migrations must honor it; a change that genuinely cannot be expressed as
self-contained SQL — one that needs a program to run mid-chain — is not an ordinary migration
and must be designed as an explicit, separately-gated step (a floor boundary, like a major),
never slipped into the linear chain.

### 4. A version floor preserves squashing

Squashing is retained as ADR-0013 intended, expressed as a **version floor** rather than as
single-stepping. When 0.x (or any range of) migrations are collapsed into a baseline at a
squash boundary, that boundary becomes a floor: the gate refuses an upgrade whose *recorded*
version is below the floor, directing the operator to first upgrade to the floor release
(whose binary still carries the pre-squash migrations) and then jump forward from there.
Between floors, multi-minor jumps are free. Before the first squash there is no floor and the
whole major is reachable in one jump. The floor is thus the single knob that bounds how far
back the migration chain must reach — set by a deliberate squash, not by the upgrade gap.

### 5. Widen the client/control-plane compatibility band (revises ADR-0039)

Because the one-minor compatibility band of
[ADR-0039](0039-cli-control-plane-version-skew.md) was justified *by* the single-minor-step
rule, it is widened here to match. The control plane remains the compatibility anchor and
every other guarantee of ADR-0039 stands (burrowd defines the contract; it never hard-blocks
on version difference alone; a client-version header turns genuine incompatibility into a
structured, actionable error; the acting client version is audited). Only the width changes:

> burrowd serves any client of the **same major** that is **no newer than the server** and
> **at or above the current version floor** (§4) — before the first squash, the whole major.

A client older than the server, anywhere down to the floor, is served (the common case: an
operator upgrades burrowd and a teammate's CLI keeps working until they choose to upgrade). A
client **newer** than the server is still served for shared operations and gated per-feature
with an actionable "ask an operator to run `burrow upgrade`" error, exactly as ADR-0039
specified. A client **below the floor** — or of a different major — gets the "your `burrow`
CLI is too old for this control plane; run `brew upgrade burrow`" error. The additive-within-a-
major API discipline of ADR-0039 §2 continues to carry this: within a major the API only
adds; a breaking change is deprecated across a minor, and the reset points are the major and
the floor, not every minor. The band's ceiling is still "not newer than the server"; its
floor is now the version floor instead of one-minor-back.

### 6. Cadence guidance (enabled by, not load-bearing for, the decision)

Because the engine now absorbs multi-version jumps safely, **version cadence is decoupled
from upgrade safety.** As guidance — not as a safety mechanism — minors should be the
releases that carry migrations and breaking changes, while routine feature work that touches
neither can ship as patches. This keeps the version number informative without making it
load-bearing. The safety comes entirely from the migration chain and the gate (§1–§4): the
version number is a signal, never the thing that decides whether a database is migrated
correctly.

## Consequences

- A database many minors behind is caught up in one upgrade, with no ladder of intermediate
  binaries. This is the direct friction removal for the solo developer who upgrades
  infrequently.
- The gate's job shrinks to three refusals (downgrade, cross-major, below-floor); the
  "install `vN.M+1` first" refusal for an ordinary forward gap is gone.
- The project takes on the **self-contained-migration invariant** (§3) as a permanent
  constraint on how the schema evolves. It is a modest, already-observed discipline — all
  fifteen shipped migrations meet it — but it is now load-bearing and must be honored, and
  its cost is that a transformation needing out-of-band code becomes an explicit floor
  boundary rather than a quiet migration.
- Squashing is preserved and made precise: a squash sets a floor, and only jumps that would
  skip over a floor are refused. The migration chain never has to reach back past the newest
  floor.
- The compatibility surface the control plane must keep working grows from one minor to a
  whole major (down to the floor). That is a real cost — the API must stay additive over a
  wider window — but it is the same cost the additive-within-a-major discipline already
  implies, and it is what stops a server upgrade from stranding a teammate's older CLI.
- Version numbers become cadence guidance rather than an upgrade-safety mechanism, which
  keeps releases free to be frequent and small without each one widening a manual upgrade
  ladder.

## Rejected alternatives

- **Keep single-minor-step (the ADR-0013 status quo).** Its stated benefit — bounding the
  tested surface — does not exist for a linear chain of self-contained SQL migrations, where
  the tested surface (each migration on its predecessor) is identical whether stepped or
  jumped. Its real benefit — safe squashing — is preserved here by a version floor without the
  cost of a per-minor upgrade ladder. So the rule imposes friction for a guarantee it is not
  the only way to provide.
- **Drop the gate entirely and always apply whatever is pending.** Rejected: downgrades,
  cross-major moves, and below-a-floor jumps are genuinely unsafe (an older binary against a
  newer schema; a discontinuity at a major; a chain whose early links were squashed away). The
  gate must still refuse those three; only the ordinary-forward-gap refusal is removed.
- **Abandon squashing so no floor is ever needed.** Rejected: without squashing the migration
  chain grows without bound and every fresh install replays the entire history of the project.
  A floor is a cheap way to keep both squashing and multi-step jumps.
- **Allow multi-step jumps but leave the ADR-0039 band at one minor.** Incoherent: the
  one-minor band was justified *by* single-minor-step, so relaxing one while freezing the other
  leaves the compatibility contract contradicting the upgrade contract. They move together.
- **Relabel feature releases as patches to sidestep the guard rather than widen the policy.**
  Rejected, and pointedly: this makes the *version number* a discipline-policed safety
  mechanism. If safety depended on features shipping as patches to stay "in window," a
  migration accidentally shipped in a patch would be silently mis-handled, and a human labeling
  mistake would become a data-correctness bug. Safety must live in the migration chain and the
  gate, not in release-numbering discipline; the cadence guidance in §6 is a convention, never
  a guardrail.
