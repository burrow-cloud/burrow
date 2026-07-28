# ADR-0066: The Postgres add-on runs on CloudNativePG; Burrow creates custom resources, not backups

## Status

✅ Accepted

## TL;DR

The Postgres add-on is a **single-replica Deployment** with one disk, and backups are `pg_dump` files
written to a second disk **in the same cluster**. That means: any node drain, kubelet restart or
add-on upgrade is full downtime; backups happen only when somebody runs the command; nothing prunes
them, so the disk fills and then new backups fail too; and the backup shares a failure domain with
the thing it is backing up.

This replaces the mechanism with **CloudNativePG** — a CNCF Postgres operator — while keeping
[ADR-0031](0031-postgres-addon.md)'s user-facing contract intact: an instance shared by the apps in
an environment, with a database and a login role per app. ([ADR-0067](0067-one-database-instance-per-environment.md)
scopes that instance to one per environment; this record is about the *mechanism*, not the count.)

The gain is not incremental. Continuous write-ahead-log archiving to object storage, scheduled
backups, retention, point-in-time recovery and replicas are things the operator already does, and
each is a thing this project would otherwise write and be wrong about. **The most valuable change is
what leaves the code: Burrow stops orchestrating backups.** It creates a Kubernetes object and reads
its status; the operator does the work. No exec, no `pg_dump`, no credentials handled on a backup
path.

**On licensing, because it decided the shape.** CloudNativePG is Apache-2.0. The commonly used
backup plugin shells out to **barman**, which is **GPL-3.0** — so this record does not use it. It
uses a **pgBackRest**-based plugin instead: Apache-2.0 plugin, **MIT** engine, no copyleft anywhere
in the path.

Supersedes [ADR-0031](0031-postgres-addon.md)'s **implementation mechanism** — the Deployment, the
image, and the teardown semantics [ADR-0064](0064-addon-removal-keeps-its-data.md) already revised.
ADR-0031's decisions about *what a user gets* stand. Depends on
[ADR-0063](0063-object-storage-provider.md) for the destination.

## Context

ADR-0031 chose a plain `postgres:17-alpine` Deployment: one replica, `Recreate` strategy (a
ReadWriteOnce volume cannot be held by two pods, so a rolling update would deadlock), a PVC, a
generated superuser password, and a `postgres_exporter` sidecar. It was the right call — an operator
is a heavy dependency to impose on a small self-hoster, and the provisioning model is what users
actually interact with.

It has ten gaps, and they are not independent. Written down together:

1. Backups land on a PVC **in the same cluster** as the database.
2. There is no object-storage destination.
3. There is no schedule; a backup happens when someone types the command.
4. There is no retention; dumps accumulate until the disk fills, which then breaks new backups too.
5. Nothing verifies a restore.
6. One replica plus `Recreate` means every drain, restart and upgrade is downtime.
7. No point-in-time recovery: the floor on data loss is the interval between manual dumps.
8. No connection pooling.
9. No major-version upgrade path — `initdb` refuses a data directory with a different catalog version.
10. Metrics are exported; nothing alerts on backup age or disk-full.

[ADR-0063](0063-object-storage-provider.md) supplies a destination, which addresses (2). Closing
(1), (3), (4) and (7) by hand means implementing WAL archiving, scheduling, retention and
point-in-time recovery — which is to say, reimplementing a backup tool.

### Why ADR-0031 rejected an operator, and why that reasoning does not hold

ADR-0031's rejected alternatives include:

> **An AGPL or proprietary Postgres distribution / operator (e.g. a bundled CloudNativePG with
> copyleft tooling).** Out of scope for *install* under the ADR-0025 license bar.

The premise is mistaken in one place and right in another. **CloudNativePG itself is Apache-2.0**, a
CNCF project — it clears [ADR-0025](0025-building-block-addons.md)'s bar outright. What is copyleft
is **barman** (GPL-3.0), the Python tooling the *most commonly used* backup plugin invokes.

That distinction matters because the backup path is now separable from the operator. CNPG's plugin
interface (CNPG-I) makes backup a pluggable concern, and permissively licensed plugins exist that do
not involve barman at all.

### The licensing standard, stated so it is falsifiable

"No GPL" is not implementable as written: every Debian-derived container image carries glibc (LGPL)
and libgcc (GPL-with-linking-exception), and `postgres:17-alpine` is neither Apache nor MIT. A rule
that forbids GPL *bytes* forbids essentially every image.

The rule that is implementable, and is the one this project holds:

- **No copyleft code compiled into, or linked by, Burrow.** Burrow's own binaries stay Apache-2.0
  with a dependency graph to match.
- **No component whose license would reach Burrow's code or a user's code.** Invoking a separate
  program is aggregation, not derivation; linking is different.
- **Prefer permissive engines where a real choice exists** — and here one does, which is why barman
  is declined rather than accepted with a footnote.

### What CNPG does not do, which is the part that shapes the OSS work

Verified against CNPG's API types and documentation, not assumed:

- **`Backup.status` carries no size field of any kind.** Timing, method, WAL range, destination and
  outcome are all there; bytes are not. "How big are my backups" is answerable only by joining to
  `VolumeSnapshot.status.restoreSize`, or by asking the object store.
- **Retention is deprecated and object-store-only.** The comparison table marks retention
  unsupported for volume snapshots, and the default snapshot deletion policy leaves snapshots behind
  after the `Backup` and even the `Cluster` are gone. **Snapshot retention is the integrator's
  problem.**
- **Recovery is never in-place, and it is never per-app.** It bootstraps a *new* `Cluster` from a
  backup — the whole instance, every app's database in that environment, to one point in time. Today's
  `burrow addon restore <addon> <app> --backup <id>` restores **one app's database** from a logical
  dump, and physical recovery cannot express that. This is the sharpest mismatch between the two
  models and §4 addresses it directly.
- **The backup status fields and metrics were deprecated in 1.26.1** and, under plugin-based
  backups, report **stale values rather than absent ones** — so an alert built on them fails silently.

## Decision

### 1. The Postgres add-on is a CloudNativePG `Cluster`

`addon install postgres` installs the CNPG operator if it is absent and creates a `Cluster`
**per environment**, per [ADR-0067](0067-one-database-instance-per-environment.md) §1.

The tenant-facing contract of ADR-0031 is otherwise unchanged: within an environment the instance is
shared, `addon attach` gives an app its own database and login role, and the app receives a
`DATABASE_URL` in its own Secret. What ADR-0067 changed is the *scope* of "shared" — one instance per
environment rather than one per cluster.

Replica count, storage and resources become configuration rather than constants, and a single-replica
instance stays the default for the small self-hoster ADR-0031 was written for.

**CNPG makes ADR-0067's cost materially lower**, which is worth stating because the two records were
written days apart and the interaction is easy to miss. ADR-0067 accepted a pod and a volume per
environment as the price of server-level isolation. Under an operator that price buys more than it
did: each environment's instance is independently upgradable — which is the rehearsal ADR-0067 §Context
says a shared instance cannot provide — independently backed up on its own schedule, and
independently sized. A hand-rolled Deployment per environment would have given isolation and little
else.

### 2. Burrow creates custom resources and reads status; it never runs a backup tool

A backup is a `Backup` object. A schedule is a `ScheduledBackup`. A restore is a `Cluster` with a
recovery bootstrap. Burrow creates them and watches `.status`.

This deletes code rather than adding it: the Job construction, the `pg_dump` argv, the Job-watching,
and the handling of a superuser credential on the backup path all leave the control plane. It is
also the strongest argument for the whole change — **backup correctness stops being Burrow's to get
wrong.**

### 3. Backups are pgBackRest via a CNPG-I plugin, not barman

The base-backup and WAL path uses a **pgBackRest**-based CNPG-I plugin: the plugin is Apache-2.0 and
pgBackRest is **MIT**. The barman-cloud plugin — the better-known option — is declined solely because
it shells out to GPL-3.0 tooling, per the standard above.

**Writing our own plugin is rejected.** The interface is small enough to tempt it — a WAL-archiving
plugin needs six RPCs, three of them trivial — but `archive` carries PostgreSQL's `archive_command`
contract transitively: returning success means PostgreSQL may recycle that segment, so success must
mean durably stored, and the call must be idempotent because failure retries forever. Get it subtly
wrong and the loss is silent until a restore. Retention is worse: nothing tells a plugin which WAL
segments the oldest base backup still needs, so a bucket lifecycle rule that outruns backup retention
destroys recoverability — the exact failure [ADR-0063](0063-object-storage-provider.md) §3 exists to
prevent, reintroduced from the other side.

The plugins available are **experimental**, and none is known to have been run against the object
storage this project intends to use. A backup engine is trusted when it has been exercised, not when
it is selected: adoption includes a restore, from the real destination, before it is offered to
users.

### 4. Burrow owns what CNPG leaves to the integrator

Three things, each because CNPG genuinely does not do them:

- **Backup size.** `Backup.status` has none, so Burrow joins the associated `VolumeSnapshot` (or the
  object store) and reports size in its own listing. A backup list without sizes is not a useful
  answer to "what have I got."
- **Snapshot retention.** CNPG's retention does not cover volume snapshots and its default deletion
  policy retains them indefinitely. Burrow prunes them, against the same retention window it
  reconciles bucket lifecycle against (ADR-0063 §3), so the two cannot disagree.
- **Restore orchestration.** Since recovery bootstraps a *new* `Cluster`, a physical restore creates
  it, waits for it, cuts `DATABASE_URL` over, and decides the fate of the old one. That sequence is
  the feature; the CR is only its first step.

**And the logical per-app path stays.** `burrow addon restore <addon> <app> --backup <id>` restores
**one app's database** and must keep doing so. Physical recovery cannot: it rewinds the whole
instance, so restoring one app would roll back every other app sharing it — a cross-app data loss
triggered by a single-app operation, which is exactly the class of failure
[ADR-0064](0064-addon-removal-keeps-its-data.md) exists to prevent.

So the two coexist deliberately, and they answer different questions:

| | Granularity | Recovers to | Use |
| --- | --- | --- | --- |
| Logical dump (`pg_dump`) | one app's database | the moment of the dump | "this app's data is wrong" |
| Physical (CNPG) | the whole instance | any point in the WAL window | "the instance is gone" |

This is not a transitional compromise. An instance shared by an environment's apps, with a database
per app (ADR-0031, scoped by ADR-0067), has both
failure modes, and neither backup kind covers the other's. Retiring the logical path would trade a
scheduling and retention problem for a granularity problem, which is a worse trade than it looks.

### 5. A backup-age health signal, owned by Burrow

Burrow reports the age of the last **successful** backup from what it observed, not from CNPG's
deprecated status fields — which report stale values rather than absent ones under plugin backups,
and would therefore alert on nothing while looking healthy.

This is [ADR-0063](0063-object-storage-provider.md) §7's status surface, and the two must be the same
surface: "when did this last work" is one question whether the answer is the store's or the
database's.

### 6. Migration is explicit and one-way

An existing add-on installed under ADR-0031 is not silently converted. The path is: back up, install
the new add-on, restore, cut over — a documented sequence a user runs deliberately, with the ADR-0031
add-on left in place until they are satisfied.

An automatic in-place conversion of the component holding every app's data is the wrong place to be
clever.

## Consequences

- **Seven of ten gaps close by adoption rather than by code**: off-cluster backups, scheduling,
  retention for object-store backups, PITR, replicas, pooling and a major-version upgrade path.
- **Burrow depends on an operator, and on cluster-scoped CRDs.** `addon install postgres` stops being
  a namespaced operation — installing CRDs needs cluster-admin, which the agent does not have and
  must not. Add-on installation becomes an operator-CLI setup step in a way it was not before, and
  that is a real reduction in what an agent can provision unaided
  ([ADR-0034](0034-agent-native-onboarding.md)'s demand-driven model is narrowed here).
- **The small self-hoster pays for capability they may not need.** An operator plus CRDs is heavier
  than a Deployment, and ADR-0031 chose the Deployment deliberately for that reader. Defaults keep it
  to one replica, but the floor rises.
- **A second backup engine's failure modes become ours to learn.** pgBackRest is mature; the CNPG-I
  plugins wrapping it are not, and "experimental" is the maintainers' word, not a hedge added here.
- **There are now two restore paths with different granularities**, and a user has to understand
  which one they want. `addon restore <addon> <app>` keeps its current meaning; physical recovery is
  a separate, larger operation that stands up a replacement instance and cuts over. Two mechanisms
  is a documentation burden and an opportunity to reach for the wrong one under pressure — accepted
  because neither covers the other's failure, but it is a real cost and the naming has to earn its
  keep.
- **The licensing standard is now written down**, which cuts both ways: it settles this argument and
  it commits the project to declining otherwise-good tooling on the same grounds in future.

## Rejected alternatives

- **Keep the Deployment and build the missing pieces.** No new dependency, no CRDs, no operator, and
  the small self-hoster keeps the light footprint ADR-0031 chose. Rejected because "the missing
  pieces" are WAL archiving, scheduling, retention and PITR — a backup tool — and the failure mode of
  getting them subtly wrong is data that cannot be restored, discovered during an incident.
- **CloudNativePG with the barman-cloud plugin.** The best-supported path, the one CNPG's own tests
  exercise most, and rejected only on licence: it invokes GPL-3.0 tooling. Running it would not
  infect Burrow's code — but a permissive alternative exists, and preferring it costs little.
- **CloudNativePG with the WAL-G plugin.** Apache-2.0 plugin over an Apache-2.0 engine, so it appears
  to qualify — but its published image is built with lzo support, which is GPL-3.0+. A plugin's
  licence is not its image's licence, and this is the example that proves the standard needs to name
  images and not just repositories.
- **Write a WAL-archiving plugin.** Attractive because the interface is small and it would remove
  every third-party question. Rejected in §3: the `archive` contract is unforgiving, retention has no
  signal to work from, and a bug surfaces at restore time.
- **A dedicated Postgres per app.** ADR-0031 rejected this on density and that reasoning is
  unchanged; CNPG makes it cheaper to express but no cheaper to run.
- **Managed Postgres from a cloud provider.** Correct for many users and orthogonal to this record —
  Burrow can already be pointed at an external database. It is not an add-on decision.
