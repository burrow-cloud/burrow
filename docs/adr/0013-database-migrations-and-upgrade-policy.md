# ADR-0013: Database migrations (embedded goose) and the single-minor-step upgrade policy

## Status

Accepted.

## Context

The control plane's schema will evolve across releases. The first cut applied a
`CREATE TABLE IF NOT EXISTS` const on startup, which cannot evolve a schema (add a
column, change a type, backfill data) and has no concept of an upgrade. We need real
migrations and an explicit answer to "how do users upgrade from one version to the next."

Constraints that shape the choice:

- The control plane ships as a **single self-contained binary** (open core); we do not
  want to require operators to run a separate migration CLI or carry loose SQL files.
- The control plane may run as **multiple replicas**, so two instances can start at once
  and must not race the migration.
- We want a **bounded, testable upgrade path** and the freedom to **squash** old
  migrations into a baseline at a future major without stranding existing databases.

## Decision

### Migrations: embedded goose, applied on startup

Schema changes are **ordered goose SQL migrations embedded in the binary** (`embed.FS`,
`controlplane/postgres/migrations/*.sql`) and applied on startup via goose's programmatic
**provider API** (`goose.NewProvider(...).Up(ctx)`) — no external migration tooling, no
loose files. Concurrent replicas are serialized by a **Postgres advisory session lock**
(`WithSessionLocker`), so only one instance migrates at a time and the others no-op.

goose is used as a library, not the CLI. It was chosen over a hand-rolled migrator
because it provides ordered versioning and the advisory-lock serialization a multi-replica
control plane needs, for a contained dependency.

### Driver: pgx through `database/sql`

The adapter uses the **pgx** driver through the standard **`database/sql`** interface
(`pgx/v5/stdlib`), not `pgxpool` and not `lib/pq`. This gives one `*sql.DB` shared by both
goose and the application, the maintained driver (lib/pq is in maintenance mode and its
authors recommend pgx), and ample performance for a control plane's modest query volume.

### Upgrade policy: one minor version at a time

Burrow supports upgrading **one minor version at a time** within a major
(`vN.M → vN.(M+1)`). Skipping minors, downgrading, and cross-major in-place upgrades are
**unsupported and refused**. This bounds what we test and lets us squash 0.x migrations
into a baseline at v1.0 without stranding old databases.

The policy is **enforced**, not just documented. A single-row `burrow_meta` table records
the Burrow version that last migrated the database. On startup, before applying any
migration, the binary compares its own version to the recorded one and **refuses with an
actionable error** if the move is a skip, a downgrade, or cross-major (e.g. "database is
at v0.1, this binary is v0.3; install v0.2 first"). A fresh database (no `burrow_meta`)
is a first install and is not gated. After a successful migration the binary stamps its
version.

Migrations are **forward-only in production**; down migrations are included for
development convenience only.

## Consequences

- `Store.Migrate(ctx, appVersion)` takes the running binary's version, runs the gate,
  applies pending migrations under the advisory lock, and stamps `burrow_meta`. `burrowd`
  supplies its version (wired in a later phase); tests pass it explicitly.
- The gate logic (`parseMajorMinor`, `checkUpgrade`) is pure and unit-tested without a
  database; the migration apply, stamping, and a refused-jump are integration-tested
  against real Postgres.
- The binary stays self-contained: migrations travel inside it. Adding a schema change is
  a new numbered `.sql` file.
- A future major may squash the 0.x migrations into a baseline; the single-step policy is
  what makes that safe.

## Rejected alternatives

- **`CREATE TABLE IF NOT EXISTS` schema const (the first cut).** Rejected: it cannot
  evolve a schema and encodes no upgrade concept.
- **A hand-rolled migrator (no dependency).** Reasonable and lighter on dependencies, but
  it would reimplement ordered versioning and, more importantly, the advisory-lock
  serialization that a multi-replica control plane needs. goose provides both, tested.
- **lib/pq driver.** Rejected: in maintenance mode; its own maintainers recommend pgx.
- **pgxpool (native pgx).** Rejected here: goose needs a `database/sql` `*sql.DB`, and a
  single `*sql.DB` for both migrations and app queries is simpler than maintaining a pool
  plus a stdlib connection. Native pgx performance is unnecessary at control-plane scale.
- **Allowing multi-version jumps.** Rejected: it would force us to test and preserve every
  N→M path and block migration squashing. One step at a time is the standard, bounded
  guarantee.
