-- +goose Up
-- volume records WHICH PersistentVolumeClaim a dump was written to (ADR-0067 §1).
--
-- Backups are now one claim per environment, the same shape as the instance the dump was taken
-- from: `burrow-postgres-backups` for the default environment, `burrow-postgres-<env>.backups` for
-- every other one. Before that there was a single shared claim, so `environment` said where a dump
-- CAME FROM while nothing said where its bytes are — and a restore that derived the claim from the
-- environment would look for a pre-existing dump on a claim that had just been created empty.
--
-- The NOT NULL DEFAULT backfills every existing row to the one claim that has ever held a dump.
-- That is the honest backfill, and it is exact rather than a best guess: no other backup claim
-- existed to write to. Nothing moves on disk — the default environment's claim keeps its name — so
-- every recorded backup keeps resolving to the file it always did.
--
-- It also makes the one case this cannot repair legible instead of silent. A backup taken in a
-- NON-default environment before this change is on the shared claim, which its environment no
-- longer mounts; the row now says so, and the restore refuses naming the claim the dump is on,
-- rather than running a Job that reports a missing file. Its bytes are still there and still
-- readable by an operator; what Burrow will not do is mount one environment's dumps into another
-- environment's Job to reach them.
ALTER TABLE postgres_backups ADD COLUMN volume TEXT NOT NULL DEFAULT 'burrow-postgres-backups';

-- +goose Down
ALTER TABLE postgres_backups DROP COLUMN volume;
