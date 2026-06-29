# ADR-0032: Postgres backups — logical dumps via Jobs, recorded in the registry

## Status

Accepted. Extends the Postgres add-on ([ADR-0031](0031-postgres-addon.md)) with backup, restore,
and listing. The first slice is **on-demand** backup/restore/list; **scheduled** backups and
retention are a named follow-on slice in this same design (see Consequences).

## Context

[ADR-0031](0031-postgres-addon.md) provisions a per-app database on a shared instance, but a
database an operator cannot back up is one they cannot trust with real data — it is the first thing
they will ask for. Burrow should make a backup a one-ask operation the same way it made a database
one: the agent (or the human/UI) says "back up `shop`," and Burrow captures a restorable copy on the
user's own cluster.

Four forces:

1. **Dumps can be large and slow.** Streaming a `pg_dump` through burrowd would tie up the control
   plane and put a Postgres client binary in its image. A backup is naturally a short-lived
   **Kubernetes Job** running the `postgres` image (which already carries `pg_dump`/`pg_restore`),
   in the add-on namespace, next to the instance.
2. **A backup needs somewhere durable to live.** Object storage is the eventual home, but the
   object-store add-on does not exist yet ([ROADMAP](../ROADMAP.md) sequences it after DB + auth).
   A dedicated **backup PVC** in the add-on namespace is the available durable target now, and does
   not foreclose an `--to s3://…` target later.
3. **"What backups do I have?" must be answerable without shelling into a volume.** burrowd is not
   mounted to the backup PVC, so the **list comes from the control-plane database** — burrowd
   records each backup's metadata when its Job succeeds. This matches Burrow's rule that the
   registry of control-plane state lives in Postgres, not by scraping the cluster.
4. **Restore is destructive.** Restoring overwrites an app's live database; it must sit behind a
   confirm guardrail, like `app delete` and `addon detach`.

## Decision

**Back up and restore a per-app database with a Kubernetes Job running `pg_dump`/`pg_restore`
against the add-on instance; store the dump on a backup PVC; record each backup in the control-plane
database; list and restore from that record.**

- **`burrow addon backup postgres <app>`** — burrowd creates a Job (the `postgres` image) in the
  add-on namespace that runs `pg_dump` of the app's database (custom format, `-Fc`) and writes it to
  `/<backup-pvc>/<app>/<backup-id>.dump`. The Job connects as `burrow_admin`, reading the superuser
  password from the `burrow-postgres` Secret via `secretKeyRef` (env), exactly as the provisioner
  does — the password is never logged or passed as an argument. On success burrowd records a row in
  a `postgres_backups` table (goose-migrated): `id`, `app`, `created_at`, `size_bytes`, `path`,
  `status`. This is an MCP-exposable operation — it moves no secret value over MCP.
- **`burrow addon backups postgres [<app>]`** — lists recorded backups (id, app, time, size) from
  the database. Read-only; an MCP tool.
- **`burrow addon restore postgres <app> --backup <id>`** — burrowd creates a Job that runs
  `pg_restore` of the named dump into the app's database (`--clean --if-exists` so it replaces
  current contents). **Behind a confirm guardrail** (`GuardrailAddonRestore`, default confirm): it
  overwrites live data. Records the restore in the audit log ([ADR-0027](0027-audit-log.md)).
- **The backup PVC** (`burrow-postgres-backups`, ReadWriteOnce) is created in the add-on namespace
  on first backup (or at `addon install postgres`). Backup/restore Jobs mount it; the instance pod
  does not.
- **RBAC:** burrowd's `burrow-addons` Role gains `batch/jobs` (`create`, `get`, `list`, `delete`)
  to run and reap these Jobs — and, for the scheduled slice, `batch/cronjobs`. Still namespace-scoped
  to `burrow-addons`; no `ClusterRole`. The existing secrets grant ([ADR-0031](0031-postgres-addon.md))
  already covers reading the superuser Secret the Jobs need.

### Security

A dump contains application data, so the backup PVC is part of the data tier and inherits its
boundary: it lives only in `burrow-addons`, never in the app or control-plane namespaces, and is
never exposed over an API. No **secret value** crosses MCP, the API response, the audit log, or the
control-plane database: the Jobs receive the superuser password only via `secretKeyRef` env from the
existing Secret, and the recorded backup metadata names the app, the size, and the on-PVC path —
never a credential or a connection string.

## Consequences

- **A trustworthy database.** Backups and point-in-time-enough recovery on the user's own cluster,
  one ask, behind the same guardrails as everything else.
- **List without scraping volumes.** The control-plane database is the index of backups; the PVC
  holds only the dump bytes.
- **Restore is gated.** Overwriting live data requires confirmation and is audited.
- **New surface:** the `postgres_backups` table + migration; the backup/restore Job builders behind
  the kube seam (faked in unit tests); `addon backup` / `backups` / `restore` commands and the
  read/backup MCP tools; the `burrow-addons` `batch` grant; and a deterministic k3d e2e (install →
  attach → write a row → backup → drop the row → restore → assert the row is back).
- **Follow-on slice — scheduled backups + retention:** a burrowd-created **CronJob** (daily default,
  `--schedule` configurable) that backs up every attached app database, plus retention (`keep last
  N`, default 7) that prunes old dumps and their rows. Deferred to a second PR to keep the first one
  focused; this ADR records the whole design so the follow-on adds no new decision.
- **Later, not now:** an `--to s3://…` object-storage target (once the object-store add-on or a
  BYO-S3 bridge exists), physical/WAL backups for true PITR, and cross-cluster restore. Logical
  dumps cover the common case first.

## Rejected alternatives

- **Stream `pg_dump` through burrowd in-process.** Rejected: ties up the control plane on a
  potentially long, large transfer and forces a Postgres client into the burrowd image. A Job using
  the `postgres` image is the natural unit and keeps burrowd lean.
- **Back up the whole instance with `pg_dumpall` only.** Rejected as the primary granularity:
  per-app `pg_dump` matches the database-per-app isolation model ([ADR-0031](0031-postgres-addon.md))
  and lets a single app be restored without touching its neighbors. (A whole-instance variant can
  come with the scheduled slice.)
- **List backups by reading the PVC.** Rejected: burrowd is not mounted to the backup PVC, and
  scraping a volume for state contradicts keeping the control-plane registry in Postgres. Recording
  metadata on Job success is the versioned contract.
- **Object storage as the v1 target.** Rejected for now only because the object-store add-on is not
  built; the PVC is the available durable target and the design leaves an `--to` seam for S3 later.
- **Make restore a plain allowed operation.** Rejected: it overwrites live data; it belongs behind a
  confirm guardrail and in the audit log, like every other destructive action.
```
